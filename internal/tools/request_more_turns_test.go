package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCogTurnExtender records calls so tests can assert on
// what the tool dispatched.
type fakeCogTurnExtender struct {
	parentCycleID string
	additional    int
	reason        string
	newCap        int
	err           error
	calls         int
}

func (f *fakeCogTurnExtender) RequestMoreTurns(parentCycleID string, additional int, reason string) (int, error) {
	f.calls++
	f.parentCycleID = parentCycleID
	f.additional = additional
	f.reason = reason
	return f.newCap, f.err
}

func TestRequestMoreTurns_ToolMetadata(t *testing.T) {
	def := RequestMoreTurns{}.Tool()
	if def.Name != "request_more_turns" {
		t.Errorf("name = %q", def.Name)
	}
	if def.InputSchema.Properties["reason"].Type != "string" {
		t.Errorf("reason should be string")
	}
	foundReason := false
	for _, r := range def.InputSchema.Required {
		if r == "reason" {
			foundReason = true
			break
		}
	}
	if !foundReason {
		t.Errorf("reason should be required")
	}
}

func TestRequestMoreTurns_HappyPath(t *testing.T) {
	cog := &fakeCogTurnExtender{newCap: 15}
	h := RequestMoreTurns{Cog: cog}
	out, err := h.Execute(context.Background(), []byte(`{"additional":5,"reason":"diagnostic walk"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cog.calls != 1 {
		t.Errorf("calls = %d, want 1", cog.calls)
	}
	if cog.additional != 5 {
		t.Errorf("additional = %d, want 5", cog.additional)
	}
	if cog.reason != "diagnostic walk" {
		t.Errorf("reason = %q", cog.reason)
	}
	if !strings.Contains(out, "15") {
		t.Errorf("response should mention new cap 15: %q", out)
	}
}

func TestRequestMoreTurns_DefaultIncrement(t *testing.T) {
	cog := &fakeCogTurnExtender{newCap: 15}
	h := RequestMoreTurns{Cog: cog}
	if _, err := h.Execute(context.Background(), []byte(`{"reason":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if cog.additional != requestMoreTurnsDefaultIncrement {
		t.Errorf("additional = %d, want default %d", cog.additional, requestMoreTurnsDefaultIncrement)
	}
}

func TestRequestMoreTurns_ClampsAtMaxIncrement(t *testing.T) {
	cog := &fakeCogTurnExtender{newCap: 20}
	h := RequestMoreTurns{Cog: cog}
	if _, err := h.Execute(context.Background(), []byte(`{"additional":99,"reason":"big jump"}`)); err != nil {
		t.Fatal(err)
	}
	if cog.additional != requestMoreTurnsMaxIncrement {
		t.Errorf("additional = %d, want max %d", cog.additional, requestMoreTurnsMaxIncrement)
	}
}

func TestRequestMoreTurns_RejectsEmptyReason(t *testing.T) {
	h := RequestMoreTurns{Cog: &fakeCogTurnExtender{}}
	if _, err := h.Execute(context.Background(), []byte(`{"additional":3,"reason":""}`)); err == nil {
		t.Error("empty reason should error")
	}
	if _, err := h.Execute(context.Background(), []byte(`{"additional":3,"reason":"   "}`)); err == nil {
		t.Error("whitespace reason should error")
	}
}

func TestRequestMoreTurns_RejectsEmptyInput(t *testing.T) {
	h := RequestMoreTurns{Cog: &fakeCogTurnExtender{}}
	if _, err := h.Execute(context.Background(), nil); err == nil {
		t.Error("nil input should error")
	}
}

func TestRequestMoreTurns_RejectsMalformedInput(t *testing.T) {
	h := RequestMoreTurns{Cog: &fakeCogTurnExtender{}}
	if _, err := h.Execute(context.Background(), []byte(`{not json`)); err == nil {
		t.Error("malformed input should error")
	}
}

func TestRequestMoreTurns_RejectsNilCog(t *testing.T) {
	h := RequestMoreTurns{}
	if _, err := h.Execute(context.Background(), []byte(`{"reason":"x"}`)); err == nil {
		t.Error("nil cog should error")
	}
}

func TestRequestMoreTurns_PropagatesCogError(t *testing.T) {
	cog := &fakeCogTurnExtender{err: errors.New("at hard cap")}
	h := RequestMoreTurns{Cog: cog}
	_, err := h.Execute(context.Background(), []byte(`{"reason":"test"}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "at hard cap") {
		t.Errorf("err = %v, want cog error to propagate", err)
	}
}
