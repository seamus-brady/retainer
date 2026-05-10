// Package embed produces dense vectors for text — used by CBR
// retrieval as the 6th signal alongside field score, inverted index,
// recency, domain match, and utility.
//
// Why an interface, not a concrete type. Tying institutional memory
// (case vectors persisted across years) to one vendor's embedding
// model means a vendor change kills the case base. The interface
// stays in front so an Ollama / Voyage / OpenAI / ONNX-cgo backend
// can drop in later. Stored vectors carry an embedder ID so a
// future model swap is a known migration: re-embed from source case
// text (which we have, in JSONL) and rewrite the vectors.
//
// Default impl: Hugot + GoMLX, model all-MiniLM-L6-v2 (384-dim).
// Pure Go, no cgo. First run downloads the model into the workspace
// data dir; subsequent runs load from cache.
package embed

import (
	"context"
	"hash/fnv"
)

// Embedder produces a dense vector for a text input.
//
// Implementations must be safe to call from multiple goroutines.
// The typical Retainer pattern is one Embed per case derivation
// (post-cycle, archivist), but query-time retrieval may also embed
// the query text — concurrent calls are realistic.
type Embedder interface {
	// Embed returns a vector of length Dimensions() for the input.
	// Returning an error MUST NOT corrupt the embedder's state — the
	// caller should be free to retry or move on. CBR retrieval
	// auto-renormalises weights when this fails so a transient
	// embedder error doesn't kill the whole retrieval.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Dimensions is the vector length the embedder produces.
	// Constant per-instance. Callers can pre-allocate slices.
	Dimensions() int

	// ID identifies the embedder + model + version. Stored alongside
	// vectors in JSONL so a future model swap is detectable.
	// Format: "<backend>/<model>/v<n>", e.g.
	// "hugot/all-minilm-l6-v2/v1".
	ID() string

	// Close releases backing resources (model session, ONNX arena).
	// Safe to call multiple times; subsequent calls return nil.
	// Idempotent so deferred Close in a Permanent supervisor's stop
	// handler doesn't double-free.
	Close() error
}

// MockEmbedder is a deterministic embedder for tests. Same text
// produces the same vector across runs; different texts produce
// different vectors with high probability (FNV hash collisions are
// the only failure mode and don't matter for retrieval-ranking
// correctness).
//
// Not for production use — vectors are uncorrelated with actual
// semantic similarity. Tests that exercise CBR's 6-signal retrieval
// use this so they don't need real ML inference.
type MockEmbedder struct {
	dim int
	id  string
}

// NewMock returns a deterministic mock embedder of the given
// dimensions. ID is "mock/seed-fnv/v1" — chosen so production CBR
// retrievals never confuse mock vectors with real ones.
func NewMock(dim int) *MockEmbedder {
	if dim <= 0 {
		dim = 1
	}
	return &MockEmbedder{dim: dim, id: "mock/seed-fnv/v1"}
}

// Embed returns a deterministic vector seeded by FNV-1a over (text,
// axis-index). Vectors are in [-1.0, 1.0] per axis.
func (m *MockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	out := make([]float32, m.dim)
	for i := 0; i < m.dim; i++ {
		h := fnv.New32a()
		_, _ = h.Write([]byte(text))
		// Salt each axis so different positions in the vector get
		// independent values — otherwise every axis would hold the
		// same number.
		_, _ = h.Write([]byte{byte(i), byte(i >> 8)})
		// Map [0, 9999] → [-1.0, 1.0] in 0.0002 increments.
		out[i] = float32(h.Sum32()%10000)/5000.0 - 1.0
	}
	return out, nil
}

// Dimensions returns the vector length.
func (m *MockEmbedder) Dimensions() int { return m.dim }

// ID is the embedder identifier stamped on stored vectors.
func (m *MockEmbedder) ID() string { return m.id }

// Close is a no-op for the mock; nothing to release.
func (m *MockEmbedder) Close() error { return nil }
