//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/pattern"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// scriptTransportNDS is an in-process fake Transport: it records every script
// Exec sees (in order) and replays a scripted FIFO queue of Results. No network,
// fully deterministic — the "fake Transport with a scripted Result queue" the
// node install_deps scenario calls for, and enough to capture the dotnet_api S4
// configure script for golden comparison. It is per-stage-suffixed so sibling
// stages sharing package e2e do not collide.
type scriptTransportNDS struct {
	osKind  spec.OSKind
	host    string
	scripts []string
	queue   []transport.Result
}

func (f *scriptTransportNDS) Connect(ctx context.Context) error { return nil }
func (f *scriptTransportNDS) Close() error                      { return nil }
func (f *scriptTransportNDS) OS() spec.OSKind                   { return f.osKind }
func (f *scriptTransportNDS) Host() string {
	if f.host == "" {
		return "host1"
	}
	return f.host
}

func (f *scriptTransportNDS) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.scripts = append(f.scripts, c.Script)
	if len(f.queue) == 0 {
		return transport.Result{ExitCode: 0}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}

func (f *scriptTransportNDS) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	return nil
}
func (f *scriptTransportNDS) Download(ctx context.Context, remote, local string) error { return nil }

// dotnetReleaseCtx mirrors engine.releaseCtx for a dotnet_api deployment: builtin
// env merged UNDER the spec environment (DESIGN §9.1).
func dotnetReleaseCtx(pat spec.Pattern, env map[string]string) pattern.ReleaseCtx {
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "web-api", "2.1.0")
	s := &spec.Deployment{
		Metadata:    spec.Metadata{Name: "web-api"},
		Target:      spec.Target{OS: spec.OSWindows},
		Artifact:    spec.Artifact{Version: "2.1.0"},
		Pattern:     pat,
		Environment: env,
	}
	merged := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return pattern.ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: merged}
}

// nodeReleaseCtx mirrors engine.releaseCtx for a node_web_app deployment.
func nodeReleaseCtx(port int, specEnv map[string]string) pattern.ReleaseCtx {
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "web", "1.0.0")
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: "web"},
		Target:   spec.Target{OS: spec.OSWindows},
		Artifact: spec.Artifact{Version: "1.0.0"},
		Pattern: spec.Pattern{
			Type:  spec.PatternNodeWebApp,
			Entry: "server.js",
			Port:  port,
		},
		Environment: specEnv,
	}
	env := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, s.Pattern.Port), s.Environment)
	return pattern.ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: env}
}

// ndsWorld carries state across the Given/When/Then steps of one scenario.
type ndsWorld struct {
	dotnetPat spec.Pattern
	rc        pattern.ReleaseCtx
	transport *scriptTransportNDS

	installDeps bool
	lockfile    string

	err error
}

// dotnetDllBinPath is the normative DESIGN §9.4 binPath the dotnet_dll launcher
// must render: `\"<dotnet_exe>\" \"<cur>\<dll>\" <args>` with the dll ALWAYS
// quoted so the SCM parses launcher and dll as two distinct tokens.
const dotnetDllBinPath = `\"C:\Program Files\dotnet\dotnet.exe\" \"C:\deploy\web-api\current\WebApi.dll\" --environment Production`

func (w *ndsWorld) givenDotnetRelease(launcher, dll string) error {
	w.dotnetPat = spec.Pattern{
		Type:        spec.PatternDotnetAPI,
		ServiceName: "WebApi",
		DisplayName: "Contoso Web API",
		Description: "Web API",
		Launcher:    launcher,
		DLL:         dll,
		Hosting:     "windows_service_native",
		StartType:   "auto",
		URLs:        "http://+:8088",
	}
	return nil
}

func (w *ndsWorld) givenDotnetExe(exe, args string) error {
	w.dotnetPat.DotnetExe = exe
	if args != "" {
		w.dotnetPat.Args = strings.Fields(args)
	}
	return nil
}

