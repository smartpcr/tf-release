package pattern

import (
	"context"
	"encoding/base64"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// -update regenerates the committed console_app golden fixtures under testdata/.
var updateConsole = flag.Bool("update", false, "update console_app golden fixtures")

// scriptTransport records every script Exec sees and replays a scripted queue of
// Results (FIFO). It is the "fake Transport with a scripted Result queue" the
// Console status-drift scenario calls for — no network, fully deterministic.
type scriptTransport struct {
	osKind  spec.OSKind
	host    string
	scripts []string           // every script passed to Exec, in order
	queue   []transport.Result // popped FIFO; empty ⇒ exit 0 with no output
}

func (f *scriptTransport) Connect(ctx context.Context) error { return nil }
func (f *scriptTransport) Close() error                      { return nil }
func (f *scriptTransport) OS() spec.OSKind                   { return f.osKind }
func (f *scriptTransport) Host() string {
	if f.host == "" {
		return "host1"
	}
	return f.host
}
func (f *scriptTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.scripts = append(f.scripts, c.Script)
	if len(f.queue) == 0 {
		return transport.Result{ExitCode: 0}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}
func (f *scriptTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	return nil
}
func (f *scriptTransport) Download(ctx context.Context, remote, local string) error { return nil }

// markerResult builds a scripted marker read Result: base64-encoded JSON so
// readMarker decodes it exactly as it would a real on-host marker.
func markerResult(version string) transport.Result {
	body := `{"version":"` + version + `","sha256":"abc","extracted_at":"2026-01-01T00:00:00Z"}`
	return transport.Result{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(body))}
}

func consoleRC(osKind spec.OSKind, root, app, version string, pat spec.Pattern) ReleaseCtx {
	p := layout.NewPaths(osKind, root, app, version)
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: app},
		Target:   spec.Target{OS: osKind},
		Artifact: spec.Artifact{Version: version},
		Pattern:  pat,
	}
	env := layout.MergeEnv(layout.BuiltinEnv(app, version, p, 0), s.Environment)
	return ReleaseCtx{App: app, Version: version, P: p, Spec: s, Env: env}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateConsole {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("golden mismatch for %s.\n got: %q\nwant: %q", name, got, string(want))
	}
}

// Scenario: Console verify flow — Configure emits post_install then
// verify_command scripts (in <current>, with env), Start is a no-op, and the
// generated scripts match the committed goldens.
func TestConsoleVerifyFlowGolden(t *testing.T) {
	var c ConsoleApp

	// Windows: post_install + verify_command.
	rcW := consoleRC(spec.OSWindows, `C:\deploy`, "sample-svc", "1.0.0", spec.Pattern{
		Type:          spec.PatternConsoleApp,
		Exe:           "sample-svc.exe",
		PostInstall:   ".\\setup.ps1 -Install",
		VerifyCommand: "sample-svc.exe --version",
	})
	fw := &scriptTransport{osKind: spec.OSWindows}
	if err := c.Configure(context.Background(), fw, rcW); err != nil {
		t.Fatalf("windows Configure: %v", err)
	}
	if len(fw.scripts) != 2 {
		t.Fatalf("windows Configure should emit post_install + verify_command scripts, got %d", len(fw.scripts))
	}
	checkGolden(t, "console_postinstall_windows.golden", fw.scripts[0])
	checkGolden(t, "console_verify_windows.golden", fw.scripts[1])

	// Linux: verify_command only.
	rcL := consoleRC(spec.OSLinux, "/opt/labdeploy", "sample-svc", "1.0.0", spec.Pattern{
		Type:          spec.PatternConsoleApp,
		Exe:           "sample-svc",
		VerifyCommand: "./sample-svc --version",
	})
	fl := &scriptTransport{osKind: spec.OSLinux}
	if err := c.Configure(context.Background(), fl, rcL); err != nil {
		t.Fatalf("linux Configure: %v", err)
	}
	if len(fl.scripts) != 1 {
		t.Fatalf("linux Configure should emit only verify_command script, got %d", len(fl.scripts))
	}
	checkGolden(t, "console_verify_linux.golden", fl.scripts[0])

	// Start is a no-op: it must NOT touch the transport.
	fs := &scriptTransport{osKind: spec.OSWindows}
	if err := c.Start(context.Background(), fs, rcW); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(fs.scripts) != 0 {
		t.Fatalf("Start must be a no-op, but it ran %d script(s)", len(fs.scripts))
	}
	// Stop is a no-op too.
	if err := c.Stop(context.Background(), fs, rcW); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(fs.scripts) != 0 {
		t.Fatalf("Stop must be a no-op, but it ran %d script(s)", len(fs.scripts))
	}
}

// Scenario: Console status drift — a marker version differing from the manifest
// version reports drift; a matching marker reports n/a; a missing marker (the
// deployed tree no longer matches) also reports drift.
func TestConsoleStatusDrift(t *testing.T) {
	var c ConsoleApp
	rc := consoleRC(spec.OSWindows, `C:\deploy`, "sample-svc", "1.1.0", spec.Pattern{
		Type: spec.PatternConsoleApp, Exe: "sample-svc.exe",
	})

	// Matching marker ⇒ n/a.
	fMatch := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{markerResult("1.1.0")}}
	if s, err := c.Status(context.Background(), fMatch, rc); err != nil || s != "n/a" {
		t.Fatalf("matching marker: got status=%q err=%v, want n/a", s, err)
	}

	// Different marker version ⇒ drift.
	fDrift := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{markerResult("1.0.0")}}
	if s, err := c.Status(context.Background(), fDrift, rc); err != nil || s != "drift" {
		t.Fatalf("stale marker: got status=%q err=%v, want drift", s, err)
	}

	// Missing marker (exit 3) ⇒ drift.
	fMissing := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{{ExitCode: 3}}}
	if s, err := c.Status(context.Background(), fMissing, rc); err != nil || s != "drift" {
		t.Fatalf("missing marker: got status=%q err=%v, want drift", s, err)
	}
}
