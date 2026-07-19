//go:build e2e

package e2e

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/pattern"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// cgspTransport is the in-process "fake Transport with a scripted Result queue"
// the acceptance scenarios call for: it records every script Exec receives (in
// order) and replays a FIFO queue of Results. No network, fully deterministic.
type cgspTransport struct {
	osKind  spec.OSKind
	scripts []string
	queue   []transport.Result
}

func (f *cgspTransport) Connect(ctx context.Context) error { return nil }
func (f *cgspTransport) Close() error                      { return nil }
func (f *cgspTransport) OS() spec.OSKind                   { return f.osKind }
func (f *cgspTransport) Host() string                      { return "host1" }
func (f *cgspTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.scripts = append(f.scripts, c.Script)
	if len(f.queue) == 0 {
		return transport.Result{ExitCode: 0}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}
func (f *cgspTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	return nil
}
func (f *cgspTransport) Download(ctx context.Context, remote, local string) error { return nil }

// cgspGoldenDir resolves internal/pattern/testdata relative to THIS source file so
// the golden lookup is independent of the process working directory.
func cgspGoldenDir() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "internal", "pattern", "testdata")
}

func cgspReadGolden(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cgspGoldenDir(), name))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// clusterWorld holds per-scenario state.
type clusterWorld struct {
	c        pattern.ClusterGeneric
	f        *cgspTransport
	service  string
	role     string
	staticIP string
	owners   []string

	// captured coordinator scripts (golden scenario)
	createScript    string
	preferredScript string
	moveScript      string

	// preflight-conflict outcome
	preflightErr error
}

func (w *clusterWorld) reset() {
	w.c = pattern.ClusterGeneric{}
	w.f = &cgspTransport{osKind: spec.OSWindows}
	w.service, w.role, w.staticIP = "", "", ""
	w.owners = nil
	w.createScript, w.preferredScript, w.moveScript = "", "", ""
	w.preflightErr = nil
}

func (w *clusterWorld) givenCreateSpec(service, role, staticIP string) error {
	w.service, w.role, w.staticIP = service, role, staticIP
	return nil
}

func (w *clusterWorld) givenPreferredOwners(ordering string) error {
	w.owners = strings.Split(ordering, ",")
	return nil
}

func (w *clusterWorld) whenGenerateCreateAndMove() error {
	ctx := context.Background()
	if err := w.c.CreateRole(ctx, w.f, w.service, w.role, w.staticIP); err != nil {
		return err
	}
	if err := w.c.SetPreferredOwners(ctx, w.f, w.role, w.owners); err != nil {
		return err
	}
	if err := w.c.StartGroup(ctx, w.f, w.role, 120); err != nil {
		return err
	}
	if err := w.c.MoveGroup(ctx, w.f, w.role, w.owners[0], 300); err != nil {
		return err
	}
	if len(w.f.scripts) < 4 {
		return cgspErrWrap("expected at least 4 coordinator scripts, got", len(w.f.scripts))
	}
	w.createScript = w.f.scripts[0]
	w.preferredScript = w.f.scripts[1]
	w.moveScript = w.f.scripts[3]
	return nil
}

func (w *clusterWorld) thenMatchesGolden(target, name string) error {
	var got string
	switch target {
	case "create-role":
		got = w.createScript
	case "preferred-owners":
		got = w.preferredScript
	case "move-group":
		got = w.moveScript
	default:
		return cgspErrStr("unknown script target " + target)
	}
	want, err := cgspReadGolden(name)
	if err != nil {
		return err
	}
	if got != want {
		return cgspErrStr("golden mismatch for " + name + ":\n got: " + got + "\nwant: " + want)
	}
	return nil
}

func (w *clusterWorld) thenScriptContains(target, sub string) error {
	var got string
	switch target {
	case "create-role":
		got = w.createScript
	case "preferred-owners":
		got = w.preferredScript
	case "move-group":
		got = w.moveScript
	default:
		return cgspErrStr("unknown script target " + target)
	}
	if !strings.Contains(got, sub) {
		return cgspErrStr("expected " + target + " script to contain " + sub + ", got:\n" + got)
	}
	return nil
}

func (w *clusterWorld) givenExistingBinding(role, boundSvc, owner string) error {
	w.role = role
	w.f.queue = []transport.Result{
		{ExitCode: 0, Stdout: "PRESENT|" + boundSvc + "|" + owner + "|Online"},
	}
	return nil
}

