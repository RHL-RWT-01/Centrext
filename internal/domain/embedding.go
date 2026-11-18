package domain

import (
	"context"
	"time"
)

// EmbedKind tells providers that tune for it whether text is a query or a document.
type EmbedKind string

const (
	EmbedDocument EmbedKind = "document"
	EmbedQuery    EmbedKind = "query"
)

// Embedder turns text into vectors (ADR-023). Implementations call a hosted
// API; the application decides what may be sent.
type Embedder interface {
	// Model names the provider and model, e.g. "openai/text-embedding-3-small".
	// Vectors from different models are never compared.
	Model() string
	// Embed returns one unit-length vector per text, in order.
	Embed(ctx context.Context, texts []string, kind EmbedKind) ([][]float32, error)
}

// EmbedJob is a context whose head revision has no current vector.
type EmbedJob struct {
	ContextID string
	Version   int
	Title     string
	Body      string
}

// EmbeddedContext is an active context with the vector of its head revision.
type EmbeddedContext struct {
	Context Context
	Vector  []float32
}

// EmbeddingFilter selects contexts for semantic search.
type EmbeddingFilter struct {
	Model        string
	NamespaceIDs []string
	Types        []ContextType
	NotExpiredAt time.Time
}

type EmbeddingRepo interface {
	MissingEmbeddings(model string, namespaceIDs []string, notExpiredAt time.Time, limit int) ([]EmbedJob, error)
	PutEmbedding(contextID string, version int, model string, vector []float32, at time.Time) error
	EmbeddedContexts(EmbeddingFilter) ([]EmbeddedContext, error)
	// CountEmbeddings returns active contexts in the namespaces, and how many have a current vector.
	CountEmbeddings(model string, namespaceIDs []string, notExpiredAt time.Time) (active, embedded int, err error)
}
