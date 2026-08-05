package utils

// Helpers for the inference cases. The cases are deliberately restricted to
// request-validation paths that never reach Bedrock, so runs cost no model
// tokens.

const InferenceAudience = "enact-model-inference"

type InferenceErrDTO struct {
	Error string `json:"error"`
}
