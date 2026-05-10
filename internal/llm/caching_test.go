package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SystemPrompt helper tests — IsZero and Concat are the small surface
// adapters use to choose between string-form and array-form system fields.

func TestSystemPrompt_IsZero(t *testing.T) {
	cases := []struct {
		name string
		sp   SystemPrompt
		want bool
	}{
		{"both empty", SystemPrompt{}, true},
		{"stable only", SystemPrompt{Stable: "x"}, false},
		{"dynamic only", SystemPrompt{Dynamic: "y"}, false},
		{"both set", SystemPrompt{Stable: "x", Dynamic: "y"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sp.IsZero(); got != tc.want {
				t.Errorf("IsZero = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSystemPrompt_Concat(t *testing.T) {
	cases := []struct {
		name string
		sp   SystemPrompt
		want string
	}{
		{"both empty", SystemPrompt{}, ""},
		{"stable only", SystemPrompt{Stable: "alpha"}, "alpha"},
		{"dynamic only", SystemPrompt{Dynamic: "beta"}, "beta"},
		{"both", SystemPrompt{Stable: "alpha", Dynamic: "beta"}, "alpha\n\nbeta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sp.Concat(); got != tc.want {
				t.Errorf("Concat = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- Anthropic: structured SystemPrompt produces array-form system ----

func TestAnthropic_SystemPrompt_ArrayFormWithCacheControl(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"text","text":"hi"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":50,"output_tokens":10,"cache_creation_input_tokens":40,"cache_read_input_tokens":0}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	resp, err := a.Chat(context.Background(), Request{
		SystemPrompt: SystemPrompt{
			Stable:  "persona text",
			Dynamic: "preamble text",
		},
		Messages: []Message{UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Wire-format check: system should be an array, not a string.
	system, ok := captured["system"].([]any)
	if !ok {
		t.Fatalf("system field is not array; got %T: %v", captured["system"], captured["system"])
	}
	if len(system) != 2 {
		t.Fatalf("system has %d blocks, want 2", len(system))
	}
	block0 := system[0].(map[string]any)
	if block0["text"] != "persona text" {
		t.Errorf("block 0 text = %v, want %q", block0["text"], "persona text")
	}
	if cc, ok := block0["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("block 0 missing cache_control={type:ephemeral}: %v", block0["cache_control"])
	}
	block1 := system[1].(map[string]any)
	if block1["text"] != "preamble text" {
		t.Errorf("block 1 text = %v, want %q", block1["text"], "preamble text")
	}
	if _, has := block1["cache_control"]; has {
		t.Errorf("block 1 should NOT have cache_control (it's the dynamic suffix)")
	}

	// Response cache-token fields surface in Usage.
	if resp.Usage.CacheCreationInputTokens != 40 {
		t.Errorf("Usage.CacheCreationInputTokens = %d, want 40", resp.Usage.CacheCreationInputTokens)
	}
	if resp.Usage.CacheReadInputTokens != 0 {
		t.Errorf("Usage.CacheReadInputTokens = %d, want 0", resp.Usage.CacheReadInputTokens)
	}
}

func TestAnthropic_LegacySystemString_PreservesStringForm(t *testing.T) {
	// When the caller uses the legacy single-string System field,
	// the wire format should send "system" as a string, not an array.
	// This preserves backward compatibility for callers that don't
	// care about caching (canary probes, ad-hoc test prompts).
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	_, err := a.Chat(context.Background(), Request{
		System:   "you are a helper",
		Messages: []Message{UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	system, ok := captured["system"].(string)
	if !ok {
		t.Fatalf("legacy System should produce string-form; got %T: %v", captured["system"], captured["system"])
	}
	if system != "you are a helper" {
		t.Errorf("system = %q, want %q", system, "you are a helper")
	}
}

func TestAnthropic_SystemPrompt_PrefersStructuredOverLegacy(t *testing.T) {
	// When both SystemPrompt and System are set, the structured form
	// wins. (Callers shouldn't set both, but we define behaviour.)
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	_, err := a.Chat(context.Background(), Request{
		System:       "legacy string (should be ignored)",
		SystemPrompt: SystemPrompt{Stable: "structured stable"},
		Messages:     []Message{UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if _, ok := captured["system"].([]any); !ok {
		t.Errorf("structured SystemPrompt should win over legacy System; got %T", captured["system"])
	}
}

// ---- Mistral: SystemPrompt concatenates into a single message ----



// ---- ChatStructured path on Anthropic ----

func TestAnthropic_ChatStructured_SystemPrompt_ArrayForm(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = r.Body.Close()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"tool_use","id":"toolu_x","name":"my_schema","input":{"value":"ok"}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":50}
		}`))
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-test", 2048, WithAnthropicHTTPClient(srv.Client()))
	a.endpointOverride = srv.URL

	var dst struct {
		Value string `json:"value"`
	}
	usage, err := a.ChatStructured(context.Background(), Request{
		SystemPrompt: SystemPrompt{Stable: "S", Dynamic: "D"},
		Messages:     []Message{UserText("go")},
	}, Schema{
		Name:       "my_schema",
		Properties: map[string]Property{"value": {Type: "string"}},
		Required:   []string{"value"},
	}, &dst)
	if err != nil {
		t.Fatalf("ChatStructured: %v", err)
	}

	if _, ok := captured["system"].([]any); !ok {
		t.Errorf("ChatStructured: structured SystemPrompt should produce array-form system; got %T", captured["system"])
	}
	if usage.CacheReadInputTokens != 50 {
		t.Errorf("usage.CacheReadInputTokens = %d, want 50", usage.CacheReadInputTokens)
	}
}
