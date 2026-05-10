package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// Both providers reject `{"type":"text"}` (no text field) on assistant
// content. Mistral 422s with "Input should be a valid string"; Anthropic
// also errors. Empty TextBlocks must be dropped at encode time so a model
// reply that pairs an empty leading text block with a tool_use round-trips
// cleanly into the next request.


func TestAnthropicEncoder_DropsEmptyAssistantText(t *testing.T) {
	msgs := []Message{
		UserText("search please"),
		{Role: RoleAssistant, Content: []ContentBlock{
			TextBlock{Text: ""},
			ToolUseBlock{ID: "call_1", Name: "echo", Input: []byte(`{"q":"hi"}`)},
		}},
	}
	wire, err := encodeMessages(msgs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(wire) != 2 {
		t.Fatalf("wire len = %d, want 2", len(wire))
	}
	asst := wire[1]
	if len(asst.Content) != 1 {
		t.Fatalf("assistant blocks = %d, want 1 (only tool_use)", len(asst.Content))
	}
	if asst.Content[0].Type != "tool_use" {
		t.Errorf("kept block type = %q, want tool_use", asst.Content[0].Type)
	}
	body, _ := json.Marshal(asst)
	if strings.Contains(string(body), `{"type":"text"}`) {
		t.Errorf("wire JSON contains bare text part: %s", body)
	}
}







// ---- decodeMistralContent ----
//
// 2026-05-04: prior version of this test pinned the OPPOSITE
// behaviour ("unknown types error"). That codified a bug — when
// Mistral emits a `reference` content part alongside text (its
// search-augmented response shape), the adapter erroring crashed
// the agent's react loop after 3 retries and the cog saw a hard
// agent-failure. The decoder now skips unknowns + warn-logs;
// these tests pin the new shape.











func TestEncodeMessages_Anthropic_RejectsUnknownBlock(t *testing.T) {
	_, err := encodeMessages([]Message{
		{Role: RoleAssistant, Content: []ContentBlock{toolResultButOnAssistantPlaceholder{}}},
	})
	if err == nil || !strings.Contains(err.Error(), "no wire mapping for content block") {
		t.Fatalf("err = %v, want unknown-block error", err)
	}
}

// toolResultButOnAssistantPlaceholder is a stand-in unknown ContentBlock —
// outside the package nothing else can satisfy ContentBlock, so we make a
// local one that does. Using TextBlock/ThinkingBlock won't trip the default
// arm because they have wire mappings.
type toolResultButOnAssistantPlaceholder struct{}

func (toolResultButOnAssistantPlaceholder) isContent() {}

func TestEncodeMessages_Anthropic_EncodesThinking(t *testing.T) {
	out, err := encodeMessages([]Message{
		UserText("hi"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ThinkingBlock{Text: "deliberation", Signature: "sig"},
			TextBlock{Text: "answer"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	asst := out[1]
	if len(asst.Content) != 2 {
		t.Fatalf("blocks = %d, want 2", len(asst.Content))
	}
	if asst.Content[0].Type != "thinking" || asst.Content[0].Thinking != "deliberation" || asst.Content[0].Signature != "sig" {
		t.Errorf("thinking block = %+v", asst.Content[0])
	}
}

func TestEncodeMessages_Anthropic_EncodesToolResult(t *testing.T) {
	out, err := encodeMessages([]Message{
		UserText("first"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ToolUseBlock{ID: "t1", Name: "echo", Input: []byte(`{"q":"hi"}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{
			ToolResultBlock{ToolUseID: "t1", Content: "ok", IsError: false},
			ToolResultBlock{ToolUseID: "t2", Content: "boom", IsError: true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	usr := out[2]
	if len(usr.Content) != 2 {
		t.Fatalf("user blocks = %d", len(usr.Content))
	}
	if usr.Content[0].Type != "tool_result" || usr.Content[0].ToolUseID != "t1" || usr.Content[0].Content != "ok" {
		t.Errorf("[0] = %+v", usr.Content[0])
	}
	if !usr.Content[1].IsError {
		t.Errorf("[1].IsError = false, want true")
	}
}

func TestEncodeMessages_Anthropic_DefaultsToolUseInputToObject(t *testing.T) {
	// Empty Input must serialise as `{}` not as nothing — Anthropic
	// requires an object on tool_use.
	out, err := encodeMessages([]Message{
		UserText("first"),
		{Role: RoleAssistant, Content: []ContentBlock{
			ToolUseBlock{ID: "t1", Name: "echo"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[1].Content[0].Input) != "{}" {
		t.Errorf("input = %q, want {}", string(out[1].Content[0].Input))
	}
}
