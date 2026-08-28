package workflows

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"enact/internal/files"
)

// attachPath is what an attachment path may look like: dotted segments of the
// same names a code step reads — input, previous, steps.<name> — with numeric
// segments indexing into arrays.
//
// The syntax deliberately matches what a code step writes (context.previous
// .document) rather than what a template writes ({{ .Previous.document }}).
// An attachment is a value being located, not text being rendered, and the
// editor completes both from the same resolved shapes.
var attachPath = regexp.MustCompile(`^(input|previous|steps)(\.[A-Za-z0-9_]+)*$`)

// ValidateAttachPath checks an attachment path's syntax, without a context to
// resolve it against. Returns a message written for the author.
func ValidateAttachPath(path string) (string, bool) {
	if path == "" {
		return "an attachment path is empty", false
	}
	if !attachPath.MatchString(path) {
		return fmt.Sprintf(
			"%q is not a usable attachment path; write a path into the step context, such as \"previous.document\" or \"steps.export.file\"",
			path), false
	}
	return "", true
}

// Attachment resolves an attachment path against a step's context and returns
// the file it names.
//
// Every failure here is an authoring mistake — a path into nothing, or a path
// into something that is not a file — so the errors name the path and say
// what was found instead. A run that cannot resolve an attachment fails the
// step rather than silently sending the model a prompt with the document
// missing, which would produce a confident answer about nothing.
func Attachment(ctx Context, path string) (files.File, error) {
	if msg, ok := ValidateAttachPath(path); !ok {
		return files.File{}, fmt.Errorf("%s", msg)
	}

	segments := strings.Split(path, ".")
	var current any
	switch segments[0] {
	case "input":
		current = ctx.Input
	case "previous":
		current = ctx.Previous
	case "steps":
		// A map of any, so the walk below handles the step name like any
		// other field rather than special-casing the first hop.
		steps := make(map[string]any, len(ctx.Steps))
		for name, value := range ctx.Steps {
			steps[name] = value
		}
		current = steps
	}

	for i, segment := range segments[1:] {
		next, err := descend(current, segment)
		if err != nil {
			// The path so far, so the message points at the segment that
			// failed rather than at the whole path.
			return files.File{}, fmt.Errorf("%s: %w", strings.Join(segments[:i+2], "."), err)
		}
		current = next
	}

	return asFile(path, current)
}

// descend takes one step into a decoded JSON value.
func descend(value any, segment string) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		child, present := typed[segment]
		if !present {
			return nil, fmt.Errorf("there is no %q here", segment)
		}
		return child, nil

	case []any:
		index, err := strconv.Atoi(segment)
		if err != nil {
			return nil, fmt.Errorf("this is a list, so %q must be a number", segment)
		}
		if index < 0 || index >= len(typed) {
			return nil, fmt.Errorf("this list has %d entries, so there is no %d", len(typed), index)
		}
		return typed[index], nil

	case nil:
		return nil, fmt.Errorf("there is nothing here to look inside")

	default:
		return nil, fmt.Errorf("this is a %T, which has no %q inside it", value, segment)
	}
}

// asFile reads a resolved value as a file descriptor.
//
// The check is the "$file" marker rather than the shape of the object: a step
// that returns something file-shaped by coincidence should not be attachable,
// and a descriptor the store wrote always carries the marker.
func asFile(path string, value any) (files.File, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return files.File{}, fmt.Errorf("%s is a %T, not a file", path, value)
	}
	if _, marked := fields["$file"]; !marked {
		return files.File{}, fmt.Errorf("%s is not a file; a file has a %q reference on it", path, "$file")
	}

	// Round-tripped through JSON rather than read field by field, so the
	// descriptor's own tags decide how it is read and the two cannot drift.
	encoded, err := json.Marshal(fields)
	if err != nil {
		return files.File{}, fmt.Errorf("%s could not be read as a file: %w", path, err)
	}
	var file files.File
	if err := json.Unmarshal(encoded, &file); err != nil {
		return files.File{}, fmt.Errorf("%s could not be read as a file: %w", path, err)
	}
	if file.Ref == "" {
		return files.File{}, fmt.Errorf("%s has an empty file reference", path)
	}
	return file, nil
}
