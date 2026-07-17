package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Retry count: attempt succeeds on the Nth try; retryConnect must call it
// exactly N times and return nil once it succeeds.
func TestRetryConnectSucceedsAfterRetries(t *testing.T) {
	calls := 0
	err := retryConnect(context.Background(), 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("connection refused (try %d)", calls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("attempt called %d times, want 3", calls)
	}
}

// Exhaustion: every attempt fails. The loop runs retries+1 times and wraps the
// last error in ERR_CONNECT.
func TestRetryConnectExhaustsToErrConnect(t *testing.T) {
	calls := 0
	err := retryConnect(context.Background(), 2, time.Millisecond, func() error {
		calls++
		return fmt.Errorf("dial fail %d", calls)
	})
	if calls != 3 {
		t.Fatalf("attempt called %d times, want retries+1=3", calls)
	}
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_CONNECT" {
		t.Fatalf("expected ERR_CONNECT, got %v", err)
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("error should report attempt count, got %q", err.Error())
	}
}

// Backoff placement: with retries=2 (3 attempts) and all failures, the fixed
// backoff is applied only BETWEEN attempts — twice, never after the final one.
// A trailing sleep would push elapsed time to ~3× backoff.
func TestRetryConnectNoTrailingBackoff(t *testing.T) {
	const backoff = 40 * time.Millisecond
	start := time.Now()
	_ = retryConnect(context.Background(), 2, backoff, func() error {
		return errors.New("always fails")
	})
	elapsed := time.Since(start)
	// 3 attempts → exactly 2 backoffs. Assert it slept ~2× and clearly < 3×.
	if elapsed < 2*backoff {
		t.Fatalf("elapsed %v < 2×backoff %v: backoff not applied between attempts", elapsed, 2*backoff)
	}
	if elapsed >= 3*backoff {
		t.Fatalf("elapsed %v >= 3×backoff %v: a trailing backoff was slept after the final attempt", elapsed, 3*backoff)
	}
}

// ERR_AUTH is not retried: a noRetry(ErrAuth(...)) short-circuits immediately.
func TestRetryConnectAuthNotRetried(t *testing.T) {
	calls := 0
	err := retryConnect(context.Background(), 5, time.Millisecond, func() error {
		calls++
		return noRetry(ErrAuth(errors.New("401 unauthorized")))
	})
	if calls != 1 {
		t.Fatalf("auth failure retried: attempt called %d times, want 1", calls)
	}
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_AUTH" {
		t.Fatalf("expected ERR_AUTH surfaced verbatim, got %v", err)
	}
}

// A non-retryable ERR_CONNECT (e.g. host-key mismatch) is also surfaced
// immediately without consuming retries.
func TestRetryConnectFatalConnectNotRetried(t *testing.T) {
	calls := 0
	err := retryConnect(context.Background(), 5, time.Millisecond, func() error {
		calls++
		return noRetry(ErrConnect(errors.New("host key mismatch")))
	})
	if calls != 1 {
		t.Fatalf("fatal connect retried: attempt called %d times, want 1", calls)
	}
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_CONNECT" {
		t.Fatalf("expected ERR_CONNECT surfaced verbatim, got %v", err)
	}
}

// Context cancellation during the backoff window aborts with ERR_CONNECT.
func TestRetryConnectContextCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryConnect(ctx, 5, 500*time.Millisecond, func() error {
		calls++
		cancel() // cancel before the first backoff elapses
		return errors.New("retryable")
	})
	if calls != 1 {
		t.Fatalf("attempt called %d times, want 1 before cancel", calls)
	}
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_CONNECT" {
		t.Fatalf("expected ERR_CONNECT on cancel, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped context.Canceled, got %v", err)
	}
}
