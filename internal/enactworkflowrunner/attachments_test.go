package enactworkflowrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"enact/internal/files"
	"enact/internal/inference"
	"enact/internal/logging"
	"enact/internal/workflows"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	store, err := files.NewFS(files.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("create the file store: %v", err)
	}
	return &Runner{files: store, logger: logging.New()}
}

// store puts a file and returns the context a step would see with that file
// as the previous step's output.
func store(t *testing.T, r *Runner, name string, body []byte) (files.File, workflows.Context) {
	t.Helper()
	file, err := r.files.Put(context.Background(), files.InExecution("w1", "e9"),
		strings.NewReader(string(body)), files.Metadata{Name: name, MimeType: "application/pdf"})
	if err != nil {
		t.Fatalf("store a file: %v", err)
	}

	encoded, err := json.Marshal(map[string]any{"document": file})
	if err != nil {
		t.Fatalf("marshal the step output: %v", err)
	}
	var previous any
	if err := json.Unmarshal(encoded, &previous); err != nil {
		t.Fatalf("decode the step output: %v", err)
	}
	return file, workflows.Context{Previous: previous, Steps: map[string]any{}}
}

func TestAttachmentsForReadsTheStoredBytes(t *testing.T) {
	runner := testRunner(t)
	_, stepCtx := store(t, runner, "q3.pdf", []byte("report bytes"))

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	attachments, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err != nil {
		t.Fatalf("resolve attachments: %v", err)
	}
	if len(attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(attachments))
	}
	if attachments[0].sent.Filename != "q3.pdf" {
		t.Errorf("filename = %q, want q3.pdf", attachments[0].sent.Filename)
	}
	decoded, err := base64.StdEncoding.DecodeString(attachments[0].sent.Content)
	if err != nil {
		t.Fatalf("the content is not valid base64: %v", err)
	}
	if string(decoded) != "report bytes" {
		t.Errorf("content = %q, want the stored bytes", decoded)
	}
}

// The record has to say what the model was given, and where it came from:
// either half alone leaves the other a guess when a run is read back.
func TestAttachmentsForRecordsWhatWasSent(t *testing.T) {
	runner := testRunner(t)
	file, stepCtx := store(t, runner, "q3.pdf", []byte("report bytes"))

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	attachments, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err != nil {
		t.Fatalf("resolve attachments: %v", err)
	}

	got := recorded(attachments)
	if len(got) != 1 {
		t.Fatalf("recorded %d attachments, want 1", len(got))
	}
	want := workflows.StepAttachment{
		Path: "previous.document", Ref: file.Ref, Name: "q3.pdf", Size: int64(len("report bytes")),
	}
	if got[0] != want {
		t.Errorf("recorded %+v, want %+v", got[0], want)
	}
}

// A step that attaches nothing must record nothing, so the field is absent
// from the stored run rather than an empty list.
func TestRecordedAndSentAreEmptyForNoAttachments(t *testing.T) {
	if recorded(nil) != nil {
		t.Error("no attachments recorded a non-nil list")
	}
	if sent(nil) != nil {
		t.Error("no attachments sent a non-nil list")
	}
}

// What is recorded and what is sent describe one file, so they must not be
// able to disagree about which.
func TestRecordedAndSentAgreeOnTheFilename(t *testing.T) {
	runner := testRunner(t)
	_, stepCtx := store(t, runner, "q3.pdf", []byte("report"))

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	attachments, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err != nil {
		t.Fatalf("resolve attachments: %v", err)
	}
	for i, a := range attachments {
		if a.sent.Filename != a.recorded.Name {
			t.Errorf("attachment %d: sent %q but recorded %q", i, a.sent.Filename, a.recorded.Name)
		}
		if int64(len(a.sent.Content)) == 0 {
			t.Errorf("attachment %d: recorded %d bytes but sent nothing", i, a.recorded.Size)
		}
	}
}

func TestAttachmentsForIsEmptyWhenNothingIsDeclared(t *testing.T) {
	runner := testRunner(t)
	_, stepCtx := store(t, runner, "q3.pdf", []byte("report"))

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent}
	attachments, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err != nil {
		t.Fatalf("a step with no attachments failed: %v", err)
	}
	if attachments != nil {
		t.Errorf("got %d attachments, want none", len(attachments))
	}
	if recorded(attachments) != nil {
		t.Error("a step with no attachments recorded some")
	}
}

// The store takes 100 MiB and a model takes 4.5 MB, so the runner has to be
// the one that says no — and has to say it before reading the file.
func TestAttachmentsForRefusesAFileTooLargeForTheModel(t *testing.T) {
	runner := testRunner(t)
	oversized := make([]byte, inference.MaxContextFileBytes+1)
	_, stepCtx := store(t, runner, "huge.pdf", oversized)

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	_, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err == nil {
		t.Fatal("an oversized file was attached")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("error %q does not state the limit", err)
	}
}

func TestAttachmentsForRefusesATypeNoModelReads(t *testing.T) {
	runner := testRunner(t)
	_, stepCtx := store(t, runner, "diagram.png", []byte("\x89PNG"))

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	_, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err == nil {
		t.Fatal("a png was attached")
	}
	if !strings.Contains(err.Error(), "pdf") {
		t.Errorf("error %q does not say what would have worked", err)
	}
}

func TestAttachmentsForRefusesAFileWithNoName(t *testing.T) {
	runner := testRunner(t)
	_, stepCtx := store(t, runner, "", []byte("bytes"))

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	if _, err := runner.attachmentsFor(context.Background(), step, stepCtx); err == nil {
		t.Fatal("a nameless file was attached, but its type is read from the name")
	}
}

// A reference that resolves to nothing must fail the step. Sending the prompt
// anyway would get a confident answer about a document the model never saw.
func TestAttachmentsForFailsOnAMissingFile(t *testing.T) {
	runner := testRunner(t)
	file, stepCtx := store(t, runner, "q3.pdf", []byte("report"))
	if err := runner.files.DeleteAt(context.Background(), files.InExecution("w1", "e9")); err != nil {
		t.Fatalf("delete the file: %v", err)
	}

	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	_, err := runner.attachmentsFor(context.Background(), step, stepCtx)
	if err == nil {
		t.Fatalf("a deleted file (%s) was attached", file.Ref)
	}
	if !strings.Contains(err.Error(), "previous.document") {
		t.Errorf("error %q does not name the path that failed", err)
	}
}

func TestAttachmentsForFailsWithoutAStore(t *testing.T) {
	runner := &Runner{logger: logging.New()}
	step := workflows.Step{Name: "summarise", Type: workflows.StepTypeAgent, Attach: []string{"previous.document"}}
	_, err := runner.attachmentsFor(context.Background(), step, workflows.Context{})
	if err == nil {
		t.Fatal("a step attached a file with no store configured")
	}
	if !strings.Contains(err.Error(), "file store") {
		t.Errorf("error %q does not name the missing store", err)
	}
}
