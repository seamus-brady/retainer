package embed

import (
	"context"
	"testing"
)

// ---- MockEmbedder ----

func TestMock_DimensionsConsistent(t *testing.T) {
	m := NewMock(384)
	if m.Dimensions() != 384 {
		t.Errorf("Dimensions = %d, want 384", m.Dimensions())
	}
	v, err := m.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 384 {
		t.Errorf("len(v) = %d, want 384", len(v))
	}
}

func TestMock_DeterministicSameTextSameVector(t *testing.T) {
	m := NewMock(8)
	v1, _ := m.Embed(context.Background(), "retainer")
	v2, _ := m.Embed(context.Background(), "retainer")
	if len(v1) != len(v2) {
		t.Fatalf("dims mismatch")
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Errorf("axis %d differs across calls: %g vs %g", i, v1[i], v2[i])
		}
	}
}

func TestMock_DifferentTextDifferentVector(t *testing.T) {
	m := NewMock(8)
	v1, _ := m.Embed(context.Background(), "alpha")
	v2, _ := m.Embed(context.Background(), "beta")
	allEqual := true
	for i := range v1 {
		if v1[i] != v2[i] {
			allEqual = false
			break
		}
	}
	if allEqual {
		t.Errorf("different inputs produced identical vectors: %v vs %v", v1, v2)
	}
}

func TestMock_VectorsInUnitRange(t *testing.T) {
	m := NewMock(64)
	v, _ := m.Embed(context.Background(), "anything")
	for i, x := range v {
		if x < -1.0 || x > 1.0 {
			t.Errorf("axis %d out of [-1,1]: %g", i, x)
		}
	}
}

func TestMock_ID(t *testing.T) {
	m := NewMock(384)
	if m.ID() != "mock/seed-fnv/v1" {
		t.Errorf("ID = %q, want mock/seed-fnv/v1", m.ID())
	}
}

func TestMock_CloseIsNoop(t *testing.T) {
	m := NewMock(8)
	if err := m.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v (must be idempotent)", err)
	}
	// Embed still works after Close — mock has no resources to release.
	if _, err := m.Embed(context.Background(), "x"); err != nil {
		t.Errorf("Embed after Close: %v", err)
	}
}

func TestMock_ZeroDimDefaultsToOne(t *testing.T) {
	m := NewMock(0)
	if m.Dimensions() != 1 {
		t.Errorf("zero dim should clamp to 1; got %d", m.Dimensions())
	}
	v, _ := m.Embed(context.Background(), "x")
	if len(v) != 1 {
		t.Errorf("len = %d, want 1", len(v))
	}
}

// ---- HugotEmbedder construction errors ----
//
// Real Hugot embedding requires downloading ~90MB and running ONNX
// inference; that's verified end-to-end in a separate integration
// test (TestHugot_RealEmbedding, build-tag gated). Here we cover the
// argument-validation and cache-presence-detection paths that don't
// need the network.

func TestHugot_MissingCacheDirIsError(t *testing.T) {
	_, err := NewHugot(context.Background(), HugotConfig{ModelCacheDir: ""})
	if err == nil {
		t.Fatal("expected error for empty ModelCacheDir")
	}
}
