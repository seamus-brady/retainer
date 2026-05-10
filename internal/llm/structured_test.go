package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type verdictStruct struct {
	Verdict   string `json:"verdict"`
	Reasoning string `json:"reasoning"`
}

var verdictSchema = Schema{
	Name:        "verdict",
	Description: "test verdict",
	Properties: map[string]Property{
		"verdict": {
			Type: "string",
			Enum: []string{"allow", "block", "escalate"},
		},
		"reasoning": {Type: "string"},
	},
	Required: []string{"verdict"},
}

// ---- Mock ----

func TestMock_ChatStructured_Success(t *testing.T) {
	m := NewMock()
	m.SetStructuredFunc(func(req Request, schema Schema) (any, error) {
		return verdictStruct{Verdict: "block", Reasoning: "test"}, nil
	})

	var got verdictStruct
	if _, err := m.ChatStructured(context.Background(), Request{}, verdictSchema, &got); err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "block" || got.Reasoning != "test" {
		t.Fatalf("got %+v", got)
	}
}

func TestMock_ChatStructured_NoFunc(t *testing.T) {
	m := NewMock()
	var got verdictStruct
	_, err := m.ChatStructured(context.Background(), Request{}, verdictSchema, &got)
	if err == nil {
		t.Fatal("expected error when no StructuredFunc set")
	}
	if !strings.Contains(err.Error(), "no StructuredFunc set") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMock_ChatStructured_FuncError(t *testing.T) {
	m := NewMock()
	sentinel := errors.New("scripted failure")
	m.SetStructuredFunc(func(req Request, schema Schema) (any, error) {
		return nil, sentinel
	})
	var got verdictStruct
	_, err := m.ChatStructured(context.Background(), Request{}, verdictSchema, &got)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

// ---- Anthropic over httptest ----

func TestAnthropic_ChatStructured_RoundTrip(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{
				"type": "tool_use",
				"id": "toolu_1",
				"name": "verdict",
				"input": {"verdict": "block", "reasoning": "obvious attack"}
			}],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 12, "output_tokens": 8}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("test-key", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL // see helper below

	var got verdictStruct
	usage, err := a.ChatStructured(context.Background(), Request{
		Messages: []Message{UserText("hello")},
	}, verdictSchema, &got)
	if err != nil {
		t.Fatal(err)
	}

	if got.Verdict != "block" || got.Reasoning != "obvious attack" {
		t.Fatalf("decoded = %+v", got)
	}
	if usage.InputTokens != 12 || usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v", usage)
	}

	// Verify the wire request includes the tool + forced tool_choice.
	var wire map[string]any
	if err := json.Unmarshal(receivedBody, &wire); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}
	tools, _ := wire["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "verdict" {
		t.Errorf("tool name = %v", tool["name"])
	}
	tc, _ := wire["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "verdict" {
		t.Errorf("tool_choice = %v", tc)
	}
}

func TestAnthropic_ChatStructured_NoMatchingToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [{"type": "text", "text": "no tool here"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 0, "output_tokens": 0}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("test-key", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	var got verdictStruct
	_, err := a.ChatStructured(context.Background(), Request{Messages: []Message{UserText("hi")}}, verdictSchema, &got)
	if err == nil || !strings.Contains(err.Error(), "no tool_use block matching") {
		t.Fatalf("err = %v, want missing-tool-use error", err)
	}
}

// ---- Mistral over httptest ----

