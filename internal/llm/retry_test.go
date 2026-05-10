package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedRetryProvider returns a queue of (response, error) pairs in
// order, one per call. Lets tests script the failure / recovery
// sequence the wrapper has to navigate.
type scriptedRetryProvider struct {
	name    string
	scripts []scriptStep
	called  int32
}

type scriptStep struct {
	resp Response
	err  error
}

func (s *scriptedRetryProvider) Name() string { return s.name }

func (s *scriptedRetryProvider) Chat(_ context.Context, _ Request) (Response, error) {
	idx := int(atomic.AddInt32(&s.called, 1) - 1)
	if idx >= len(s.scripts) {
		return Response{}, fmt.Errorf("scripted: exhausted at call %d", idx+1)
	}
	step := s.scripts[idx]
	return step.resp, step.err
}

func (s *scriptedRetryProvider) ChatStructured(_ context.Context, _ Request, _ Schema, _ any) (Usage, error) {
	idx := int(atomic.AddInt32(&s.called, 1) - 1)
	if idx >= len(s.scripts) {
		return Usage{}, fmt.Errorf("scripted: exhausted")
	}
	step := s.scripts[idx]
	return Usage{}, step.err
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fastRetryConfig keeps test runs sub-second by collapsing all
// timing knobs to nanosecond floors. Operationally meaningless; lets
// us assert behaviour without burning real wall-clock time.
func fastRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    3,
		InitialDelay:   time.Nanosecond,
		MaxDelay:       time.Nanosecond,
		RateLimitDelay: time.Nanosecond,
		OverloadDelay:  time.Nanosecond,
		Logger:         discardLog(),
		// Deterministic jitter so backoff math doesn't flake.
		rng: rand.New(rand.NewSource(1)),
	}
}

// ---- shouldRetry classification ----

func TestShouldRetry_ContextCancelled(t *testing.T) {
	r := &retryingProvider{cfg: fastRetryConfig().applyDefaults()}
	if r.shouldRetry(context.Background(), context.Canceled) {
		t.Error("context.Canceled should not retry")
	}
	if r.shouldRetry(context.Background(), context.DeadlineExceeded) {
		t.Error("DeadlineExceeded should not retry")
	}
}

