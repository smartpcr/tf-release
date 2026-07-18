//go:build e2e

package e2e

import (
	"context"
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

// goldenDir points at the committed windows_service golden fixtures the impl
// workstream owns (DESIGN §9.2). The e2e package runs with its own directory as
// the working directory, so we resolve testdata relative to the module root.
const goldenDir = "../../../internal/pattern/testdata"

// wsScriptTransport is a no-network fake Transport that records every script it
// is asked to run and replays a scripted FIFO queue of Results. It lets the
// golden scenarios exercise the REAL WindowsService.Configure verb (which emits
// PowerShell scripts) without any live Windows host.
type wsScriptTransport struct {
	osKind  spec.OSKind
	scripts []string
	queue   []transport.Result
}

func (f *wsScriptTransport) Connect(ctx context.Context) error { return nil }
func (f *wsScriptTransport) Close() error                      { return nil }
func (f *wsScriptTransport) OS() spec.OSKind                   { return f.osKind }
func (f *wsScriptTransport) Host() string                      { return "host1" }
func (f *wsScriptTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.scripts = append(f.scripts, c.Script)
	if len(f.queue) == 0 {
		return transport.Result{ExitCode: 0}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}
func (f *wsScriptTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	return nil
}
func (f *wsScriptTransport) Download(ctx context.Context, remote, local string) error { return nil }

// winsvcRC rebuilds a windows_service ReleaseCtx the same way engine.releaseCtx
// does: builtin env merged UNDER the spec environment (DESIGN §9.1).
func winsvcRC(pat spec.Pattern, env map[string]string) pattern.ReleaseCtx {
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "payments-svc", "1.2.0")
	s := &spec.Deployment{
		Metadata:    spec.Metadata{Name: "payments-svc"},
		Target:      spec.Target{OS: spec.OSWindows},
		Artifact:    spec.Artifact{Version: "1.2.0"},
		Pattern:     pat,
		Environment: env,
	}
	merged := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return pattern.ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: merged}
}

// wsState holds the per-scenario generation outputs.
type wsState struct {
	transport *wsScriptTransport
	scripts   []string          // S4 configure + S5 env scripts
	xml       string            // WinSW xml
	winswEnv  map[string]string // env for the WinSW xml scenario
}

func wsReadGolden(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// --- Scenario 1: S4 fresh vs update golden --------------------------------

func (s *wsState) givenWindowsServiceSpecWithRecoveryAndEnv(name string) error {
	restart := true
	rc := winsvcRC(spec.Pattern{
		Type:        spec.PatternWindowsService,
		ServiceName: name,
		DisplayName: "Contoso Payments",
		Description: "Handles payments",
		Exe:         `bin\Payments.exe`,
		Args:        []string{"--config", "appsettings.json"},
		StartType:   "auto",
		Account:     spec.ServiceAccount{Username: `CONTOSO\svc-pay`, PasswordEnv: "PAY_PW"},
		Recovery:    spec.Recovery{RestartOnFailure: &restart},
	}, map[string]string{"ASPNETCORE_ENVIRONMENT": "Production"})

	var w pattern.WindowsService
	f := &wsScriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f, rc); err != nil {
		return err
	}
	s.transport = f
	return nil
}

func (s *wsState) whenS4ScriptsGenerated() error {
	if s.transport == nil || len(s.transport.scripts) != 2 {
		return fmt.Errorf("windows_service Configure should emit S4 configure + S5 env scripts, got %d", len(s.transport.scripts))
	}
	s.scripts = s.transport.scripts
	return nil
}

func (s *wsState) thenS4MatchesGolden(name string) error {
	want, err := wsReadGolden(name)
	if err != nil {
		return err
	}
	if s.scripts[0] != want {
		return fmt.Errorf("S4 golden mismatch for %s:\n got: %q\nwant: %q", name, s.scripts[0], want)
	}
	return nil
}

func (s *wsState) thenS5MatchesGolden(name string) error {
	want, err := wsReadGolden(name)
	if err != nil {
		return err
	}
	if s.scripts[1] != want {
		return fmt.Errorf("S5 golden mismatch for %s:\n got: %q\nwant: %q", name, s.scripts[1], want)
	}
	return nil
}

