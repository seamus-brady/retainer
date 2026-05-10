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

// HTTP-level error paths shared by Chat + ChatStructured on both providers.
// Without these, the bottom of each method's error ladder went untested.

func TestAnthropic_Chat_Non2xxStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	_, err := a.Chat(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("err = %v, want 400", err)
	}
}

func TestAnthropic_Chat_UnknownContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "image", "text": "x"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 0, "output_tokens": 0}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	_, err := a.Chat(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err == nil || !strings.Contains(err.Error(), "unknown response content type") {
		t.Fatalf("err = %v, want unknown-type error", err)
	}
}

func TestAnthropic_Chat_TransportError(t *testing.T) {
	a := NewAnthropic("k", "claude-test", 2048)
	a.endpointOverride = "http://127.0.0.1:1" // unreachable
	_, err := a.Chat(context.Background(), Request{Messages: []Message{UserText("hi")}})
	if err == nil || !strings.Contains(err.Error(), "http:") {
		t.Fatalf("err = %v, want transport error", err)
	}
}

func TestAnthropic_Chat_ExtendedThinkingBumpsMaxTokens(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 0, "output_tokens": 0}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	_, err := a.Chat(context.Background(), Request{
		Messages:  []Message{UserText("hi")},
		Effort:    EffortExtended,
		MaxTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	thinking, ok := seen["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("thinking field missing: %+v", seen)
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking.type = %v", thinking["type"])
	}
	// Extended adds the thinking budget on top of the requested max_tokens.
	if int(seen["max_tokens"].(float64)) != 1000+anthropicThinkingBudget {
		t.Errorf("max_tokens = %v, want %d", seen["max_tokens"], 1000+anthropicThinkingBudget)
	}
}





func TestAnthropic_ChatStructured_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"oops"}`))
	}))
	defer srv.Close()
	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL
	var dst verdictStruct
	_, err := a.ChatStructured(context.Background(), Request{Messages: []Message{UserText("hi")}}, verdictSchema, &dst)
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropic_ChatStructured_TransportError(t *testing.T) {
	a := NewAnthropic("k", "x", 1)
	a.endpointOverride = "http://127.0.0.1:1"
	var dst verdictStruct
	_, err := a.ChatStructured(context.Background(), Request{Messages: []Message{UserText("hi")}}, verdictSchema, &dst)
	if err == nil || !strings.Contains(err.Error(), "http:") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropic_ChatStructured_BadToolInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		// Tool input with the wrong shape — verdict expects strings, not int.
		_, _ = w.Write([]byte(`{
			"content": [{"type": "tool_use", "id": "x", "name": "verdict", "input": {"verdict": 42}}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 0, "output_tokens": 0}
		}`))
	}))
	defer srv.Close()
	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL
	var dst verdictStruct
	_, err := a.ChatStructured(context.Background(), Request{Messages: []Message{UserText("hi")}}, verdictSchema, &dst)
	if err == nil || !strings.Contains(err.Error(), "decode tool_use input") {
		t.Fatalf("err = %v", err)
	}
}







// Marker-method coverage: isContent() is the interface seal. It's never
// invoked at runtime — type switches don't call it — but we test it
// directly so the coverage tool stops flagging it. The point is to assert
// that every ContentBlock satisfies the interface.
func TestContentBlockMarkers(t *testing.T) {
	var _ ContentBlock = TextBlock{}
	var _ ContentBlock = ThinkingBlock{}
	var _ ContentBlock = ToolUseBlock{}
	var _ ContentBlock = ToolResultBlock{}
	// Exercise the bodies for coverage's sake.
	TextBlock{}.isContent()
	ThinkingBlock{}.isContent()
	ToolUseBlock{}.isContent()
	ToolResultBlock{}.isContent()
}