func TestShouldRetry_StatusCodes(t *testing.T) {
	r := &retryingProvider{cfg: fastRetryConfig().applyDefaults()}
	cases := []struct {
		err  error
		want bool
		name string
	}{
		{fmt.Errorf("anthropic: status 429: rate limited"), true, "429"},
		{fmt.Errorf("anthropic: status 500: oops"), true, "500"},
		{fmt.Errorf("anthropic: status 502: bad gateway"), true, "502"},
		{fmt.Errorf("anthropic: status 503: service unavailable"), true, "503"},
		{fmt.Errorf("anthropic: status 529: overloaded"), true, "529"},
		{fmt.Errorf("anthropic: status 400: invalid"), false, "400"},
		{fmt.Errorf("anthropic: status 401: unauthorized"), false, "401"},
		{fmt.Errorf("anthropic: status 403: forbidden"), false, "403"},
		{fmt.Errorf("anthropic: status 404: model not found"), false, "404"},
		{fmt.Errorf("anthropic: status 413: payload too large"), false, "413"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.shouldRetry(context.Background(), tc.err); got != tc.want {
				t.Errorf("shouldRetry(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestShouldRetry_NetErrorAlwaysRetries(t *testing.T) {
	r := &retryingProvider{cfg: fastRetryConfig().applyDefaults()}
	netErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if !r.shouldRetry(context.Background(), netErr) {
		t.Error("net.OpError should retry")
	}
	urlErr := &url.Error{Op: "Get", URL: "https://x", Err: errors.New("hangup")}
	if !r.shouldRetry(context.Background(), urlErr) {
		t.Error("url.Error should retry")
	}
}

func TestShouldRetry_UnknownErrorIsConservative(t *testing.T) {
	r := &retryingProvider{cfg: fastRetryConfig().applyDefaults()}
	mystery := errors.New("something unexpected happened")
	if r.shouldRetry(context.Background(), mystery) {
		t.Error("mystery errors should NOT retry (conservative default)")
	}
}

// ---- RetriableError opt-in ----

type optInError struct {
	retry bool
	after time.Duration
	msg   string
}

func (e *optInError) Error() string             { return e.msg }
func (e *optInError) Retriable() bool           { return e.retry }
func (e *optInError) RetryAfter() time.Duration { return e.after }

func TestShouldRetry_HonoursRetriableInterface(t *testing.T) {
	r := &retryingProvider{cfg: fastRetryConfig().applyDefaults()}
	if !r.shouldRetry(context.Background(), &optInError{retry: true, msg: "bla"}) {
		t.Error("Retriable=true should retry")
	}
	if r.shouldRetry(context.Background(), &optInError{retry: false, msg: "bla"}) {
		t.Error("Retriable=false should not retry")
	}
}

// ---- delayFor ----

func TestDelayFor_RateLimitDelayUsedFor429(t *testing.T) {
	cfg := RetryConfig{RateLimitDelay: 30 * time.Millisecond, MaxDelay: 30 * time.Millisecond}
	r := &retryingProvider{cfg: cfg.applyDefaults()}
	got := r.delayFor(2, fmt.Errorf("status 429: too many requests"))
	if got != 30*time.Millisecond {
		t.Errorf("delay = %v, want rate-limit delay 30ms", got)
	}
}

func TestDelayFor_OverloadDelayUsedFor529(t *testing.T) {
	cfg := RetryConfig{OverloadDelay: 20 * time.Millisecond, MaxDelay: 20 * time.Millisecond}
	r := &retryingProvider{cfg: cfg.applyDefaults()}
	got := r.delayFor(2, fmt.Errorf("status 529: overloaded"))
	if got != 20*time.Millisecond {
		t.Errorf("delay = %v, want overload delay 20ms", got)
	}
}

func TestDelayFor_RetryAfterHintWins(t *testing.T) {
	cfg := RetryConfig{RateLimitDelay: 99 * time.Millisecond, MaxDelay: 200 * time.Millisecond}
	r := &retryingProvider{cfg: cfg.applyDefaults()}
	hint := &optInError{retry: true, after: 50 * time.Millisecond, msg: "status 429"}
	got := r.delayFor(2, hint)
	if got != 50*time.Millisecond {
		t.Errorf("delay = %v, want RetryAfter hint 50ms", got)
	}
}

func TestDelayFor_ExponentialBackoffCapped(t *testing.T) {
	cfg := RetryConfig{
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     40 * time.Millisecond,
		rng:          rand.New(rand.NewSource(1)),
	}
	r := &retryingProvider{cfg: cfg.applyDefaults()}
	// attempt 1 → 10ms base * jitter [0.5, 1.0] = at most 10ms
	d1 := r.delayFor(1, fmt.Errorf("status 503"))
	if d1 > 10*time.Millisecond {
		t.Errorf("attempt 1 delay = %v, expected ≤ 10ms", d1)
	}
	// attempt 5 should hit cap regardless of attempt count
	d5 := r.delayFor(5, fmt.Errorf("status 503"))
	if d5 > 40*time.Millisecond {
		t.Errorf("attempt 5 delay = %v, expected ≤ MaxDelay 40ms", d5)
	}
}

// ---- WithRetry end-to-end ----

func TestWithRetry_RecoversAfterTransient(t *testing.T) {
	want := Response{Content: []ContentBlock{TextBlock{Text: "hi"}}, StopReason: "end_turn"}
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: fmt.Errorf("anthropic: status 503: hiccup")},
			{err: fmt.Errorf("anthropic: status 503: hiccup")},
			{resp: want},
		},
	}
	r := WithRetry(p, fastRetryConfig())
	got, err := r.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if textOf(got) != "hi" {
		t.Errorf("text = %q, want hi", textOf(got))
	}
	if atomic.LoadInt32(&p.called) != 3 {
		t.Errorf("called = %d, want 3 (initial + 2 retries)", atomic.LoadInt32(&p.called))
	}
}

func TestWithRetry_StopsOnPermanentError(t *testing.T) {
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: fmt.Errorf("anthropic: status 401: unauthorized")},
			{err: fmt.Errorf("should not be reached")},
		},
	}
	r := WithRetry(p, fastRetryConfig())
	_, err := r.Chat(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected permanent error to surface")
	}
	if got := atomic.LoadInt32(&p.called); got != 1 {
		t.Errorf("called = %d, want 1 (no retry on 401)", got)
	}
}

func TestWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	cfg := fastRetryConfig()
	cfg.MaxAttempts = 2 // 1 + 1 retry = 2 total
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: fmt.Errorf("status 503: a")},
			{err: fmt.Errorf("status 503: b")},
			{err: fmt.Errorf("status 503: c")},
		},
	}
	r := WithRetry(p, cfg)
	_, err := r.Chat(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if got := atomic.LoadInt32(&p.called); got != 2 {
		t.Errorf("called = %d, want 2 (MaxAttempts cap)", got)
	}
}

