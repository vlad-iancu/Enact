package workflows

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"enact/internal/files"
)

// The Google step types. Three types rather than one with a "service" field,
// because they are three different things to an author: a document, a
// spreadsheet and a deck do not share operations, and a single type would
// spend most of its validation explaining which fields apply to which.
const (
	// StepTypeGoogleDocs reads a Google Doc out of, or writes one back into,
	// the user's own Drive.
	StepTypeGoogleDocs = "google-docs"
	// StepTypeGoogleSheets does the same for a spreadsheet, and can append
	// rows to one — which is how a workflow logs what it did.
	StepTypeGoogleSheets = "google-sheets"
	// StepTypeGoogleSlides does the same for a presentation.
	StepTypeGoogleSlides = "google-slides"
)

// Operations a Google step can perform. Not every type supports every one;
// see googleSteps.
const (
	// GoogleOperationExport fetches the file and stores it, which a later
	// agent step can attach. This is what makes a real document readable by a
	// model: a template can only produce text, so the bytes travel as a file.
	GoogleOperationExport = "export"
	// GoogleOperationCreate makes a new file from what an earlier step
	// produced.
	GoogleOperationCreate = "create"
	// GoogleOperationAppend adds rows to an existing spreadsheet. Sheets only:
	// a document has no equivalent that is not just "create", and a deck's
	// would mean inventing a slide layout.
	GoogleOperationAppend = "append"
)

// Deliberately kept as the older names so existing workflows and callers do
// not have to change. They are the same constants.
const (
	DocsOperationExport = GoogleOperationExport
	DocsOperationCreate = GoogleOperationCreate
)

// exportFormat is one format a file may be exported as: the MIME type Drive is
// asked for, and the extension the stored file is named with.
//
// The extension matters beyond tidiness. An attached file's type is read from
// its NAME, so a file stored without one cannot be attached however correct
// its bytes are — and only some of these are types a model can read at all.
type exportFormat struct {
	MimeType  string
	Extension string
	// Attachable reports whether a model can be given this format. False is
	// not a reason to refuse the export — the file is still downloadable — but
	// it is worth being able to say so.
	Attachable bool
}

// googleStep describes what one Google step type can do.
type googleStep struct {
	// Label names the thing in a message: "document", "spreadsheet", "deck".
	Label string
	// Formats are the export formats this type offers.
	Formats map[string]exportFormat
	// Operations are the operations it supports.
	Operations []string
}

var googleSteps = map[string]googleStep{
	StepTypeGoogleDocs: {
		Label:      "document",
		Operations: []string{GoogleOperationExport, GoogleOperationCreate},
		Formats: map[string]exportFormat{
			"pdf":  {"application/pdf", ".pdf", true},
			"docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx", true},
			"txt":  {"text/plain", ".txt", true},
			"html": {"text/html", ".html", true},
			"md":   {"text/markdown", ".md", true},
		},
	},
	StepTypeGoogleSheets: {
		Label:      "spreadsheet",
		Operations: []string{GoogleOperationExport, GoogleOperationCreate, GoogleOperationAppend},
		Formats: map[string]exportFormat{
			"pdf": {"application/pdf", ".pdf", true},
			"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
				".xlsx", true},
			// Drive exports only the FIRST sheet as csv. Offered anyway
			// because a single-sheet export is usually what a model should be
			// reading — a spreadsheet as csv is far cheaper to reason over
			// than as a pdf.
			"csv": {"text/csv", ".csv", true},
		},
	},
	StepTypeGoogleSlides: {
		Label:      "presentation",
		Operations: []string{GoogleOperationExport, GoogleOperationCreate},
		Formats: map[string]exportFormat{
			"pdf": {"application/pdf", ".pdf", true},
			// A deck as pptx can be downloaded but NOT attached: no model
			// reads it. Kept because "export my deck" is a reasonable thing
			// to want without a model involved.
			"pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation",
				".pptx", false},
			"txt": {"text/plain", ".txt", true},
		},
	},
}

// IsGoogleStep reports whether a step type is one of the Google ones.
func IsGoogleStep(stepType string) bool {
	_, ok := googleSteps[stepType]
	return ok
}

