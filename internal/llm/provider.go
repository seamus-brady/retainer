// Package llm is the vendor-neutral seam for chat-completion providers.
//
// Concrete adapters (anthropic, mock, future: openai, vertex, ...) implement
// Provider. The cognitive loop and agent framework only ever see this
// interface. Caching, thinking blocks, tool use, and structured output will be
// added as new fields on Request/Response — adapters that don't support a
// capability ignore the field.
package llm

import "context"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn. ContentBlock is a sum type — for now only Text is
// modelled. ToolUse / ToolResult / Thinking / Image will land alongside the
// features that need them.
type Message struct {
	Role    Role
	Content []ContentBlock
}

type ContentBlock interface{ isContent() }

type TextBlock struct{ Text string }

func (TextBlock) isContent() {}

// ThinkingBlock is the extended-thinking output some providers emit (e.g.
// Anthropic when `thinking` is enabled). Signature is opaque and must be
// passed back unmodified if a subsequent turn continues a tool-use loop.
type ThinkingBlock struct {
	Text      string
	Signature string
}

func (ThinkingBlock) isContent() {}

// ToolUseBlock represents a tool call the model wants the caller to
// execute. ID is the provider-issued identifier; the next tool_result must
// reference the same ID per the four-invariant rule. Input is the raw
// JSON args the model produced — caller decodes into the tool's expected
// schema.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input []byte // raw JSON of the tool's input args
}

func (ToolUseBlock) isContent() {}

// ToolResultBlock represents the response to a tool call. Lives on user
// messages internally; adapters translate to the provider's wire shape
// (Anthropic: tool_result content block; Mistral: separate role=tool message).
type ToolResultBlock struct {
	ToolUseID string
	Content   string
	IsError   bool
}

func (ToolResultBlock) isContent() {}

// UserText / AssistantText are sugar for the common single-text-block case.
func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{TextBlock{Text: s}}}
}

func AssistantText(s string) Message {
	return Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock{Text: s}}}
}

// Effort selects between fast/standard inference and extended-reasoning mode.
// Adapters interpret it per-provider:
//   - Anthropic: Extended → `thinking: {type:"enabled", budget_tokens:N}`.
//   - Mistral:   Extended → `reasoning_effort: "high"`. Standard omits the
//     field (defaults to model's normal behavior).
//   - Models without an extended-reasoning concept (e.g. Haiku, Mistral
//     non-Small-4 models) ignore Extended; the request runs as standard.
type Effort int

const (
	EffortStandard Effort = iota
	EffortExtended
)

type Request struct {
	Model string
	// System is the legacy single-string system prompt. Adapters use this
	// when SystemPrompt is the zero value. Kept for callers that don't care
	// about prompt caching (canary probes, ad-hoc test prompts).
	System string
	// SystemPrompt is the cache-aware structured form. When set, adapters
	// use it in preference to System. The Stable half is intended to be
	// byte-identical across cycles in a session (persona, authority,
	// available_skills, bootstrap_skills); the Dynamic half is per-cycle
	// (sensorium, virtual_memory, preamble). The Anthropic adapter places
	// a cache_control marker between them so Stable hits the prompt cache.
	// Mistral and Mock concatenate the two halves and send uncached.
	SystemPrompt SystemPrompt
	Messages     []Message
	MaxTokens    int
	Effort       Effort
	// Tools is the optional list of tools the model may call. Empty means
	// the model returns text only. When non-empty, the response may contain
	// ToolUseBlock content blocks the caller dispatches and follows up with
	// a user message containing matching ToolResultBlocks.
	Tools []Tool
}

// SystemPrompt is the cache-aware structured form of a system prompt. The
// Stable half is intended to remain byte-identical across cycles in a
// session so the upstream prompt cache hits. The Dynamic half is per-cycle
// content (timestamps, queue depth, recent narrative) that changes every
// request. Anthropic adapter places a cache_control marker between them;
// other adapters concatenate.
//
// Empty value (both fields "") means the legacy Request.System string is
// used instead.
type SystemPrompt struct {
	// Stable is the cacheable prefix — persona, authority, skills lists.
	// Should be byte-identical across cycles in a session for cache hits.
	Stable string
	// Dynamic is the per-cycle suffix — sensorium, preamble, anything
	// that legitimately changes each cycle.
	Dynamic string
}

// IsZero reports whether the SystemPrompt has no content (both halves empty).
// Adapters use this to decide whether to fall back to Request.System.
func (s SystemPrompt) IsZero() bool {
	return s.Stable == "" && s.Dynamic == ""
}

// Concat returns the two halves joined as a single string with a blank
// line between them. Used by adapters that don't support cache markers
// (Mistral, Mock).
func (s SystemPrompt) Concat() string {
	switch {
	case s.Stable == "" && s.Dynamic == "":
		return ""
	case s.Stable == "":
		return s.Dynamic
	case s.Dynamic == "":
		return s.Stable
	}
	return s.Stable + "\n\n" + s.Dynamic
}

// Tool describes one tool the model can call. InputSchema reuses the
// Schema type ChatStructured uses; tool-use parsing is provider-native
// (Anthropic tool_use, Mistral OpenAI-shape tool_calls).
type Tool struct {
	Name        string
	Description string
	InputSchema Schema
}

type Response struct {
	Content    []ContentBlock
	StopReason string
	Usage      Usage
}

// Text returns the concatenation of all TextBlocks in Content. Convenience for
// the common "just give me the reply string" path.
func (r Response) Text() string {
	var out string
	for _, b := range r.Content {
		if t, ok := b.(TextBlock); ok {
			out += t.Text
		}
	}
	return out
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	// CacheCreationInputTokens reports tokens that were just written into
	// the upstream prompt cache (charged at a different rate than read /
	// uncached input on Anthropic). Zero for providers that don't expose
	// caching telemetry.
	CacheCreationInputTokens int
	// CacheReadInputTokens reports tokens that hit the upstream prompt
	// cache (charged at the cached rate). Zero for providers that don't
	// expose caching telemetry.
	CacheReadInputTokens int
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, req Request) (Response, error)
	// ChatStructured forces the provider to respond via tool-use against the
	// given schema and decodes the resulting tool-call args into dst (which
	// must be a pointer to a struct or map). Returns Usage for telemetry,
	// or an error if the API call failed or the response wasn't a tool-call
	// matching the schema name.
	ChatStructured(ctx context.Context, req Request, schema Schema, dst any) (Usage, error)
}

// Schema is a cross-provider description of a structured-output target.
// Adapters translate this to their native tool/function format. Subset of
// JSON Schema sufficient for our use cases — extend as needed.
type Schema struct {
	Name        string
	Description string
	Properties  map[string]Property
	Required    []string
}

// Property describes one field in a schema. Type is the JSON-schema type
// name: "string", "number", "integer", "boolean", "array", "object".
type Property struct {
	Type        string
	Description string
	Enum        []string             // optional; only for string types
	Items       *Property            // required when Type == "array"
	Properties  map[string]Property  // required when Type == "object"
	Required    []string             // for nested objects
}

