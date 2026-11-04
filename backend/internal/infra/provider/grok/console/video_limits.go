package console

// Console video reference limits follow the model-specific xAI contract.
// They are shared by admission control and the Console adapter so invalid
// asynchronous jobs are rejected before enqueueing without weakening the
// adapter's final request-boundary validation.
const (
	maxVideoReferenceImages          = 7
	maxVideo15ReferenceImages        = 14
	maxVideoReferenceDurationSeconds = 10
)
