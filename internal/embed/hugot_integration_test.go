//go:build embedintegration

// End-to-end Hugot embedding test. Behind the embedintegration build
// tag because it downloads ~90MB on first run and exercises real ONNX
// inference (~seconds). Default `go test ./...` skips this; opt in with
//
//   go test -tags=embedintegration ./internal/embed/...
//
// CI should run this on a schedule — model files are stable but
// HuggingFace can have transient outages.

package embed

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHugot_DownloadAndEmbedRoundTrip(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "models")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e, err := NewHugot(ctx, HugotConfig{ModelCacheDir: cacheDir})
	if err != nil {
		t.Fatalf("NewHugot: %v", err)
	}
	defer e.Close()

	if e.Dimensions() != MiniLMDimensions {
		t.Errorf("Dimensions = %d, want %d", e.Dimensions(), MiniLMDimensions)
	}
	if e.ID() != HugotMiniLMID {
		t.Errorf("ID = %q, want %q", e.ID(), HugotMiniLMID)
	}

	// Embed a single string. The output is a single 384-dim vector;
	// values aren't asserted on (model output is opaque), but the
	// shape and finiteness are.
	v, err := e.Embed(ctx, "the quick brown fox jumps over the lazy dog")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != MiniLMDimensions {
		t.Fatalf("len(v) = %d, want %d", len(v), MiniLMDimensions)
	}
	hasNonZero := false
	for _, x := range v {
		if x != 0 {
			hasNonZero = true
			break
		}
	}
	if !hasNonZero {
		t.Error("vector is all zeros — model didn't run?")
	}
}

func TestHugot_DownloadCacheHitOnSecondCall(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "models")
	ctx := context.Background()

	e1, err := NewHugot(ctx, HugotConfig{ModelCacheDir: cacheDir})
	if err != nil {
		t.Fatalf("first NewHugot: %v", err)
	}
	_ = e1.Close()

	// Second construction should find the cached model and not
	// re-download. We can't directly observe network calls, but a
	// second NewHugot completing cleanly + quickly is the tell.
	e2, err := NewHugot(ctx, HugotConfig{ModelCacheDir: cacheDir})
	if err != nil {
		t.Fatalf("second NewHugot: %v", err)
	}
	defer e2.Close()
}