func TestWithRetry_ContextCancelStopsImmediately(t *testing.T) {
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: context.Canceled},
		},
	}
	r := WithRetry(p, fastRetryConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Chat(ctx, Request{})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&p.called); got != 1 {
		t.Errorf("called = %d, want 1 (no retry on cancelled ctx)", got)
	}
}

func TestWithRetry_ChatStructuredAlsoRetries(t *testing.T) {
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: fmt.Errorf("status 502")},
			{}, // success
		},
	}
	r := WithRetry(p, fastRetryConfig())
	if _, err := r.ChatStructured(context.Background(), Request{}, Schema{}, &struct{}{}); err != nil {
		t.Fatalf("ChatStructured: %v", err)
	}
	if got := atomic.LoadInt32(&p.called); got != 2 {
		t.Errorf("called = %d, want 2 (initial + 1 retry)", got)
	}
}

func TestWithRetry_PassesThroughSuccessFirstTry(t *testing.T) {
	want := Response{Content: []ContentBlock{TextBlock{Text: "ok"}}}
	p := &scriptedRetryProvider{
		name:    "scripted",
		scripts: []scriptStep{{resp: want}},
	}
	r := WithRetry(p, fastRetryConfig())
	if _, err := r.Chat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&p.called); got != 1 {
		t.Errorf("called = %d, want 1", got)
	}
}

// ---- OnBackoff callback ----

func TestOnBackoff_FiresOncePerRetry(t *testing.T) {
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: errors.New("provider: status 429: rate limited")},
			{err: errors.New("provider: status 429: rate limited")},
			{resp: Response{}},
		},
	}
	type call struct {
		attempt, max int
		delay        time.Duration
		reason       string
	}
	var calls []call
	cfg := fastRetryConfig()
	cfg.OnBackoff = func(attempt, max int, delay time.Duration, reason string) {
		calls = append(calls, call{attempt, max, delay, reason})
	}
	r := WithRetry(p, cfg)
	if _, err := r.Chat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	// 3 attempts total, 2 backoffs (after attempts 1 + 2).
	if len(calls) != 2 {
		t.Fatalf("OnBackoff fires = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.attempt != i+1 {
			t.Errorf("[%d] attempt = %d, want %d", i, c.attempt, i+1)
		}
		if c.max != cfg.MaxAttempts {
			t.Errorf("[%d] max = %d, want %d", i, c.max, cfg.MaxAttempts)
		}
		if c.reason != "rate_limited" {
			t.Errorf("[%d] reason = %q, want rate_limited", i, c.reason)
		}
		if c.delay <= 0 {
			t.Errorf("[%d] delay = %v, want positive", i, c.delay)
		}
	}
}

func TestOnBackoff_NotFiredWhenNoRetryNeeded(t *testing.T) {
	p := &scriptedRetryProvider{
		name:    "scripted",
		scripts: []scriptStep{{resp: Response{}}},
	}
	called := false
	cfg := fastRetryConfig()
	cfg.OnBackoff = func(int, int, time.Duration, string) { called = true }
	r := WithRetry(p, cfg)
	if _, err := r.Chat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("OnBackoff fired on a clean first-try success")
	}
}

func TestOnBackoff_NilIsSafe(t *testing.T) {
	// Belt-and-braces: a wrapper built without OnBackoff should
	// retry without panicking on the missing callback.
	p := &scriptedRetryProvider{
		name: "scripted",
		scripts: []scriptStep{
			{err: errors.New("provider: status 429: rate limited")},
			{resp: Response{}},
		},
	}
	cfg := fastRetryConfig()
	cfg.OnBackoff = nil // explicit
	r := WithRetry(p, cfg)
	if _, err := r.Chat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyReason(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("provider: status 429: rate_limited"), "rate_limited"},
		{errors.New("provider: status 529: overloaded"), "overloaded"},
		{errors.New("provider: status 503: service unavailable"), "transient"},
		{nil, "transient"},
	}
	for _, tc := range cases {
		if got := classifyReason(tc.err); got != tc.want {
			t.Errorf("classifyReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// ---- helpers ----

func textOf(r Response) string {
	for _, b := range r.Content {
		if t, ok := b.(TextBlock); ok {
			return t.Text
		}
	}
	return ""
}
