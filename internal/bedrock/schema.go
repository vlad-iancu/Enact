package bedrock

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ValidateStructuredOutputSchema checks the one rule Bedrock enforces on a
// structured-output schema that a caller cannot discover until a generation is
// already under way.
//
// The rule, established against the API rather than from documentation: EVERY
// node of type "object" — the root, anything nested, and objects inside array
// items — must set "additionalProperties": false explicitly. Omitting it is
// rejected, and so is setting it to true. Non-object roots (an array or a
// string schema) are fine, and "required" need not list every property.
//
// It is checked here, at save time, because the alternative is where it used
// to land: a 400 from Bedrock in the middle of a streamed reply, quoting
// "output_config.format.schema" at somebody who was talking to an agent and
// has no idea what that is.
//
// Deliberately narrow. This does NOT try to validate JSON Schema in general —
// Bedrock owns those semantics and re-implementing them would mean rejecting
// schemas that work. It encodes one rule, one that bites reliably and lands
// far from its cause.
func ValidateStructuredOutputSchema(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("output schema is not valid JSON: %w", err)
	}
	return checkObjectNodes(doc, "")
}

// checkObjectNodes walks the schema's KEYWORD positions — not every map it can
// find. A property may legitimately be named "type" or "items", and treating
// user field names as schema keywords would invent errors that are not there.
func checkObjectNodes(node any, path string) error {
	schema, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if declaresObject(schema["type"]) {
		switch additional, present := schema["additionalProperties"]; {
		case !present:
			return fmt.Errorf(
				`%s is {"type":"object"} but does not set "additionalProperties"; Bedrock requires every object in a structured-output schema to set it to false explicitly`,
				describe(path))
		case additional != false:
			return fmt.Errorf(
				`%s sets "additionalProperties" to %v; Bedrock requires false`, describe(path), additional)
		}
	}

	// Sub-schemas keyed by name.
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions"} {
		named, ok := schema[keyword].(map[string]any)
		if !ok {
			continue
		}
		for name, sub := range named {
			if err := checkObjectNodes(sub, join(path, keyword, name)); err != nil {
				return err
			}
		}
	}
	// Sub-schemas in a single position.
	for _, keyword := range []string{"items", "contains", "not", "if", "then", "else", "propertyNames", "additionalItems"} {
		if sub, present := schema[keyword]; present {
			if err := checkObjectNodes(sub, join(path, keyword)); err != nil {
				return err
			}
		}
	}
	// Sub-schemas in a list.
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		list, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for i, sub := range list {
			if err := checkObjectNodes(sub, join(path, fmt.Sprintf("%s[%d]", keyword, i))); err != nil {
				return err
			}
		}
	}
	// "items" may itself be a list in older drafts.
	if list, ok := schema["items"].([]any); ok {
		for i, sub := range list {
			if err := checkObjectNodes(sub, join(path, fmt.Sprintf("items[%d]", i))); err != nil {
				return err
			}
		}
	}
	return nil
}

// declaresObject reports whether a "type" keyword names the object type,
// tolerating the array form ("type": ["object", "null"]).
func declaresObject(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed == "object"
	case []any:
		for _, entry := range typed {
			if name, ok := entry.(string); ok && name == "object" {
				return true
			}
		}
	}
	return false
}

func join(path string, parts ...string) string {
	all := append([]string{}, parts...)
	if path == "" {
		return strings.Join(all, ".")
	}
	return path + "." + strings.Join(all, ".")
}

// describe names the offending node for someone reading the message, rather
// than leaving them to find it.
func describe(path string) string {
	if path == "" {
		return "the output schema"
	}
	return "the output schema at " + path
}
