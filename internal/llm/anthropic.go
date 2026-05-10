package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	anthropicEndpoint       = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion     = "2023-06-01"
	anthropicThinkingBudget = 4096 // tokens reserved for extended thinking
)

// Anthropic talks to the Messages API directly over HTTP. We avoid the SDK so
// we can layer prompt caching, thinking blocks, and streaming on the same
// transport later (per springdrift-arch-notes.md §LLM).
type Anthropic struct {
	apiKey           string
	model            string
	maxTokens        int
	httpClient       *http.Client
	endpointOverride string // tests only; empty uses anthropicEndpoint
}

func (a *Anthropic) endpoint() string {
	if a.endpointOverride != "" {
		return a.endpointOverride
	}
	return anthropicEndpoint
}

type AnthropicOption func(*Anthropic)

func WithAnthropicHTTPClient(c *http.Client) AnthropicOption {
	return func(a *Anthropic) { a.httpClient = c }
}

func NewAnthropic(apiKey, model string, maxTokens int, opts ...AnthropicOption) *Anthropic {
	a := &Anthropic{
		apiKey:     apiKey,
		model:      model,
		maxTokens:  maxTokens,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func (*Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Chat(ctx context.Context, req Request) (Response, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	outputBudget := req.MaxTokens
	if outputBudget == 0 {
		outputBudget = a.maxTokens
	}

	encodedMessages, err := encodeMessages(req.Messages)
	if err != nil {
		return Response{}, err
	}

	wireReq := anthropicRequest{
		Model:     model,
		MaxTokens: outputBudget,
		System:    buildAnthropicSystem(req.System, req.SystemPrompt),
		Messages:  encodedMessages,
	}
	if req.Effort == EffortExtended {
		// Anthropic requires max_tokens > budget_tokens; the budget covers
		// the thinking output, the remainder is left for the visible reply.
		wireReq.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: anthropicThinkingBudget}
		wireReq.MaxTokens = outputBudget + anthropicThinkingBudget
	}
	if len(req.Tools) > 0 {
		wireReq.Tools = make([]anthropicTool, len(req.Tools))
		for i, t := range req.Tools {
			wireReq.Tools[i] = anthropicToolFromTool(t)
		}
	}

	body, err := json.Marshal(wireReq)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(), bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, string(raw))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return Response{}, fmt.Errorf("anthropic: decode: %w", err)
	}

	out := Response{StopReason: ar.StopReason}
	for _, b := range ar.Content {
		switch b.Type {
		case "text":
			out.Content = append(out.Content, TextBlock{Text: b.Text})
		case "thinking":
			out.Content = append(out.Content, ThinkingBlock{Text: b.Thinking, Signature: b.Signature})
		case "tool_use":
			out.Content = append(out.Content, ToolUseBlock{ID: b.ID, Name: b.Name, Input: append([]byte(nil), b.Input...)})
		default:
			return Response{}, fmt.Errorf("anthropic: unknown response content type %q", b.Type)
		}
	}
	out.Usage.InputTokens = ar.Usage.InputTokens
	out.Usage.OutputTokens = ar.Usage.OutputTokens
	out.Usage.CacheCreationInputTokens = ar.Usage.CacheCreationInputTokens
	out.Usage.CacheReadInputTokens = ar.Usage.CacheReadInputTokens
	return out, nil
}

// anthropicRequest mirrors Anthropic's /v1/messages request shape. The
// System field is `any` because Anthropic accepts both a single string
// and an array of typed text blocks; we use the array form when prompt
// caching is configured (see anthropicSystemBlock + cache_control), and
// the string form otherwise.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    any                `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Thinking  *anthropicThinking `json:"thinking,omitempty"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

// anthropicSystemBlock is one entry in the array-form system field. The
// stable prefix gets a cache_control marker; the dynamic suffix doesn't.
type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl marks a block as cache-eligible. Currently
// Anthropic only supports "ephemeral" (~5min TTL).
type anthropicCacheControl struct {
	Type string `json:"type"`
}

