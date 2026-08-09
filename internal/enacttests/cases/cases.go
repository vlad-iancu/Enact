// Package cases holds the integration test cases, one per file, each a
// struct implementing utils.TestCase (Setup / Run / TearDown).
package cases

import "enact/internal/enacttests/utils"

// All is the manifest of every integration test case, populated manually —
// adding a case means implementing utils.TestCase in its own file here and
// listing its factory below.
func All() []utils.Factory {
	return []utils.Factory{
		// Agent management
		NewAgentCreateGetDelete,
		NewAgentPartialUpdate,
		NewAgentRejectsUnknownModel,
		NewAgentGetMissing,
		NewAgentDeleteIsolation,
		NewAgentNameLifecycle,
		NewAgentRAGUploadListDelete,

		// Knowledge bases
		NewKBCreateGetDelete,
		NewKBDocumentsOnDetail,
		NewKBDeleteIsolation,
		NewKBNameLifecycle,
		NewKBDocumentDeleteAsync,

		// enact-main (UI backend), exercised as a browser: cookie sessions,
		// no service tokens.
		NewMainAuthSessionLifecycle,
		NewMainAgentsCrudProxy,
		NewMainKBCrudProxy,
		NewMainConversationsLifecycle,
		NewMainModelsList,
		NewMainInferenceValidation,
		NewMainAvatarValidation,

		// Model catalogue
		NewModelsList,

		// Inference request validation
		NewInferenceRejectsUnknownModel,
		NewInferenceRequiresMessages,
		NewInferenceRejectsBadTopK,
		NewInferenceContextFilesValidation,
	}
}