func (s *wsState) thenS4HasBothBranches() error {
	if !strings.Contains(s.scripts[0], "sc.exe create") || !strings.Contains(s.scripts[0], "sc.exe config") {
		return fmt.Errorf("S4 script must contain both fresh (create) and update (config) branches:\n%s", s.scripts[0])
	}
	return nil
}

func (s *wsState) thenS4InstallsRecovery() error {
	if !strings.Contains(s.scripts[0], "sc.exe failure") || !strings.Contains(s.scripts[0], "restart/5000") {
		return fmt.Errorf("S4 script must install recovery (sc.exe failure) actions:\n%s", s.scripts[0])
	}
	return nil
}

// --- Scenario 2: WinSW xml golden -----------------------------------------

func (s *wsState) givenWinswSpec(wrapper string) error {
	if wrapper != "winsw" {
		return fmt.Errorf("unexpected wrapper %q", wrapper)
	}
	// Mirror the impl unit fixture inputs exactly (DESIGN §9.2 S4 winsw).
	s.winswEnv = map[string]string{
		"LD_APP":                 "payments-svc",
		"ASPNETCORE_ENVIRONMENT": "Production",
		"LD_VERSION":             "1.2.0",
	}
	return nil
}

func (s *wsState) whenWinswXMLGenerated() error {
	s.xml = pattern.WinswXML(
		"PaymentsSvc", "Contoso Payments", "Handles payments",
		`C:\deploy\payments-svc\current\bin\Payments.exe`,
		[]string{"--config", "appsettings.json"},
		`C:\deploy\payments-svc\shared\logs`, 45, s.winswEnv)
	return nil
}

func (s *wsState) thenWinswXMLMatchesGolden(name string) error {
	want, err := wsReadGolden(name)
	if err != nil {
		return err
	}
	if s.xml != want {
		return fmt.Errorf("WinSW xml golden mismatch for %s:\n got: %q\nwant: %q", name, s.xml, want)
	}
	return nil
}

func (s *wsState) thenXMLContains(substr string) error {
	if !strings.Contains(s.xml, substr) {
		return fmt.Errorf("WinSW xml must contain %q:\n%s", substr, s.xml)
	}
	return nil
}

func InitializeScenario_single_target_deployment_patterns_windows_service_pattern(ctx *godog.ScenarioContext) {
	st := &wsState{}
	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		*st = wsState{}
		return ctx, nil
	})

	ctx.Step(`^a windows_service spec for "([^"]*)" with recovery and env injection$`, st.givenWindowsServiceSpecWithRecoveryAndEnv)
	ctx.Step(`^the S4 scripts are generated for fresh install and update$`, st.whenS4ScriptsGenerated)
	ctx.Step(`^the S4 configure script matches the golden "([^"]*)"$`, st.thenS4MatchesGolden)
	ctx.Step(`^the S5 env-injection script matches the golden "([^"]*)"$`, st.thenS5MatchesGolden)
	ctx.Step(`^the S4 script contains both the fresh create and update config branches$`, st.thenS4HasBothBranches)
	ctx.Step(`^the S4 script installs the recovery actions$`, st.thenS4InstallsRecovery)

	ctx.Step(`^a windows_service spec with wrapper "([^"]*)"$`, st.givenWinswSpec)
	ctx.Step(`^the WinSW xml is generated$`, st.whenWinswXMLGenerated)
	ctx.Step(`^the WinSW xml matches the golden "([^"]*)"$`, st.thenWinswXMLMatchesGolden)
	ctx.Step(`^the WinSW xml contains the stopwait entry "([^"]*)"$`, st.thenXMLContains)
	ctx.Step(`^the WinSW xml contains the env entry "([^"]*)"$`, st.thenXMLContains)
}

func TestE2E_single_target_deployment_patterns_windows_service_pattern(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_single_target_deployment_patterns_windows_service_pattern,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"single_target_deployment_patterns_windows_service_pattern.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run windows_service e2e feature tests")
	}
}


