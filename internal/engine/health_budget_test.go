package engine

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// probeRecorder is a transport that records every probe command it receives and
// returns a configurable result, so RunHealthCheck's budget accounting can be
// asserted deterministically (no real sleeps beyond the tiny configured ones).
type probeRecorder struct {
	os        spec.OSKind
	exit      int // exit code returned per probe (non-zero => unhealthy)
	stdout    string
	timeouts  []int // TimeoutSec seen on each probe, in order
	execDelay time.Duration
}

func (p *probeRecorder) Connect(ctx context.Context) error { return nil }
func (p *probeRecorder) Close() error                      { return nil }
func (p *probeRecorder) OS() spec.OSKind                   { return p.os }
func (p *probeRecorder) Host() string                      { return "probe-host" }
func (p *probeRecorder) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	p.timeouts = append(p.timeouts, c.TimeoutSec)
	if p.execDelay > 0 {
		select {
		case <-ctx.Done():
			return transport.Result{}, ctx.Err()
		case <-time.After(p.execDelay):
		}
	}
	return transport.Result{ExitCode: p.exit, Stdout: p.stdout}, nil
}
func (p *probeRecorder) Upload(ctx context.Context, r io.Reader, size int64, remote string) error {
	return nil
}
func (p *probeRecorder) Download(ctx context.Context, remote, local string) error { return nil }

var _ transport.Transport = (*probeRecorder)(nil)

// TestHealthBudgetExhaustion: an always-unhealthy probe must fail with
// ERR_HEALTH_CHECK once timeout_seconds elapses — and MUST NOT run the full
// 30/60s per-probe timeout past the budget (evaluator item 3).
func TestHealthBudgetExhaustion(t *testing.T) {
	rec := &probeRecorder{os: spec.OSWindows, exit: 1, stdout: "connection refused"}
	hc := &spec.HealthCheck{
		Type:                "http",
		HTTP:                spec.HTTPCheck{URL: "http://localhost:8080/health"},
		InitialDelaySeconds: 0, // Budget() floors to... no: 0<=0 => 5? see below
		IntervalSeconds:     1,
		TimeoutSeconds:      2,
	}
	// Budget() forces initial=5 when <=0; we want a small initial, so set 1.
	hc.InitialDelaySeconds = 1

	start := time.Now()
	err := RunHealthCheck(context.Background(), rec, hc, `C:\wd`, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected ERR_HEALTH_CHECK on exhausted budget")
	}
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("want ERR_HEALTH_CHECK, got %v", err)
	}
	// Total budget is 2s; allow generous slack but it must be far below the
	// 30s per-probe timeout the old code would have permitted.
	if elapsed > 5*time.Second {
		t.Fatalf("budget not enforced: ran %s (>5s)", elapsed)
	}
	if len(rec.timeouts) == 0 {
		t.Fatal("no probes executed")
	}
	// Every probe's command timeout must be capped by the remaining budget (<=2s),
	// never the raw 30s http default.
	for i, to := range rec.timeouts {
		if to > 2 {
			t.Fatalf("probe %d timeout %d exceeds 2s budget cap", i, to)
		}
	}
}

// TestHealthPassesFirstProbe: a healthy probe returns nil immediately after the
// initial delay, without waiting out the budget.
func TestHealthPassesFirstProbe(t *testing.T) {
	rec := &probeRecorder{os: spec.OSLinux, exit: 0}
	hc := &spec.HealthCheck{
		Type:                "tcp",
		TCP:                 spec.TCPCheck{Port: 5432},
		InitialDelaySeconds: 1,
		IntervalSeconds:     5,
		TimeoutSeconds:      30,
	}
	start := time.Now()
	if err := RunHealthCheck(context.Background(), rec, hc, "/wd", nil); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("healthy probe should return promptly, took %s", elapsed)
	}
	if len(rec.timeouts) != 1 {
		t.Fatalf("want exactly 1 probe, got %d", len(rec.timeouts))
	}
}

// TestHealthContextCancel: a cancelled context aborts the loop with
// ERR_HEALTH_CHECK rather than sleeping out the budget.
func TestHealthContextCancel(t *testing.T) {
	rec := &probeRecorder{os: spec.OSLinux, exit: 1}
	hc := &spec.HealthCheck{
		Type:                "tcp",
		TCP:                 spec.TCPCheck{Port: 5432},
		InitialDelaySeconds: 30,
		IntervalSeconds:     5,
		TimeoutSeconds:      60,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	err := RunHealthCheck(ctx, rec, hc, "/wd", nil)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancel not honored, took %s", elapsed)
	}
}
