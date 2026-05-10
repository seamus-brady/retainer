package cog

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestFormatErrorForUser_NeverEmptyForNonNil(t *testing.T) {
	cases := []error{
		errors.New(""),
		errors.New("anything"),
		errors.New("mistral: status 429: rate limited"),
		errors.New("anthropic: status 401: unauthorized"),
		errors.New("dial tcp: connection refused"),
		context.Canceled,
		context.DeadlineExceeded,
	}
	for _, e := range cases {
		got := FormatErrorForUser(e)
		if got == "" {
			t.Errorf("FormatErrorForUser(%v) returned empty string", e)
		}
	}
}

func TestFormatErrorForUser_NilReturnsEmpty(t *testing.T) {
	if got := FormatErrorForUser(nil); got != "" {
		t.Errorf("nil → %q, want empty", got)
	}
}

func TestFormatErrorForUser_NeverLeaksProviderInternals(t *testing.T) {
	// The whole point of this helper. Provider names, status
	// codes, and raw JSON should never appear in user-facing
	// strings. Test against every category's representative
	// error to pin the contract.
	leakyTokens := []string{
		"mistral", "anthropic", "openai", "openrouter", "vertex",
		"status 4", "status 5", "status 2",
		`{"object":`, `"error":`, `"code":`,
		"raw_status_code", "rate_limited", "type:",
	}
	cases := []error{
		errors.New(`mistral: status 429: {"object":"error","message":"Rate limit exceeded","type":"rate_limited","param":null,"code":"1300","raw_status_code":429}`),
		errors.New(`anthropic: status 401: {"error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
		errors.New("anthropic: status 529: overloaded"),
		errors.New("anthropic: status 502: bad gateway"),
		errors.New("dial tcp 1.2.3.4:443: connection refused"),
		errors.New("anthropic: status 422: invalid request: tool schema"),
		errors.New("cog: tool loop exceeded max_tool_turns=10"),
	}
	for _, e := range cases {
		got := strings.ToLower(FormatErrorForUser(e))
		for _, leak := range leakyTokens {
			if strings.Contains(got, leak) {
				t.Errorf("error %q produced user-facing string %q which contains leaked token %q",
					e.Error(), got, leak)
			}
		}
	}
}

func TestClassifyError_Categories(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrCategory
	}{
		{"nil", nil, ErrCategoryUnknown},
		{"context cancelled", context.Canceled, ErrCategoryCancelled},
		{"context deadline", context.DeadlineExceeded, ErrCategoryTimeout},
		{"watchdog", errors.New("cog: watchdog fired waiting for Thinking"), ErrCategoryTimeout},
		{"tool budget", errors.New("cog: tool loop exceeded max_tool_turns=10"), ErrCategoryToolBudget},
		{"mistral 429", errors.New(`mistral: status 429: {"message":"Rate limit exceeded"}`), ErrCategoryRateLimited},
		{"too many requests", errors.New("server returned too many requests"), ErrCategoryRateLimited},
		{"rate limit phrase", errors.New("provider: rate limit hit"), ErrCategoryRateLimited},
		{"anthropic 529", errors.New("anthropic: status 529: overloaded"), ErrCategoryOverloaded},
		{"401", errors.New("anthropic: status 401: invalid x-api-key"), ErrCategoryAuth},
		{"forbidden", errors.New("forbidden: missing scope"), ErrCategoryAuth},
		{"400", errors.New("anthropic: status 400: bad request"), ErrCategoryBadRequest},
		{"503", errors.New("provider: status 503: service unavailable"), ErrCategoryServerError},
		{"network err", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, ErrCategoryTransport},
		{"connection refused phrase", errors.New("dial tcp: connection refused"), ErrCategoryTransport},
		{"unknown", errors.New("something weird happened"), ErrCategoryUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyError(c.err); got != c.want {
				t.Errorf("ClassifyError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestFormatErrorForUser_RateLimitedSentence(t *testing.T) {
	// Pin the wording for the most operator-relevant category —
	// a future edit shouldn't accidentally drop the "try again
	// in a minute" hint or substitute jargon.
	got := FormatErrorForUser(errors.New(`mistral: status 429: {"message":"Rate limit exceeded"}`))
	for _, want := range []string{"rate limit", "try again"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("rate-limit message %q missing %q", got, want)
		}
	}
}