// GoogleStepTypes lists them, for an error that says what would have worked.
func GoogleStepTypes() string {
	names := make([]string, 0, len(googleSteps))
	for name := range googleSteps {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// GoogleExportFormat resolves a format for one step type. The boolean reports
// whether the type offers it.
func GoogleExportFormat(stepType, format string) (exportFormat, bool) {
	spec, ok := googleSteps[stepType]
	if !ok {
		return exportFormat{}, false
	}
	f, ok := spec.Formats[format]
	return f, ok
}

// DefaultGoogleExportFormat is what an export uses when none is named. Pdf
// everywhere: every type offers it, and every model can read it.
const DefaultGoogleExportFormat = "pdf"

// GoogleExportFormatNames lists a type's formats in a stable order.
func GoogleExportFormatNames(stepType string) string {
	spec, ok := googleSteps[stepType]
	if !ok {
		return ""
	}
	names := make([]string, 0, len(spec.Formats))
	for name := range spec.Formats {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// supportsOperation reports whether a type offers an operation.
func supportsOperation(stepType, operation string) bool {
	spec, ok := googleSteps[stepType]
	if !ok {
		return false
	}
	for _, candidate := range spec.Operations {
		if candidate == operation {
			return true
		}
	}
	return false
}

// operationNames lists a type's operations, for an error message.
func operationNames(stepType string) string {
	spec, ok := googleSteps[stepType]
	if !ok {
		return ""
	}
	quoted := make([]string, 0, len(spec.Operations))
	for _, op := range spec.Operations {
		quoted = append(quoted, `"`+op+`"`)
	}
	return strings.Join(quoted, ", ")
}

// validateGoogleStep checks a Google step's shape.
//
// Every templated field is PARSED here, so a typo in {{ .Input.doc_id }} is an
// authoring error rather than something found after a run has already spent an
// agent step or two getting to it.
//
// What is NOT checked is whether the named provider exists — that needs a call
// to the identities service, so it belongs to the caller that has one. This
// function stays free of I/O so the rules can be tested without a service.
func validateGoogleStep(position string, step Step) (string, bool) {
	spec := googleSteps[step.Type]
	label := fmt.Sprintf("%s (%s)", position, step.Name)

	if step.Provider == "" {
		return fmt.Sprintf("%s is a %s step but names no provider; it needs one to act as the person running the workflow",
			label, spec.Label), false
	}
	if step.AgentID != "" || step.Prompt != "" || step.Code != "" {
		return label + " is a Google step but also carries an agent, a prompt or code", false
	}
	// Attachments are how files reach a MODEL. This step produces one rather
	// than reading one, and has nothing to send anywhere.
	if len(step.Attach) > 0 {
		return label + " is a Google step but attaches files; only an agent step sends files to a model", false
	}
	// The output shape follows from the operation, so there is nothing here
	// for an author to declare or to get wrong.
	if len(step.OutputSchema) > 0 {
		return label + " declares an output_schema, but a Google step's output shape is fixed by its operation", false
	}
	if step.Operation == "" {
		return fmt.Sprintf("%s has no operation; a %s step supports %s", label, spec.Label, operationNames(step.Type)), false
	}
	if !supportsOperation(step.Type, step.Operation) {
		return fmt.Sprintf("%s has operation %q; a %s step supports %s",
			label, step.Operation, spec.Label, operationNames(step.Type)), false
	}

	switch step.Operation {
	case GoogleOperationExport:
		if step.DocumentID == "" {
			return fmt.Sprintf("%s exports a %s but names none; set document_id", label, spec.Label), false
		}
		if step.Title != "" || step.Body != "" || step.Rows != "" || step.Range != "" {
			return fmt.Sprintf("%s exports a %s but also sets fields that belong to another operation", label, spec.Label), false
		}
		if step.Format != "" {
			if _, known := spec.Formats[step.Format]; !known {
				return fmt.Sprintf("%s exports as %q; a %s step offers %s",
					label, step.Format, spec.Label, GoogleExportFormatNames(step.Type)), false
			}
		}
		return parseTemplates(step.Name, map[string]string{"document_id": step.DocumentID})

	case GoogleOperationCreate:
		if step.Title == "" {
			return fmt.Sprintf("%s creates a %s but gives it no title", label, spec.Label), false
		}
		if step.DocumentID != "" || step.Format != "" || step.Range != "" {
			return fmt.Sprintf("%s creates a %s but also sets fields that belong to another operation", label, spec.Label), false
		}
		// A deck has no body: creating one with content would mean inventing a
		// slide layout, which is a decision this step should not be making.
		if step.Type == StepTypeGoogleSlides && step.Body != "" {
			return label + " sets a body, but a presentation is created empty; add its slides with the Slides tools instead", false
		}
		// Rows belong to a spreadsheet, body to a document.
		if step.Type == StepTypeGoogleSheets && step.Body != "" {
			return label + " sets a body, but a spreadsheet takes rows; use rows instead", false
		}
		if step.Type != StepTypeGoogleSheets && step.Rows != "" {
			return fmt.Sprintf("%s sets rows, but only a spreadsheet takes them", label), false
		}
		return parseTemplates(step.Name, map[string]string{
			"title": step.Title, "body": step.Body, "rows": step.Rows,
		})

	case GoogleOperationAppend:
		if step.DocumentID == "" {
			return label + " appends rows but names no spreadsheet; set document_id", false
		}
		if step.Rows == "" {
			return label + " appends rows but has none; set rows", false
		}
		if step.Title != "" || step.Body != "" || step.Format != "" {
			return label + " appends rows but also sets fields that belong to another operation", false
		}
		return parseTemplates(step.Name, map[string]string{
			"document_id": step.DocumentID, "rows": step.Rows, "range": step.Range,
		})
	}
	return "", true
}

// parseTemplates checks each named template, skipping the empty ones.
func parseTemplates(stepName string, fields map[string]string) (string, bool) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	// Sorted so the same broken workflow always names the same field first.
	sort.Strings(names)
	for _, name := range names {
		if fields[name] == "" {
			continue
		}
		if err := ParsePrompt(stepName+"."+name, fields[name]); err != nil {
			return err.Error(), false
		}
	}
	return "", true
}

// GoogleOutputSchema is what a Google step produces, by operation.
//
// Fixed rather than declared: the step writes its own output, so its shape is
// a property of the operation and not something an author can be wrong about.
// Exported for the same reason files.FileSchema is — the shape an editor
// completes against and the shape the runner writes come from one definition.
func GoogleOutputSchema(stepType, operation string) json.RawMessage {
	spec, ok := googleSteps[stepType]
	if !ok {
		return nil
	}
	switch operation {
	case GoogleOperationExport:
		// The file is nested under "file" rather than being the whole output,
		// so the source's metadata has somewhere to sit beside it — and so the
		// attach path reads as steps.<name>.file.
		return json.RawMessage(`{
	"type": "object",
	"description": "A Google ` + spec.Label + ` exported to a file.",
	"properties": {
		"file": ` + string(files.FileSchema()) + `,
		"document_id": {"type": "string"},
		"format": {"type": "string"}
	},
	"required": ["file", "document_id"],
	"additionalProperties": false
}`)
	case GoogleOperationCreate:
		return json.RawMessage(`{
	"type": "object",
	"description": "A newly created Google ` + spec.Label + `.",
	"properties": {
		"document_id": {"type": "string"},
		"title": {"type": "string"},
		"url": {"type": "string", "description": "Where a person can open it"}
	},
	"required": ["document_id", "url"],
	"additionalProperties": false
}`)
	case GoogleOperationAppend:
		return json.RawMessage(`{
	"type": "object",
	"description": "Rows appended to a Google spreadsheet.",
	"properties": {
		"document_id": {"type": "string"},
		"url": {"type": "string"},
		"updated_range": {"type": "string", "description": "The cells the rows landed in"},
		"appended_rows": {"type": "integer"}
	},
	"required": ["document_id", "appended_rows"],
	"additionalProperties": false
}`)
	}
	return nil
}

// DocsOutputSchema is the Docs-specific spelling, kept so existing callers do
// not change.
func DocsOutputSchema(operation string) json.RawMessage {
	return GoogleOutputSchema(StepTypeGoogleDocs, operation)
}
