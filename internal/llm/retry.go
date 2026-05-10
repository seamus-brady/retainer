package llm

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"strings"
	"time"
)

// RetryConfig controls the LLM retry wrapper. All durations clamp to
// [0, MaxDelay] after computation. Zero values pick sane defaults so
// callers can wire the wrapper with `RetryConfig{}` and get reasonable
// behaviour.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts (including the
	// initial call). 1 disables retry; 0 picks defaultMaxAttempts.
	MaxAttempts int

	// InitialDelay is the first backoff delay. Subsequent delays
	// double up to MaxDelay. Zero picks defaultInitialDelay.
	InitialDelay time.Duration

	// MaxDelay caps any single backoff delay. Zero picks
	// defaultMaxDelay.
	MaxDelay time.Duration

	// RateLimitDelay is the delay used after a 429 response when the
	// wrapper can't extract a Retry-After hint from the error. Zero
	// picks defaultRateLimitDelay. Distinguished from the
	// exponential-backoff schedule because rate-limited servers want
	// noticeably longer cooldowns than transient 5xx hiccups.
	RateLimitDelay time.Duration

	// OverloadDelay is the delay used after Anthropic's 529 (overloaded
	// error). Anthropic recommends "retry shortly" — we wait longer
	// than InitialDelay but shorter than RateLimitDelay. Zero picks
	// defaultOverloadDelay.
	OverloadDelay time.Duration

	// Logger receives one warn line per retry. Defaults to
	// slog.Default.
	Logger *slog.Logger

	// OnBackoff is invoked once per backoff sleep (BEFORE the wait
	// starts) so subscribers can surface the delay to operators.
	// The cog wires this to its Activity hub so the TUI / webui
	// can render "rate limited, retry 2/5, 15s" instead of going
	// silent for tens of seconds. Nil = disabled (the retry just
	// logs to slog as before).
	//
	// Reason is one of "rate_limited", "overloaded", "transient" —
	// matches the delay schedule classification.
	//
	// Called from whatever goroutine is driving the provider call
	// (typically the cog's own goroutine via actor.Spawn). Must be
	// non-blocking; the retry sleep starts immediately after.
	OnBackoff func(attempt, maxAttempts int, delay time.Duration, reason string)

	// rng is exposed for tests so jitter is deterministic; nil uses
	// math/rand's package-level Rand which is seeded at init.
	rng *rand.Rand
}

const (
	// defaultMaxAttempts gives sustained-rate-limit responses a
	// realistic chance to recover. Mistral's free-tier and
	// account-quota throttles can hold for tens of seconds; with
	// 3 attempts and 10s+jitter the wrapper exhausts before the
	// quota window resets and the operator sees a leaked 429.
	// 5 attempts × 15s default rate-limit delay = up to ~75s of
	// patience, which covers most provider throttle windows.
	defaultMaxAttempts    = 5
	defaultInitialDelay   = 1 * time.Second
	defaultMaxDelay       = 30 * time.Second
	defaultRateLimitDelay = 15 * time.Second
	defaultOverloadDelay  = 5 * time.Second
)

