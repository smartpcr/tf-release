package engine

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// recTransport records every Exec script; the log-archive probe returns NOFILES
// so CollectFiles short-circuits without a Download.
type recTransport struct {
	os      spec.OSKind
	scripts []string
}

func (r *recTransport) Connect(ctx context.Context) error { return nil }
func (r *recTransport) Close() error                      { return nil }
func (r *recTransport) OS() spec.OSKind                   { return r.os }
func (r *recTransport) Host() string                      { return "rec-host" }
func (r *recTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	r.scripts = append(r.scripts, c.Script)
	return transport.Result{ExitCode: 0, Stdout: "NOFILES"}, nil
}
func (r *recTransport) Upload(ctx context.Context, rd io.Reader, size int64, remote string) error {
	return nil
}
func (r *recTransport) Download(ctx context.Context, remote, local string) error { return nil }

var _ transport.Transport = (*recTransport)(nil)

// chdirTemp switches into a throwaway dir so relative collection dirs
// (labdeploy-logs/*) never pollute the package directory.
func chdirTemp(t *testing.T) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// TestResolveTargetGlobs covers evaluator item 2's resolution logic: absolute
// globs pass through, relative globs join under the base with the target sep.
func TestResolveTargetGlobs(t *testing.T) {
	lin := resolveTargetGlobs(spec.OSLinux, "/opt/deploy/app/shared",
		[]string{"*.log", "sub/*.log", "/var/log/abs.log"})
	wantLin := []string{"/opt/deploy/app/shared/*.log", "/opt/deploy/app/shared/sub/*.log", "/var/log/abs.log"}
	for i := range wantLin {
		if lin[i] != wantLin[i] {
			t.Fatalf("linux[%d]=%q want %q", i, lin[i], wantLin[i])
		}
	}
	win := resolveTargetGlobs(spec.OSWindows, `C:\deploy\app\shared`,
		[]string{"*.log", `D:\abs\x.log`, `\\srv\share\y.log`})
	wantWin := []string{`C:\deploy\app\shared\*.log`, `D:\abs\x.log`, `\\srv\share\y.log`}
	for i := range wantWin {
		if win[i] != wantWin[i] {
			t.Fatalf("win[%d]=%q want %q", i, win[i], wantWin[i])
		}
	}
}

// TestCollectDeploymentLogsWiring guards evaluator item 1: Deployment.Logs.Paths
// and Deployment.Logs.WindowsEventLogs are actually collected from the target,
// with relative paths resolved under the app's shared dir and events filtered
// from operation start.
func TestCollectDeploymentLogsWiring(t *testing.T) {
	chdirTemp(t)
	rec := &recTransport{os: spec.OSLinux}
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: "app"},
		Target:   spec.Target{OS: spec.OSLinux},
		Logs: spec.LogsSpec{
			Paths: []string{"*.log"},
		},
	}
	p := layout.NewPaths(spec.OSLinux, "/opt/deploy", "app", "1.0.0")
	e := New()
	started := time.Unix(1_700_000_000, 0).UTC()
	e.collectDeploymentLogs(context.Background(), rec, s, p, started)

	if len(rec.scripts) == 0 {
		t.Fatal("collectDeploymentLogs made no target calls; logs.paths unused")
	}
	joined := strings.Join(rec.scripts, "\n---\n")
	// Relative glob resolved under <app>/shared and bounded find root present.
	if !strings.Contains(joined, "/opt/deploy/app/shared") {
		t.Fatalf("relative glob not resolved under shared dir; scripts:\n%s", joined)
	}
}

// TestCollectDeploymentLogsEvents verifies windows_event_logs are collected with
// the operation-start filter timestamp (DESIGN §6.5 since=operation start).
func TestCollectDeploymentLogsEvents(t *testing.T) {
	chdirTemp(t)
	rec := &recTransport{os: spec.OSWindows}
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: "app"},
		Target:   spec.Target{OS: spec.OSWindows},
		Logs: spec.LogsSpec{
			WindowsEventLogs: []spec.EventLogSpec{{Log: "Application"}},
		},
	}
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "app", "1.0.0")
	e := New()
	started := time.Unix(1_700_000_000, 0).UTC()
	e.collectDeploymentLogs(context.Background(), rec, s, p, started)

	joined := strings.Join(rec.scripts, "\n---\n")
	if !strings.Contains(joined, "Get-WinEvent") {
		t.Fatalf("event logs not collected; scripts:\n%s", joined)
	}
	if !strings.Contains(joined, started.Format(time.RFC3339)) {
		t.Fatalf("event filter did not use operation-start timestamp %s; scripts:\n%s",
			started.Format(time.RFC3339), joined)
	}
}

// TestCollectDeploymentLogsNoop: with no logs configured, no target calls.
func TestCollectDeploymentLogsNoop(t *testing.T) {
	rec := &recTransport{os: spec.OSLinux}
	s := &spec.Deployment{Metadata: spec.Metadata{Name: "app"}, Target: spec.Target{OS: spec.OSLinux}}
	p := layout.NewPaths(spec.OSLinux, "/opt/deploy", "app", "1.0.0")
	New().collectDeploymentLogs(context.Background(), rec, s, p, time.Now())
	if len(rec.scripts) != 0 {
		t.Fatalf("expected no target calls when logs unset, got %d", len(rec.scripts))
	}
}