func (w *clusterWorld) whenPreflight(wantSvc string) error {
	w.service = wantSvc
	exists, owner, err := w.c.PreflightRole(context.Background(), w.f, w.role, wantSvc)
	w.preflightErr = err
	if err == nil {
		return cgspErrStr("PreflightRole must fail on a conflicting binding (exists=" +
			cgspBoolStr(exists) + " owner=" + owner + ")")
	}
	return nil
}

func (w *clusterWorld) thenFailsWithCode(code string) error {
	var se *pattern.StepError
	if !errors.As(w.preflightErr, &se) {
		return cgspErrStr("expected *pattern.StepError, got a different error type")
	}
	if se.Code != code {
		return cgspErrStr("expected code " + code + ", got " + se.Code)
	}
	return nil
}

func (w *clusterWorld) thenNamesBoth(a, b string) error {
	msg := w.preflightErr.Error()
	if !strings.Contains(msg, a) || !strings.Contains(msg, b) {
		return cgspErrStr("conflict error must name both " + a + " and " + b + ", got: " + msg)
	}
	return nil
}

func (w *clusterWorld) thenNothingModified() error {
	if len(w.f.scripts) != 1 {
		return cgspErrWrap("conflict preflight must run exactly one (read-only) script, got", len(w.f.scripts))
	}
	if !strings.Contains(w.f.scripts[0], "Get-ClusterResource") {
		return cgspErrStr("the single script must be the read-only role-binding probe:\n" + w.f.scripts[0])
	}
	for _, banned := range []string{
		"Add-ClusterGenericServiceRole", "Set-ClusterOwnerNode",
		"Move-ClusterGroup", "Start-ClusterGroup", "Remove-ClusterGroup",
	} {
		if strings.Contains(w.f.scripts[0], banned) {
			return cgspErrStr("conflict preflight must not emit a mutating cmdlet (" + banned + ")")
		}
	}
	return nil
}

// --- tiny error helpers (avoid fmt for terse messages) ---

type cgspErrString string

func (e cgspErrString) Error() string { return string(e) }
func cgspErrStr(s string) error      { return cgspErrString(s) }
func cgspErrWrap(s string, n int) error {
	return cgspErrString(s + " " + cgspItoa(n))
}
func cgspItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
func cgspBoolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func InitializeScenario_failover_cluster_support_cluster_generic_service_pattern_scripts(ctx *godog.ScenarioContext) {
	w := &clusterWorld{}

	ctx.Before(func(c context.Context, sc *godog.Scenario) (context.Context, error) {
		w.reset()
		return c, nil
	})

	ctx.Step(`^a cluster_generic_service spec with service "([^"]*)" role "([^"]*)" static address "([^"]*)"$`, w.givenCreateSpec)
	ctx.Step(`^the preferred owner ordering "([^"]*)"$`, w.givenPreferredOwners)
	ctx.Step(`^the create and move scripts are generated$`, w.whenGenerateCreateAndMove)
	ctx.Step(`^the create-role script matches the committed golden "([^"]*)"$`, func(name string) error {
		return w.thenMatchesGolden("create-role", name)
	})
	ctx.Step(`^the create-role script passes "([^"]*)"$`, func(sub string) error {
		return w.thenScriptContains("create-role", sub)
	})
	ctx.Step(`^the preferred-owners script matches the committed golden "([^"]*)"$`, func(name string) error {
		return w.thenMatchesGolden("preferred-owners", name)
	})
	ctx.Step(`^the preferred-owners script preserves the owner ordering "([^"]*)"$`, func(sub string) error {
		return w.thenScriptContains("preferred-owners", sub)
	})
	ctx.Step(`^the move-group script matches the committed golden "([^"]*)"$`, func(name string) error {
		return w.thenMatchesGolden("move-group", name)
	})

	ctx.Step(`^a cluster role "([^"]*)" already bound to service "([^"]*)" owned by "([^"]*)"$`, w.givenExistingBinding)
	ctx.Step(`^preflight runs for spec service "([^"]*)" via a fake transport$`, w.whenPreflight)
	ctx.Step(`^it fails with code "([^"]*)"$`, w.thenFailsWithCode)
	ctx.Step(`^the error names both service "([^"]*)" and service "([^"]*)"$`, w.thenNamesBoth)
	ctx.Step(`^nothing is modified — only the read-only role-binding probe ran$`, w.thenNothingModified)
}

func TestE2E_failover_cluster_support_cluster_generic_service_pattern_scripts(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_failover_cluster_support_cluster_generic_service_pattern_scripts,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"failover_cluster_support_cluster_generic_service_pattern_scripts.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: e2e scenarios failed")
	}
}
