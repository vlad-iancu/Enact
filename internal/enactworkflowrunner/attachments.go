package enactworkflowrunner

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"

	"enact/internal/files"
	"enact/internal/inference"
	"enact/internal/workflows"
)

// attachment is one resolved file: what is sent to the model, and what the
// execution record keeps about it. The two are built together because they
// describe one thing, and a record that disagreed with what was sent would be
// worse than no record.
type attachment struct {
	sent     inference.ContextFile
	recorded workflows.StepAttachment
}

// sent returns what goes to the inference API.
func sent(list []attachment) []inference.ContextFile {
	if len(list) == 0 {
		return nil
	}
	out := make([]inference.ContextFile, len(list))
	for i, a := range list {
		out[i] = a.sent
	}
	return out
}

// recorded returns what goes onto the step's record.
func recorded(list []attachment) []workflows.StepAttachment {
	if len(list) == 0 {
		return nil
	}
	out := make([]workflows.StepAttachment, len(list))
	for i, a := range list {
		out[i] = a.recorded
	}
	return out
}

// attachmentsFor resolves an agent step's declared attachments and reads them
// into the form the inference API takes.
//
// Every failure fails the step. A prompt that says "summarise the attached
// report" with no report attached does not produce an error from the model —
// it produces a confident answer about nothing, which is far worse than a run
// that stops and says which path did not resolve.
func (r *Runner) attachmentsFor(ctx context.Context, step workflows.Step, stepCtx workflows.Context) ([]attachment, error) {
	if len(step.Attach) == 0 {
		return nil, nil
	}
	if r.files == nil {
		return nil, fmt.Errorf("this step attaches files, but no file store is configured on the runner")
	}

	attachments := make([]attachment, 0, len(step.Attach))
	for _, path := range step.Attach {
		file, err := workflows.Attachment(stepCtx, path)
		if err != nil {
			return nil, fmt.Errorf("cannot attach %s: %w", path, err)
		}
		resolved, err := r.readAttachment(ctx, path, file)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, resolved)
	}
	return attachments, nil
}

// readAttachment turns one descriptor into an inference attachment, refusing
// what the model would refuse.
//
// The checks happen here, before any bytes are read, because the file store
// accepts files twenty times larger than a model will take: without them the
// runner would read a hundred megabytes, base64 it into a request, and be
// told no by a service one network hop away.
func (r *Runner) readAttachment(ctx context.Context, path string, file files.File) (attachment, error) {
	name := file.Name
	if name == "" {
		return attachment{}, fmt.Errorf(
			"cannot attach %s: the file has no name, and its type is read from the name (%s)",
			path, inference.ContextFileFormats())
	}
	if !inference.SupportsContextFile(name) {
		return attachment{}, fmt.Errorf(
			"cannot attach %s: %q is not a type a model can read (%s)",
			path, name, inference.ContextFileFormats())
	}

	// Trusted over the descriptor's own Size: the descriptor travelled
	// through a step's output, and the store is what actually holds the file.
	meta, err := r.files.Stat(ctx, file.Ref)
	if err != nil {
		return attachment{}, fmt.Errorf("cannot attach %s: %w", path, err)
	}
	if meta.Size > inference.MaxContextFileBytes {
		return attachment{}, fmt.Errorf(
			"cannot attach %s: %q is %d bytes and a step may attach at most %d",
			path, name, meta.Size, int64(inference.MaxContextFileBytes))
	}

	reader, err := r.files.Open(ctx, file.Ref)
	if err != nil {
		return attachment{}, fmt.Errorf("cannot attach %s: %w", path, err)
	}
	defer func() { _ = reader.Close() }()

	// Read with one byte of headroom past the limit: a file that grew between
	// the stat and the read is caught here rather than in the request body.
	body, err := io.ReadAll(io.LimitReader(reader, inference.MaxContextFileBytes+1))
	if err != nil {
		return attachment{}, fmt.Errorf("cannot attach %s: %w", path, err)
	}
	if len(body) > inference.MaxContextFileBytes {
		return attachment{}, fmt.Errorf(
			"cannot attach %s: %q exceeds the %d-byte limit",
			path, name, int64(inference.MaxContextFileBytes))
	}

	return attachment{
		sent: inference.ContextFile{
			Filename: name,
			Content:  base64.StdEncoding.EncodeToString(body),
		},
		// The size recorded is what was actually read, not what the
		// descriptor claimed: the record should say what the model was given.
		recorded: workflows.StepAttachment{
			Path: path, Ref: file.Ref, Name: name, Size: int64(len(body)),
		},
	}, nil
}