func (w *ndsWorld) whenGenerateS4() error {
	var d pattern.DotnetAPI
	w.rc = dotnetReleaseCtx(w.dotnetPat, nil)
	w.transport = &scriptTransportNDS{osKind: spec.OSWindows}
	w.err = d.Configure(context.Background(), w.transport, w.rc)
	return nil
}

func (w *ndsWorld) thenS4MatchesGolden(name string) error {
	if w.err != nil {
		return fmt.Errorf("Configure(dotnet_dll) failed: %w", w.err)
	}
	if len(w.transport.scripts) != 2 {
		return fmt.Errorf("dotnet_dll Configure should emit S4 configure + S5 env scripts, got %d", len(w.transport.scripts))
	}
	goldenPath := filepath.Join("..", "..", "..", "internal", "pattern", "testdata", name)
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", goldenPath, err)
	}
	if got := w.transport.scripts[0]; got != string(want) {
		return fmt.Errorf("S4 golden mismatch for %s.\n got: %q\nwant: %q", name, got, string(want))
	}
	return nil
}

func (w *ndsWorld) thenS4ContainsBinPath() error {
	if len(w.transport.scripts) == 0 {
		return errors.New("no S4 script was generated")
	}
	s4 := w.transport.scripts[0]
	if !strings.Contains(s4, dotnetDllBinPath) {
		return fmt.Errorf("S4 binPath must be the quoted `\"<dotnet_exe>\" \"<cur>\\<dll>\" <args>` form.\nwant substring: %q\ngot script:\n%s", dotnetDllBinPath, s4)
	}
	// Regression: the dll must NOT appear unquoted (the old quoteArgs-only path
	// dropped the quotes when the dll path had no spaces).
	if strings.Contains(s4, `dotnet.exe\" C:\deploy\web-api\current\WebApi.dll`) {
		return fmt.Errorf("dll path must be quoted, found bare dll token:\n%s", s4)
	}
	return nil
}

func (w *ndsWorld) givenNodeInstallDeps() error {
	w.installDeps = true
	return nil
}

func (w *ndsWorld) givenNoLockfile(name string) error {
	// Capture the lockfile the guard must name so the When/Then can assert the
	// REAL generated install script references it (the absent file itself is
	// modeled by the fake transport replaying the remote guard's exit=46 result).
	w.lockfile = name
	return nil
}

func (w *ndsWorld) whenNodeInstallDeps() error {
	var n pattern.NodeWebApp
	w.rc = nodeReleaseCtx(3000, nil)
	w.rc.Spec.Pattern.InstallDeps = w.installDeps
	// Drive the REAL node lifecycle the engine runs (DESIGN §9.4): Preflight
	// gates node/npm on PATH (first Exec => exit 0), then the STAGE `npm ci`
	// install runs. The remote `Test-Path` guard exits 46 (ERR_SERVICE_INSTALL)
	// when the lockfile is absent; the fake transport replays those two scripted
	// results in order so the exit-46 => ERR_SERVICE_INSTALL mapping is exercised
	// end to end. The scripted stderr is only a realistic remote stub — the
	// acceptance clause "names the lockfile" is proven against the GENERATED
	// install script (scripts[1]) in thenFailureNamesLockfile, NOT against this
	// injected string, so a regression that dropped the guard/sentinel is caught.
	w.transport = &scriptTransportNDS{osKind: spec.OSWindows, queue: []transport.Result{
		{ExitCode: 0}, // Preflight: node/npm present on PATH
		{ExitCode: pattern.ExitSvcInstall, Stderr: w.lockfile + " missing (required for npm ci)"},
	}}
	if err := n.Preflight(context.Background(), w.transport, w.rc); err != nil {
		w.err = err
		return nil
	}
	w.err = n.InstallDeps(context.Background(), w.transport, w.rc)
	return nil
}

func (w *ndsWorld) thenFailsWithCode(code string) error {
	if w.err == nil {
		return errors.New("Preflight/InstallDeps must fail when package-lock.json is missing")
	}
	var se *pattern.StepError
	if !errors.As(w.err, &se) {
		return fmt.Errorf("expected *pattern.StepError, got %T: %v", w.err, w.err)
	}
	if se.Code != code {
		return fmt.Errorf("missing lockfile must map to %s, got %q", code, se.Code)
	}
	return nil
}

