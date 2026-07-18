//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/pattern"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// consoleScriptTransport is the "fake Transport with a scripted Result queue"
// the acceptance scenarios call for: it records every script Exec sees and
// replays a FIFO queue of Results. No network, fully deterministic.
type consoleScriptTransport struct {
	osKind  spec.OSKind
	scripts []string
	queue   []transport.Result
}

func (f *consoleScriptTransport) Connect(ctx context.Context) error { return nil }
func (f *consoleScriptTransport) Close() error                      { return nil }
func (f *consoleScriptTransport) OS() spec.OSKind                   { return f.osKind }
func (f *consoleScriptTransport) Host() string                      { return "host1" }
func (f *consoleScriptTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.scripts = append(f.scripts, c.Script)
	if len(f.queue) == 0 {
		return transport.Result{ExitCode: 0}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}
func (f *consoleScriptTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	return nil
}
func (f *consoleScriptTransport) Download(ctx context.Context, remote, local string) error {
	return nil
}

// consoleWorld holds per-scenario state exercised through the Pattern interface.
type consoleWorld struct {
	c          pattern.ConsoleApp
	rc         pattern.ReleaseCtx
	configure  *consoleScriptTransport
	start      *consoleScriptTransport
	statusTx   *consoleScriptTransport
	status     string
	statusErr  error
	configErr  error
}

func osKindFrom(name string) (spec.OSKind, error) {
	switch name {
	case "windows":
		return spec.OSWindows, nil
	case "linux":
		return spec.OSLinux, nil
	}
	return "", fmt.Errorf("unknown os %q", name)
}

// buildConsoleRC mirrors the impl's ReleaseCtx construction (layout paths +
// merged builtin env) so the pattern verbs run exactly as the engine invokes
// them (DESIGN §9.1/§9.3).
func buildConsoleRC(osKind spec.OSKind, app, version string, pat spec.Pattern) pattern.ReleaseCtx {
	root := `C:\deploy`
	if osKind == spec.OSLinux {
		root = "/opt/labdeploy"
	}
	p := layout.NewPaths(osKind, root, app, version)
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: app},
		Target:   spec.Target{OS: osKind},
		Artifact: spec.Artifact{Version: version},
		Pattern:  pat,
	}
	env := layout.MergeEnv(layout.BuiltinEnv(app, version, p, 0), s.Environment)
	return pattern.ReleaseCtx{App: app, Version: version, P: p, Spec: s, Env: env}
}

// markerResult builds a scripted marker-read Result: base64-encoded JSON so the
// pattern's readMarker decodes it exactly as it would a real on-host marker.
func markerResult(version string) transport.Result {
	body := `{"version":"` + version + `","sha256":"abc","extracted_at":"2026-01-01T00:00:00Z"}`
	return transport.Result{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(body))}
}

// goldenPath resolves a committed fixture under internal/pattern/testdata,
// relative to this test file so it is independent of the working directory.
func goldenPath(name string) string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	return filepath.Join(dir, "..", "..", "..", "internal", "pattern", "testdata", name)
}

func (w *consoleWorld) reset() { *w = consoleWorld{} }

func (w *consoleWorld) givenConsoleRC(app, version, osName string) error {
	osKind, err := osKindFrom(osName)
	if err != nil {
		return err
	}
	exe := app + ".exe"
	if osKind == spec.OSLinux {
		exe = app
	}
	w.rc = buildConsoleRC(osKind, app, version, spec.Pattern{
		Type: spec.PatternConsoleApp,
		Exe:  exe,
	})
	return nil
}

func (w *consoleWorld) givenVerifyCommand(cmd string) error {
	if w.rc.Spec == nil {
		return fmt.Errorf("ReleaseCtx not initialised")
	}
	pat := w.rc.Spec.Pattern
	pat.VerifyCommand = cmd
	w.rc.Spec.Pattern = pat
	return nil
}

func (w *consoleWorld) givenMarker(version string) error {
	w.statusTx = &consoleScriptTransport{
		osKind: w.rc.Spec.Target.OS,
		queue:  []transport.Result{markerResult(version)},
	}
	return nil
}

func (w *consoleWorld) whenConfigureAndStart() error {
	w.configure = &consoleScriptTransport{osKind: w.rc.Spec.Target.OS}
	if err := w.c.Configure(context.Background(), w.configure, w.rc); err != nil {
		w.configErr = err
		return fmt.Errorf("Configure: %w", err)
	}
	w.start = &consoleScriptTransport{osKind: w.rc.Spec.Target.OS}
	if err := w.c.Start(context.Background(), w.start, w.rc); err != nil {
		return fmt.Errorf("Start: %w", err)
	}
	return nil
}

func (w *consoleWorld) whenStatus() error {
	if w.statusTx == nil {
		return fmt.Errorf("no fake transport with a marker was configured")
	}
	w.status, w.statusErr = w.c.Status(context.Background(), w.statusTx, w.rc)
	return nil
}

func (w *consoleWorld) thenStartNoScripts() error {
	if w.start == nil {
		return fmt.Errorf("Start was never invoked")
	}
	if n := len(w.start.scripts); n != 0 {
		return fmt.Errorf("Start must be a no-op, but it ran %d script(s)", n)
	}
	return nil
}

func (w *consoleWorld) thenVerifyMatchesGolden(name string) error {
	if len(w.configure.scripts) == 0 {
		return fmt.Errorf("Configure emitted no scripts")
	}
	// verify_command is the LAST script Configure emits (post_install precedes
	// it when present); this scenario sets only verify_command.
	got := w.configure.scripts[len(w.configure.scripts)-1]
	want, err := os.ReadFile(goldenPath(name))
	if err != nil {
		return fmt.Errorf("read golden %s: %w", name, err)
	}
	if got != string(want) {
		return fmt.Errorf("golden mismatch for %s.\n got: %q\nwant: %q", name, got, string(want))
	}
	return nil
}

func (w *consoleWorld) thenStatusIs(expected string) error {
	if w.statusErr != nil {
		return fmt.Errorf("Status returned error: %w", w.statusErr)
	}
	if w.status != expected {
		return fmt.Errorf("service status = %q, want %q", w.status, expected)
	}
	return nil
}

func InitializeScenario_single_target_deployment_patterns_pattern_interface_and_console_app(ctx *godog.ScenarioContext) {
	w := &consoleWorld{}
	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		w.reset()
		return ctx, nil
	})

	ctx.Step(`^a console_app ReleaseCtx for app "([^"]*)" version "([^"]*)" on "([^"]*)"$`, w.givenConsoleRC)
	ctx.Step(`^the verify_command is "([^"]*)"$`, w.givenVerifyCommand)
	ctx.Step(`^a fake transport whose on-host release marker reports version "([^"]*)"$`, w.givenMarker)
	ctx.Step(`^the Configure and Start scripts are generated over a fake transport$`, w.whenConfigureAndStart)
	ctx.Step(`^Status is computed via the fake transport$`, w.whenStatus)
	ctx.Step(`^Start ran no scripts on the transport$`, w.thenStartNoScripts)
	ctx.Step(`^the generated verify_command script matches the committed golden "([^"]*)"$`, w.thenVerifyMatchesGolden)
	ctx.Step(`^the reported service status is "([^"]*)"$`, w.thenStatusIs)
}

func TestE2E_single_target_deployment_patterns_pattern_interface_and_console_app(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_single_target_deployment_patterns_pattern_interface_and_console_app,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"single_target_deployment_patterns_pattern_interface_and_console_app.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
