package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests exercise the full Chat path — wire request body and response
// decoding — for tool_use loops on both providers. They'd have caught the
// Mistral 422 from a bare `{"type":"text"}` part on the assistant message,
// which type-level encoder tests alone miss.

var sampleTool = Tool{
	Name:        "echo",
	Description: "echoes the input back",
	InputSchema: Schema{
		Properties: map[string]Property{
			"q": {Type: "string", Description: "thing to echo"},
		},
		Required: []string{"q"},
	},
}

// ---- Anthropic ----

func TestAnthropic_Chat_AdvertisesTools(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 1, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	_, err := a.Chat(context.Background(), Request{
		Messages: []Message{UserText("hi")},
		Tools:    []Tool{sampleTool},
	})
	if err != nil {
		t.Fatal(err)
	}

	var wire map[string]any
	if err := json.Unmarshal(receivedBody, &wire); err != nil {
		t.Fatal(err)
	}
	tools, ok := wire["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", wire["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "echo" || tool["description"] != "echoes the input back" {
		t.Errorf("tool = %+v", tool)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("input_schema type = %v", schema["type"])
	}
}

func TestAnthropic_Chat_DecodesToolUseResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [
				{"type": "text", "text": "I'll search."},
				{"type": "tool_use", "id": "toolu_1", "name": "echo", "input": {"q": "hi"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 5, "output_tokens": 7}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	resp, err := a.Chat(context.Background(), Request{
		Messages: []Message{UserText("hi")},
		Tools:    []Tool{sampleTool},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(resp.Content))
	}
	if _, ok := resp.Content[0].(TextBlock); !ok {
		t.Errorf("[0] = %T, want TextBlock", resp.Content[0])
	}
	tu, ok := resp.Content[1].(ToolUseBlock)
	if !ok {
		t.Fatalf("[1] = %T, want ToolUseBlock", resp.Content[1])
	}
	if tu.ID != "toolu_1" || tu.Name != "echo" {
		t.Errorf("tool_use = %+v", tu)
	}
	var inputArgs map[string]any
	if err := json.Unmarshal(tu.Input, &inputArgs); err != nil {
		t.Fatalf("decode tool input: %v", err)
	}
	if inputArgs["q"] != "hi" {
		t.Errorf("input args = %+v", inputArgs)
	}
}

func TestAnthropic_Chat_RoundTripsToolUseHistory(t *testing.T) {
	// Models routinely return an empty leading text block before a
	// tool_use. The next-turn request must not carry a bare
	// `{"type":"text"}` on the assistant message — Anthropic rejects it.
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "done"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 0, "output_tokens": 0}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	msgs := []Message{
		UserText("search"),
		{Role: RoleAssistant, Content: []ContentBlock{
			TextBlock{Text: ""}, // empty leading text — must be dropped on the wire
			ToolUseBlock{ID: "toolu_1", Name: "echo", Input: []byte(`{"q":"hi"}`)},
		}},
		{Role: RoleUser, Content: []ContentBlock{
			ToolResultBlock{ToolUseID: "toolu_1", Content: "echoed: hi"},
		}},
	}

	if _, err := a.Chat(context.Background(), Request{Messages: msgs, Tools: []Tool{sampleTool}}); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(receivedBody), `{"type":"text"}`) {
		t.Errorf("wire body contains bare text part: %s", receivedBody)
	}

	var wire map[string]any
	_ = json.Unmarshal(receivedBody, &wire)
	wireMsgs, _ := wire["messages"].([]any)
	if len(wireMsgs) != 3 {
		t.Fatalf("wire messages = %d, want 3", len(wireMsgs))
	}
	asst := wireMsgs[1].(map[string]any)
	asstContent, _ := asst["content"].([]any)
	if len(asstContent) != 1 {
		t.Fatalf("assistant content blocks = %d, want 1 (only tool_use)", len(asstContent))
	}
	if asstContent[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant block type = %v", asstContent[0])
	}
	usr := wireMsgs[2].(map[string]any)
	usrContent, _ := usr["content"].([]any)
	if len(usrContent) != 1 || usrContent[0].(map[string]any)["type"] != "tool_result" {
		t.Errorf("user content = %+v", usrContent)
	}
	if usrContent[0].(map[string]any)["tool_use_id"] != "toolu_1" {
		t.Errorf("tool_use_id mismatch: %+v", usrContent[0])
	}
}

// ---- Mistral ----







// TestMistralToolFromTool_EmptyPropertiesEncodesAsObject pins the
// fix for a Mistral 400 caught by the self-diagnostic skill:
// argument-less tools (e.g. agent_scheduler's `list_jobs`) must
// serialise as `"properties": {}`, not `"properties": null`.
//
// Mistral's tool-schema validator rejects null with:
//
//	{"object":"error","message":"Invalid tool schema: None is not
//	of type 'object'","type":"invalid_request_tool_schema",...}
//
// The wire shape MUST round-trip through JSON (encoder tests on
// the Go struct alone wouldn't catch the nil → null mapping).

// TestMistralToolFromSchema_EmptyPropertiesEncodesAsObject is the
// same regression check on the structured-output path (Schema →
// mistralTool, used by ChatStructured), not just the tool-use
// path. Both routes share `mistralConvertProperties` but it's
// cheap to pin both.

func TestAnthropicToolFromTool_PreservesSchema(t *testing.T) {
	tool := Tool{
		Name:        "search",
		Description: "search",
		InputSchema: Schema{
			Properties: map[string]Property{
				"q":    {Type: "string"},
				"tags": {Type: "array", Items: &Property{Type: "string"}},
			},
			Required: []string{"q"},
		},
	}
	wire := anthropicToolFromTool(tool)
	if wire.Name != "search" {
		t.Errorf("name = %q", wire.Name)
	}
	if wire.InputSchema.Type != "object" {
		t.Errorf("schema type = %q", wire.InputSchema.Type)
	}
	if len(wire.InputSchema.Required) != 1 || wire.InputSchema.Required[0] != "q" {
		t.Errorf("required = %+v", wire.InputSchema.Required)
	}
	tagsProp, ok := wire.InputSchema.Properties["tags"]
	if !ok || tagsProp.Type != "array" || tagsProp.Items == nil || tagsProp.Items.Type != "string" {
		t.Errorf("tags prop = %+v", tagsProp)
	}
}
