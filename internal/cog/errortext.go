package cog

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// FormatErrorForUser turns a backend error (provider 4xx/5xx,
// transport failure, watchdog timeout, etc.) into a calm,
// operator-readable sentence. The raw error always lands in
// slog + cycle-log via abandonCycle's logger; this is what the
// chat surface (TUI / webui) shows to a human.
//
// Discipline:
//
//   - No provider names ("mistral", "anthropic", "openai") —
//     they're internal substrate and noisy to the operator.
//   - No status codes — "429" / "503" mean nothing to a
//     non-engineer. Categorise instead.
//   - No raw JSON — the operator should never see
//     `{"object":"error","message":"…"}` rendered as chat.
//   - No stack traces, no file:line, no underlying error
//     wrappers exposed.
//   - One sentence; the next-step hint goes in a second
//     sentence only when there's something useful to say.
//
// Returns a non-empty string for every non-nil error. Returns
// "" when err is nil.
func FormatErrorForUser(err error) string {
	if err == nil {
		return ""
	}
	switch ClassifyError(err) {
	case ErrCategoryRateLimited:
		return "I hit a rate limit and couldn't recover after retries. Try again in a minute."
	case ErrCategoryOverloaded:
		return "The model service is overloaded right now. Try again in a moment."
	case ErrCategoryTransport:
		return "I couldn't reach the model service. Check the network and try again."
	case ErrCategoryAuth:
		return "The model API key was rejected. Check the configured credentials."
	case ErrCategoryBadRequest:
		return "The model rejected the request. This is usually a Retainer bug — check the cycle log for details."
	case ErrCategoryServerError:
		return "The model service returned an unexpected error. Try again; if it persists, check the provider status page."
	case ErrCategoryTimeout:
		return "The cycle timed out before completing. Try again, or break the request into smaller steps."
	case ErrCategoryCancelled:
		return "The cycle was cancelled before it could finish."
	case ErrCategoryToolBudget:
		return "I ran out of tool-call turns within the cycle. Try a narrower request, or extend the budget if this is genuinely a multi-step problem."
	}
	// Unclassified — generic message. Don't surface the raw err
	// even here; the operator can grep slog if they want
	// internals. Avoid the "mistral: status 429" leak that
	// motivated this helper.
	return "Something went wrong inside the cycle. The full error is in the system log."
}

// ErrCategory is the coarse classification ClassifyError
// returns. New categories should match a matching branch in
// FormatErrorForUser.
type ErrCategory int

const (
	ErrCategoryUnknown ErrCategory = iota
	ErrCategoryRateLimited
	ErrCategoryOverloaded
	ErrCategoryTransport
	ErrCategoryAuth
	ErrCategoryBadRequest
	ErrCategoryServerError
	ErrCategoryTimeout
	ErrCategoryCancelled
	ErrCategoryToolBudget
)

// ClassifyError categorises an error for FormatErrorForUser.
// Order matters: more specific categories win. Heuristics
// match the same patterns the retry wrapper uses, so what
// retries-and-fails maps to a Rate-limited / Overloaded /
// Transport message rather than an Unknown one.
func ClassifyError(err error) ErrCategory {
	if err == nil {
		return ErrCategoryUnknown
	}
	if errors.Is(err, context.Canceled) {
		return ErrCategoryCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrCategoryTimeout
	}

	msg := strings.ToLower(err.Error())

	// Cog-internal: tool-budget exhaustion has a stable phrase.
	if strings.Contains(msg, "max_tool_turns") || strings.Contains(msg, "tool loop exceeded") {
		return ErrCategoryToolBudget
	}
	// Watchdog / internal timeouts.
	if strings.Contains(msg, "watchdog") || strings.Contains(msg, "timed out") {
		return ErrCategoryTimeout
	}

	// Provider-shaped errors. The retry wrapper already classifies
	// these heuristically (see internal/llm/retry.go); we mirror
	// the same patterns so retried-and-exhausted errors land in
	// matching categories rather than Unknown.
	if containsAny(msg, "status 429", "too many requests", "rate limit", "rate-limit", "rate_limit") {
		return ErrCategoryRateLimited
	}
	if containsAny(msg, "status 529", "overloaded") {
		return ErrCategoryOverloaded
	}
	if containsAny(msg, "status 401", "status 403", "unauthorized", "unauthorised", "forbidden", "invalid api key", "invalid_api_key") {
		return ErrCategoryAuth
	}
	if containsAny(msg, "status 400", "status 404", "status 422", "bad request", "invalid request") {
		return ErrCategoryBadRequest
	}
	if containsAny(msg, "status 500", "status 502", "status 503", "status 504") {
		return ErrCategoryServerError
	}

	// Transport failures — DNS, connection refused, TLS, EOF.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrCategoryTransport
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return ErrCategoryTransport
	}
	if containsAny(msg, "no such host", "connection refused", "connection reset", "tls handshake", "broken pipe") {
		return ErrCategoryTransport
	}

	return ErrCategoryUnknown
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