// applyDefaults fills zero-valued config fields. Returns a fresh
// RetryConfig — caller's instance is unmodified.
func (c RetryConfig) applyDefaults() RetryConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.InitialDelay <= 0 {
		c.InitialDelay = defaultInitialDelay
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = defaultMaxDelay
	}
	if c.RateLimitDelay <= 0 {
		c.RateLimitDelay = defaultRateLimitDelay
	}
	if c.OverloadDelay <= 0 {
		c.OverloadDelay = defaultOverloadDelay
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// RetriableError is the opt-in interface adapters can implement to
// expose retry-relevant metadata. The wrapper checks errors.As on
// every error and falls back to heuristic classification when the
// interface isn't implemented.
//
// V1 anthropic / mistral adapters return plain `fmt.Errorf` strings,
// so the heuristic fallback is the live path. Adapters can opt in
// later without breaking the wrapper.
type RetriableError interface {
	error
	// Retriable reports whether the wrapper should retry this error.
	Retriable() bool
	// RetryAfter is a hint for how long to wait. Zero means "use the
	// wrapper's backoff schedule"; positive overrides.
	RetryAfter() time.Duration
}

// WithRetry wraps a Provider with retry logic. Retries transient
// errors with exponential backoff + full jitter, capped at
// MaxAttempts. Permanent errors (4xx, auth, malformed-request) and
// context cancellation pass through untouched on the first attempt.
//
// Retries cover both Chat and ChatStructured; the wrapper's
// classification is the same for both.
func WithRetry(p Provider, cfg RetryConfig) Provider {
	return &retryingProvider{
		inner: p,
		cfg:   cfg.applyDefaults(),
	}
}

type retryingProvider struct {
	inner Provider
	cfg   RetryConfig
}

func (r *retryingProvider) Name() string { return r.inner.Name() }

func (r *retryingProvider) Chat(ctx context.Context, req Request) (Response, error) {
	var (
		resp Response
		err  error
	)
	for attempt := 1; attempt <= r.cfg.MaxAttempts; attempt++ {
		resp, err = r.inner.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		if !r.shouldRetry(ctx, err) || attempt == r.cfg.MaxAttempts {
			return resp, err
		}
		r.sleep(ctx, attempt, err)
	}
	return resp, err
}

func (r *retryingProvider) ChatStructured(ctx context.Context, req Request, schema Schema, dst any) (Usage, error) {
	var (
		usage Usage
		err   error
	)
	for attempt := 1; attempt <= r.cfg.MaxAttempts; attempt++ {
		usage, err = r.inner.ChatStructured(ctx, req, schema, dst)
		if err == nil {
			return usage, nil
		}
		if !r.shouldRetry(ctx, err) || attempt == r.cfg.MaxAttempts {
			return usage, err
		}
		r.sleep(ctx, attempt, err)
	}
	return usage, err
}

// shouldRetry classifies an error. Order:
//
//  1. Context cancellation / deadline → never retry (caller wants out).
//  2. Opt-in RetriableError → honours the adapter's say.
//  3. Net / URL errors → transient by nature; retry.
//  4. String heuristics on the error message — match the
//     `fmt.Errorf("provider: status N: ...")` shape we ship today.
//
// Permanent failures (4xx that aren't 408/429, malformed-request,
// auth) fall through to false.
func (r *retryingProvider) shouldRetry(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	// Caller-driven cancellation — never retry.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Opt-in: adapter says.
	var re RetriableError
	if errors.As(err, &re) {
		return re.Retriable()
	}
	// Net / URL errors — typically transient (DNS, connection reset,
	// TCP RST). Retry these without further inspection.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	// String classification on `fmt.Errorf("...: status N: ...")`.
	msg := err.Error()
	if containsAny(msg, retriablePatterns) {
		return true
	}
	if containsAny(msg, permanentPatterns) {
		return false
	}
	// Unknown shape — be conservative and don't retry. A buggy
	// adapter that returns mystery errors shouldn't get masked by
	// silent retries.
	return false
}

// sleep waits the appropriate backoff before the next attempt. Logs
// one warn line so operators can correlate slow cycles with retry
// activity. If ctx fires during the sleep, returns early — caller
// will see the cancelled context on the next attempt.
func (r *retryingProvider) sleep(ctx context.Context, attempt int, err error) {
	delay := r.delayFor(attempt, err)
	reason := classifyReason(err)
	r.cfg.Logger.Warn("llm retry: backing off",
		"provider", r.inner.Name(),
		"attempt", attempt,
		"max_attempts", r.cfg.MaxAttempts,
		"delay", delay,
		"reason", reason,
		"err", err,
	)
	if r.cfg.OnBackoff != nil {
		// Surface the wait to subscribers (cog → Activity hub →
		// TUI/webui). Fired BEFORE the timer so the UI updates
		// immediately rather than after the wait.
		r.cfg.OnBackoff(attempt, r.cfg.MaxAttempts, delay, reason)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// classifyReason buckets a retriable error into one of the three
// reason tags exposed via OnBackoff. Mirrors the dispatch in
// delayFor — same patterns, just returning the tag instead of the
// duration.
func classifyReason(err error) string {
	if err == nil {
		return "transient"
	}
	msg := err.Error()
	switch {
	case containsAny(msg, rateLimitPatterns):
		return "rate_limited"
	case containsAny(msg, overloadPatterns):
		return "overloaded"
	default:
		return "transient"
	}
}

// delayFor picks the backoff duration. Honours an opt-in
// RetryAfter hint, otherwise: rate-limit delay for 429-shaped errors,
// overload delay for 529-shaped errors, exponential backoff with full
// jitter for everything else. Always capped at MaxDelay.
func (r *retryingProvider) delayFor(attempt int, err error) time.Duration {
	// Opt-in RetryAfter wins.
	var re RetriableError
	if errors.As(err, &re) {
		if hint := re.RetryAfter(); hint > 0 {
			return capDelay(hint, r.cfg.MaxDelay)
		}
	}

	msg := err.Error()
	if containsAny(msg, rateLimitPatterns) {
		return capDelay(r.cfg.RateLimitDelay, r.cfg.MaxDelay)
	}
	if containsAny(msg, overloadPatterns) {
		return capDelay(r.cfg.OverloadDelay, r.cfg.MaxDelay)
	}

	// Exponential: initial * 2^(attempt-1), capped, with full jitter.
	base := r.cfg.InitialDelay
	for i := 1; i < attempt; i++ {
		base *= 2
		if base >= r.cfg.MaxDelay {
			base = r.cfg.MaxDelay
			break
		}
	}
	jitter := r.jitterFraction()
	return time.Duration(float64(base) * jitter)
}

// jitterFraction returns a value in [0.5, 1.0] so the actual backoff
// is between half and full of the computed base. Full-jitter spreads
// thundering herds without ever waiting more than the cap.
func (r *retryingProvider) jitterFraction() float64 {
	if r.cfg.rng != nil {
		return 0.5 + r.cfg.rng.Float64()*0.5
	}
	return 0.5 + rand.Float64()*0.5
}

// capDelay clamps to max.
func capDelay(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	if d < 0 {
		return 0
	}
	return d
}

// containsAny returns true when any pattern is a substring of the
// (lower-cased) message.
func containsAny(msg string, patterns []string) bool {
	low := strings.ToLower(msg)
	for _, p := range patterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// retriablePatterns are substrings of error messages that indicate a
// transient failure worth retrying. Lowercased; `containsAny`
// lower-cases the input. Order doesn't matter; first match wins.
var retriablePatterns = []string{
	"status 429", "status 500", "status 502", "status 503", "status 504", "status 529",
	"too many requests", "overloaded", "rate limit", "rate-limit", "rate_limit",
	"timeout", "deadline", "temporarily unavailable", "service unavailable",
	"connection reset", "connection refused", "no such host", "i/o timeout",
}

// permanentPatterns are substrings that explicitly indicate a NON-
// retriable failure. Used as a fast-path: if a message matches one,
// we skip retry without further analysis.
var permanentPatterns = []string{
	"status 400", "status 401", "status 403", "status 404", "status 413", "status 422",
	"invalid request", "unauthorized", "forbidden", "not found", "payload too large",
}

// rateLimitPatterns identifies 429-shaped errors so the wrapper picks
// the rate-limit-specific delay rather than exponential backoff.
var rateLimitPatterns = []string{
	"status 429", "too many requests", "rate limit", "rate-limit", "rate_limit",
}

// overloadPatterns identifies Anthropic-style 529 errors.
var overloadPatterns = []string{
	"status 529", "overloaded",
}

// Compile-time check that retryingProvider satisfies Provider.
var _ Provider = (*retryingProvider)(nil)

// Sentinel for tests / docs: a typed nil error to pair with
// fmt.Errorf in contexts that want to demonstrate a permanent error.
var errPermanentSentinel = errors.New("permanent: validation failed")

// Suppress unused-var lint for the sentinel; tests reference it.
var _ = errPermanentSentinel
