// Package models is the registry mapping user-facing friendly model names to
// the underlying Amazon Bedrock model (or inference-profile) ids. Agents and
// the model-management service refer to models by friendly name; the
// inference service resolves them to Bedrock ids at call time.
//
// The default registry targets the eu-* cross-region inference profiles
// (eu.anthropic.…), in line with the eu-north-1 region this project is
// configured for — eu-north-1 does not support on-demand invocation of the
// underlying foundation-model ids directly. Adjust the entries to match the
// inference profiles enabled in your AWS account / region.
package models

// Model describes one available model. Name is the id used across the
// platform's APIs; DisplayName is what UIs show people.
type Model struct {
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	BedrockModelID string `json:"bedrock_model_id"`
	Description    string `json:"description,omitempty"`
}

// defaultModels is the built-in registry. Edit this list to expose different
// Bedrock models under friendly names.
var defaultModels = []Model{
	{
		Name:           "claude-haiku-4-5",
		DisplayName:    "Claude Haiku 4.5",
		BedrockModelID: "eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		Description:    "Anthropic Claude Haiku 4.5.",
	},
	{
		Name:           "claude-sonnet-4-5",
		DisplayName:    "Claude Sonnet 4.5",
		BedrockModelID: "eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Description:    "Anthropic Claude Sonnet 4.5.",
	},
	{
		Name:           "claude-sonnet-4-6",
		DisplayName:    "Claude Sonnet 4.6",
		BedrockModelID: "eu.anthropic.claude-sonnet-4-6",
		Description:    "Anthropic Claude Sonnet 4.6.",
	},
	{
		Name:           "claude-opus-4-5",
		DisplayName:    "Claude Opus 4.5",
		BedrockModelID: "eu.anthropic.claude-opus-4-5-20251101-v1:0",
		Description:    "Anthropic Claude Opus 4.5.",
	},
	{
		Name:           "claude-opus-4-6",
		DisplayName:    "Claude Opus 4.6",
		BedrockModelID: "eu.anthropic.claude-opus-4-6-v1",
		Description:    "Anthropic Claude Opus 4.6.",
	},
}

// List returns all available models.
func List() []Model {
	out := make([]Model, len(defaultModels))
	copy(out, defaultModels)
	return out
}

// Resolve maps a friendly model name to its Bedrock model id. The boolean
// reports whether the name is known.
func Resolve(name string) (string, bool) {
	for _, m := range defaultModels {
		if m.Name == name {
			return m.BedrockModelID, true
		}
	}
	return "", false
}