// buildAnthropicSystem chooses string-form or array-form for the system
// field based on whether structured caching is requested. Returns nil
// when both legacy System and SystemPrompt are empty.
func buildAnthropicSystem(legacy string, sp SystemPrompt) any {
	if !sp.IsZero() {
		var blocks []anthropicSystemBlock
		if sp.Stable != "" {
			blocks = append(blocks, anthropicSystemBlock{
				Type:         "text",
				Text:         sp.Stable,
				CacheControl: &anthropicCacheControl{Type: "ephemeral"},
			})
		}
		if sp.Dynamic != "" {
			blocks = append(blocks, anthropicSystemBlock{
				Type: "text",
				Text: sp.Dynamic,
			})
		}
		return blocks
	}
	if legacy != "" {
		return legacy
	}
	return nil
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicResponse struct {
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

// anthropicUsage carries token counts including the prompt-cache fields
// when caching is in use. The cache fields are zero on responses that
// didn't involve caching (legacy string-form system, cache miss for
// other reasons, etc.).
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// ChatStructured forces a single tool-use call against the given schema and
// decodes the resulting input JSON into dst. Returns Usage for telemetry.
func (a *Anthropic) ChatStructured(ctx context.Context, req Request, schema Schema, dst any) (Usage, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	outputBudget := req.MaxTokens
	if outputBudget == 0 {
		outputBudget = a.maxTokens
	}

	encodedMessages, err := encodeMessages(req.Messages)
	if err != nil {
		return Usage{}, err
	}

	wireReq := anthropicStructuredRequest{
		Model:      model,
		MaxTokens:  outputBudget,
		System:     buildAnthropicSystem(req.System, req.SystemPrompt),
		Messages:   encodedMessages,
		Tools:      []anthropicTool{anthropicToolFromSchema(schema)},
		ToolChoice: &anthropicToolChoice{Type: "tool", Name: schema.Name},
	}
	if req.Effort == EffortExtended {
		wireReq.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: anthropicThinkingBudget}
		wireReq.MaxTokens = outputBudget + anthropicThinkingBudget
	}

	body, err := json.Marshal(wireReq)
	if err != nil {
		return Usage{}, fmt.Errorf("anthropic: marshal structured request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(), bytes.NewReader(body))
	if err != nil {
		return Usage{}, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return Usage{}, fmt.Errorf("anthropic: http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Usage{}, fmt.Errorf("anthropic: read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Usage{}, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, string(raw))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return Usage{}, fmt.Errorf("anthropic: decode: %w", err)
	}

	for _, b := range ar.Content {
		if b.Type != "tool_use" || b.Name != schema.Name {
			continue
		}
		if err := json.Unmarshal(b.Input, dst); err != nil {
			return Usage{}, fmt.Errorf("anthropic: decode tool_use input: %w", err)
		}
		return Usage{
			InputTokens:              ar.Usage.InputTokens,
			OutputTokens:             ar.Usage.OutputTokens,
			CacheCreationInputTokens: ar.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     ar.Usage.CacheReadInputTokens,
		}, nil
	}
	return Usage{}, fmt.Errorf("anthropic: no tool_use block matching %q in response", schema.Name)
}

type anthropicStructuredRequest struct {
	Model      string               `json:"model"`
	MaxTokens  int                  `json:"max_tokens"`
	System     any                  `json:"system,omitempty"` // string or []anthropicSystemBlock
	Messages   []anthropicMessage   `json:"messages"`
	Thinking   *anthropicThinking   `json:"thinking,omitempty"`
	Tools      []anthropicTool      `json:"tools"`
	ToolChoice *anthropicToolChoice `json:"tool_choice"`
}

type anthropicTool struct {
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	InputSchema anthropicInputSchema `json:"input_schema"`
}

type anthropicInputSchema struct {
	Type       string                       `json:"type"`
	Properties map[string]anthropicProperty `json:"properties"`
	Required   []string                     `json:"required,omitempty"`
}

type anthropicProperty struct {
	Type        string                       `json:"type"`
	Description string                       `json:"description,omitempty"`
	Enum        []string                     `json:"enum,omitempty"`
	Items       *anthropicProperty           `json:"items,omitempty"`
	Properties  map[string]anthropicProperty `json:"properties,omitempty"`
	Required    []string                     `json:"required,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func anthropicToolFromSchema(s Schema) anthropicTool {
	return anthropicTool{
		Name:        s.Name,
		Description: s.Description,
		InputSchema: anthropicInputSchema{
			Type:       "object",
			Properties: anthropicConvertProperties(s.Properties),
			Required:   s.Required,
		},
	}
}

func anthropicConvertProperties(props map[string]Property) map[string]anthropicProperty {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]anthropicProperty, len(props))
	for k, v := range props {
		out[k] = anthropicConvertProperty(v)
	}
	return out
}

func anthropicConvertProperty(p Property) anthropicProperty {
	out := anthropicProperty{
		Type:        p.Type,
		Description: p.Description,
		Enum:        p.Enum,
		Required:    p.Required,
	}
	if p.Items != nil {
		item := anthropicConvertProperty(*p.Items)
		out.Items = &item
	}
	if p.Properties != nil {
		out.Properties = anthropicConvertProperties(p.Properties)
	}
	return out
}

func encodeMessages(ms []Message) ([]anthropicMessage, error) {
	out := make([]anthropicMessage, 0, len(ms))
	for _, m := range ms {
		am := anthropicMessage{Role: string(m.Role)}
		for _, b := range m.Content {
			switch v := b.(type) {
			case TextBlock:
				if v.Text == "" {
					continue
				}
				am.Content = append(am.Content, anthropicContent{Type: "text", Text: v.Text})
			case ThinkingBlock:
				am.Content = append(am.Content, anthropicContent{Type: "thinking", Thinking: v.Text, Signature: v.Signature})
			case ToolUseBlock:
				input := json.RawMessage(v.Input)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				am.Content = append(am.Content, anthropicContent{Type: "tool_use", ID: v.ID, Name: v.Name, Input: input})
			case ToolResultBlock:
				am.Content = append(am.Content, anthropicContent{Type: "tool_result", ToolUseID: v.ToolUseID, Content: v.Content, IsError: v.IsError})
			default:
				return nil, fmt.Errorf("anthropic: no wire mapping for content block %T", v)
			}
		}
		out = append(out, am)
	}
	return out, nil
}

func anthropicToolFromTool(t Tool) anthropicTool {
	return anthropicTool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: anthropicInputSchema{
			Type:       "object",
			Properties: anthropicConvertProperties(t.InputSchema.Properties),
			Required:   t.InputSchema.Required,
		},
	}
}
