package llm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- ScriptedMock ----

func TestScriptedMock_TextResponse(t *testing.T) {
	m := NewScriptedMock([]ScriptedResponse{
		{Text: "hello back"},
	})
	resp, err := m.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if got := textBlocks(resp); got != "hello back" {
		t.Errorf("text = %q, want %q", got, "hello back")
	}
}

func TestScriptedMock_ToolUseResponse(t *testing.T) {
	m := NewScriptedMock([]ScriptedResponse{
		{
			ToolUses: []ScriptedToolUse{
				{Name: "memory_write", Input: map[string]string{"key": "user.name", "value": "Seamus"}},
			},
		},
	})
	resp, err := m.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(resp.Content))
	}
	tu, ok := resp.Content[0].(ToolUseBlock)
	if !ok {
		t.Fatalf("got %T, want ToolUseBlock", resp.Content[0])
	}
	if tu.Name != "memory_write" {
		t.Errorf("Name = %q, want memory_write", tu.Name)
	}
	if tu.ID == "" {
		t.Error("ID auto-assignment empty")
	}
	var inputMap map[string]string
	if err := json.Unmarshal(tu.Input, &inputMap); err != nil {
		t.Fatalf("input not valid JSON: %v", err)
	}
	if inputMap["key"] != "user.name" || inputMap["value"] != "Seamus" {
		t.Errorf("input wrong: %+v", inputMap)
	}
}

func TestScriptedMock_SequenceAdvances(t *testing.T) {
	m := NewScriptedMock([]ScriptedResponse{
		{Text: "first"},
		{Text: "second"},
	})
	r1, _ := m.Chat(context.Background(), Request{})
	r2, _ := m.Chat(context.Background(), Request{})
	if textBlocks(r1) != "first" {
		t.Errorf("call 1 = %q", textBlocks(r1))
	}
	if textBlocks(r2) != "second" {
		t.Errorf("call 2 = %q", textBlocks(r2))
	}
}

func TestScriptedMock_ExhaustedReturnsError(t *testing.T) {
	m := NewScriptedMock([]ScriptedResponse{{Text: "only"}})
	if _, err := m.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := m.Chat(context.Background(), Request{}); err == nil {
		t.Error("second call should error after exhaustion")
	}
}

func TestScriptedMock_PreservesExplicitToolID(t *testing.T) {
	m := NewScriptedMock([]ScriptedResponse{
		{ToolUses: []ScriptedToolUse{{ID: "custom-id", Name: "x"}}},
	})
	r, _ := m.Chat(context.Background(), Request{})
	tu := r.Content[0].(ToolUseBlock)
	if tu.ID != "custom-id" {
		t.Errorf("ID = %q, want custom-id", tu.ID)
	}
}

func TestScriptedMock_TextAndToolUseTogether(t *testing.T) {
	// Anthropic emits both when the model says "I'll use this tool"
	// before the tool_use block. The scripted mock should preserve
	// both in declaration order: tool_uses first, text last.
	m := NewScriptedMock([]ScriptedResponse{
		{
			ToolUses: []ScriptedToolUse{{Name: "x"}},
			Text:     "thinking out loud",
		},
	})
	r, _ := m.Chat(context.Background(), Request{})
	if len(r.Content) != 2 {
		t.Fatalf("got %d blocks, want 2", len(r.Content))
	}
	if _, ok := r.Content[0].(ToolUseBlock); !ok {
		t.Errorf("block 0 = %T, want ToolUseBlock", r.Content[0])
	}
	if _, ok := r.Content[1].(TextBlock); !ok {
		t.Errorf("block 1 = %T, want TextBlock", r.Content[1])
	}
	if r.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use (tool_uses present)", r.StopReason)
	}
}

func TestScriptedMock_StructuredCallErrors(t *testing.T) {
	m := NewScriptedMock([]ScriptedResponse{{Text: "x"}})
	if _, err := m.ChatStructured(context.Background(), Request{}, Schema{Name: "v"}, &struct{}{}); err == nil {
		t.Error("ChatStructured should error on scripted mock")
	}
}

// ---- LoadScriptedMock ----

func TestLoadScriptedMock_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "script.json")
	body := `{
		"responses": [
			{"text": "first"},
			{"tool_uses": [{"name": "memory_write", "input": {"k": "v"}}]}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadScriptedMock(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r, _ := m.Chat(context.Background(), Request{}); textBlocks(r) != "first" {
		t.Errorf("first call wrong: %+v", r)
	}
	if r, _ := m.Chat(context.Background(), Request{}); r.StopReason != "tool_use" {
		t.Errorf("second call should be tool_use; got %+v", r)
	}
}

func TestLoadScriptedMock_MissingFileErrors(t *testing.T) {
	if _, err := LoadScriptedMock("/no/such/file.json"); err == nil {
		t.Error("missing file should error")
	}
}

func TestLoadScriptedMock_MalformedJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScriptedMock(path); err == nil {
		t.Error("malformed JSON should error")
	}
}

func TestLoadScriptedMock_EmptyResponsesErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte(`{"responses":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadScriptedMock(path); err == nil {
		t.Error("zero responses should error (silent zero-script masks test setup bugs)")
	}
}

// ---- helpers ----

func textBlocks(r Response) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
