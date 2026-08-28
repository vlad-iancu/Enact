package workflows

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// englishMessages renders the validator's error kinds. The library localises
// its messages; the platform speaks English everywhere else, so it does here
// too rather than following whatever locale the process happens to have.
var englishMessages = message.NewPrinter(language.English)

// MaxSchemaBytes caps a stored schema. Schemas are small; this is far above
// anything legitimate and well below anything that would make a workflow
// record expensive to read.
const MaxSchemaBytes = 64 << 10 // 64 KiB

// maxSchemaErrors bounds how many validation failures are reported at once.
// A caller needs to know what is wrong, not every way in which it is wrong.
const maxSchemaErrors = 8

// CompileSchema checks that a document is a usable JSON Schema and returns it
// ready to validate against.
//
// Unlike an agent's output_schema — where Bedrock owns the keyword semantics
// and second-guessing it would mean rejecting schemas that work — these
// schemas are enforced HERE. So they are compiled at save time: an unusable
// schema is an authoring error, not something to discover when a run trips
// over it.
func CompileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("schema is %d bytes; the limit is %d", len(raw), MaxSchemaBytes)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %w", err)
	}
	if _, ok := doc.(map[string]any); !ok {
		return nil, fmt.Errorf("schema must be a JSON object, for example {\"type\":\"object\",\"properties\":{…}}")
	}
	compiler := jsonschema.NewCompiler()
	// The URL is a handle, not a fetch target: the document is supplied
	// directly, so nothing here reaches the network. A schema with a remote
	// $ref would fail to compile, which is the correct outcome — a workflow
	// must not depend on somebody else's server being up.
	const loc = "workflow://schema"
	if err := compiler.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("schema could not be read: %w", err)
	}
	compiled, err := compiler.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("schema is not a valid JSON Schema: %s", firstLine(err.Error()))
	}
	return compiled, nil
}

// ValidateAgainst checks an instance against a schema, returning a message
// written for whoever has to fix it.
//
// An empty schema accepts anything: schemas are optional, and absent means
// unconstrained rather than forbidden.
func ValidateAgainst(raw json.RawMessage, instance json.RawMessage) error {
	compiled, err := CompileSchema(raw)
	if err != nil || compiled == nil {
		return err
	}
	// A null instance is validated as JSON null, not skipped: a schema that
	// requires fields should reject a workflow triggered with no payload at
	// all, rather than quietly accepting it.
	var decoded any
	if len(instance) > 0 {
		if err := json.Unmarshal(instance, &decoded); err != nil {
			return fmt.Errorf("value is not valid JSON: %w", err)
		}
	}
	if err := compiled.Validate(decoded); err != nil {
		return fmt.Errorf("%s", describeValidationError(err))
	}
	return nil
}

// describeValidationError flattens the validator's error tree into a short
// list of concrete problems.
//
// The library's own rendering is a nested outline built for reading a whole
// schema; what someone fixing a payload wants is "/customer: got number, want
// string" and nothing else.
func describeValidationError(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return firstLine(err.Error())
	}
	problems := collectCauses(ve, nil)
	sort.Strings(problems)
	problems = dedupe(problems)
	if len(problems) == 0 {
		return firstLine(err.Error())
	}
	if len(problems) > maxSchemaErrors {
		extra := len(problems) - maxSchemaErrors
		problems = problems[:maxSchemaErrors]
		problems = append(problems, fmt.Sprintf("… and %d more", extra))
	}
	return strings.Join(problems, "; ")
}

// collectCauses walks to the LEAVES of the error tree. A parent node says
// "properties failed"; only a leaf says which property and why.
func collectCauses(ve *jsonschema.ValidationError, into []string) []string {
	if len(ve.Causes) == 0 {
		location := ve.InstanceLocation
		path := "/" + strings.Join(location, "/")
		if len(location) == 0 {
			path = "the value"
		}
		return append(into, fmt.Sprintf("%s: %s", path, ve.ErrorKind.LocalizedString(englishMessages)))
	}
	for _, cause := range ve.Causes {
		into = collectCauses(cause, into)
	}
	return into
}

func dedupe(in []string) []string {
	out := in[:0]
	var last string
	for _, s := range in {
		if s != last {
			out = append(out, s)
		}
		last = s
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