// thenFailureNamesLockfile proves the acceptance clause "yields ERR_SERVICE_INSTALL
// NAMING the lockfile" against PRODUCTION output: the REAL generated install
// script (scripts[1], emitted by NodeWebApp.InstallDeps) must carry the
// lockfile-naming guard and the exit-46 (ExitSvcInstall) sentinel. Asserting the
// returned error's message would be circular — failFrom builds that message from
// the stderr this test itself injected, so it would stay green even if the real
// script dropped the guard or the sentinel. Inspecting the emitted script mirrors
// the sibling unit test TestNodeInstallDepsMissingLockfile and actually catches
// such a regression. It also confirms both lifecycle steps (Preflight + npm ci
// install) executed against the transport.
func (w *ndsWorld) thenFailureNamesLockfile(name string) error {
	if w.err == nil {
		return errors.New("expected a failure that names the lockfile, got nil error")
	}
	// The Preflight gate and the STAGE npm ci install must both have run (2 Execs).
	if len(w.transport.scripts) != 2 {
		return fmt.Errorf("expected Preflight + npm ci install to run (2 scripts), got %d", len(w.transport.scripts))
	}
	install := w.transport.scripts[1]
	// Prove the GENERATED guard names the required lockfile — production output,
	// not the stderr the fake transport supplied.
	if !strings.Contains(install, name) {
		return fmt.Errorf("generated install script must name the required lockfile %q:\n%s", name, install)
	}
	// Prove the missing-lockfile guard exits with the ERR_SERVICE_INSTALL sentinel
	// (46, DESIGN §12) — the exit code the fake transport replays and thenFailsWithCode
	// maps back to ERR_SERVICE_INSTALL.
	if !strings.Contains(install, "exit 46") {
		return fmt.Errorf("missing-lockfile guard must exit with the ERR_SERVICE_INSTALL sentinel (exit 46):\n%s", install)
	}
	// Prove the install runs `npm ci --omit=dev` in the RELEASE dir (before switch).
	if !strings.Contains(install, "npm ci --omit=dev") {
		return fmt.Errorf("install step must run `npm ci --omit=dev`:\n%s", install)
	}
	if !strings.Contains(install, w.rc.P.Release) {
		return fmt.Errorf("npm ci must run in the release dir %q (before switch):\n%s", w.rc.P.Release, install)
	}
	return nil
}

func InitializeScenario_single_target_deployment_patterns_node_and_dotnet_service_patterns(ctx *godog.ScenarioContext) {
	w := &ndsWorld{}

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		*w = ndsWorld{}
		return ctx, nil
	})

	ctx.Step(`^a dotnet_api release with launcher "([^"]*)" and dll "([^"]*)"$`, w.givenDotnetRelease)
	ctx.Step(`^the dotnet executable is "([^"]*)" with args "([^"]*)"$`, w.givenDotnetExe)
	ctx.Step(`^the S4 configure script is generated$`, w.whenGenerateS4)
	ctx.Step(`^the generated S4 script matches the committed golden "([^"]*)"$`, w.thenS4MatchesGolden)
	ctx.Step(`^the S4 script contains the normative quoted dotnet binPath$`, w.thenS4ContainsBinPath)

	ctx.Step(`^a node_web_app release with install_deps enabled$`, w.givenNodeInstallDeps)
	ctx.Step(`^the release directory has no "([^"]*)"$`, w.givenNoLockfile)
	ctx.Step(`^node Preflight and the install_deps step run via the fake transport$`, w.whenNodeInstallDeps)
	ctx.Step(`^it fails with code "([^"]*)"$`, w.thenFailsWithCode)
	ctx.Step(`^the failure names the lockfile "([^"]*)"$`, w.thenFailureNamesLockfile)
}

func TestE2E_single_target_deployment_patterns_node_and_dotnet_service_patterns(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_single_target_deployment_patterns_node_and_dotnet_service_patterns,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"single_target_deployment_patterns_node_and_dotnet_service_patterns.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run node/dotnet service patterns e2e feature")
	}
}
