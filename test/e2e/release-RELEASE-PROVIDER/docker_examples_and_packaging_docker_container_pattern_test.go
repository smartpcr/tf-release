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

// scriptTransportDCP is an in-process fake Transport: it records every script
// Exec sees (in order) and replays a scripted FIFO queue of Results. No network,
// fully deterministic — the "fake Transport with a scripted Result queue" the
// docker preflight scenario calls for, and enough to capture the D-step scripts
// for golden comparison. It is per-stage-suffixed so sibling stages sharing
// package e2e do not collide.
type scriptTransportDCP struct {
	osKind  spec.OSKind
	host    string
	scripts []string
	queue   []transport.Result
}

func (f *scriptTransportDCP) Connect(ctx context.Context) error { return nil }
func (f *scriptTransportDCP) Close() error                      { return nil }
func (f *scriptTransportDCP) OS() spec.OSKind                   { return f.osKind }
func (f *scriptTransportDCP) Host() string {
	if f.host == "" {
		return "host1"
	}
	return f.host
}

func (f *scriptTransportDCP) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.scripts = append(f.scripts, c.Script)
	if len(f.queue) == 0 {
		return transport.Result{ExitCode: 0}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}

func (f *scriptTransportDCP) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	return nil
}
func (f *scriptTransportDCP) Download(ctx context.Context, remote, local string) error { return nil }

// dockerReleaseCtxDCP mirrors engine.releaseCtx for a docker_container deployment
// exactly the way the sibling unit test dockerRC does: builtin env merged UNDER
// the spec environment (DESIGN §9.1). The D-step scripts (§9.6) are rendered
// from this context, so the goldens only match when the layout/env is identical.
func dockerReleaseCtxDCP(pat spec.Pattern, src spec.Source, env map[string]string) pattern.ReleaseCtx {
	p := layout.NewPaths(spec.OSLinux, "/opt/deploy", "sample-svc", "2.0.0")
	s := &spec.Deployment{
		Metadata:    spec.Metadata{Name: "sample-svc"},
		Target:      spec.Target{OS: spec.OSLinux},
		Artifact:    spec.Artifact{Type: spec.ArtifactDocker, Version: "2.0.0", Source: src},
		Pattern:     pat,
		Environment: env,
	}
	merged := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return pattern.ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: merged}
}

// dockerPatternDCP is the reusable container spec (ports/env/volumes/restart) the
// golden scenarios exercise; run_args carries an extra runtime flag. Mirrors the
// sibling unit test dockerPattern so the committed goldens apply verbatim.
func dockerPatternDCP() spec.Pattern {
	return spec.Pattern{
		Type:          spec.PatternDockerCont,
		ContainerName: "sample",
		Ports:         []string{"8080:8080", "9090:9090"},
		Volumes:       []string{"/srv/data:/data"},
		RestartPolicy: "always",
		RunArgs:       []string{"--pull=never"},
	}
}

// dcpWorld carries state across the Given/When/Then steps of one scenario.
type dcpWorld struct {
	image string
	tag   string

	fetchScript    string
	startScript    string
	digestScript   string
	rollbackScript string

	preflightTransport *scriptTransportDCP
	preflightErr       error
}

func (w *dcpWorld) givenDockerSpec(image, tag string) error {
	w.image = image
	w.tag = tag
	return nil
}

// whenGenerateDSteps drives the REAL docker_container verbs the engine runs so
// the emitted scripts are production output, not hand-written fixtures:
//   - FETCH  = D1 login + D2 tag pull + D7 logout (auth configured)
//   - START  = D4 rm -f + D5 run with ports/env/volumes/restart
//   - digest = D2 digest pull (no auth ⇒ no login/logout)
//   - rollback = D4 rm -f + D5 run against the RECORDED old image id (D6)
func (w *dcpWorld) whenGenerateDSteps() error {
	// The password value never appears in the rendered script, but Pull reads it
	// from the env var named by the spec; set it so the login path is exercised.
	os.Setenv("LD_REG_PW_ENV", "s3cr3t")

	var d pattern.DockerContainer

	authSrc := spec.Source{
		Type:  "docker_registry",
		Image: w.image,
		Tag:   w.tag,
		Auth:  spec.SourceAuth{Username: "svc", PasswordEnv: "LD_REG_PW_ENV"},
	}
	authRC := dockerReleaseCtxDCP(dockerPatternDCP(), authSrc,
		map[string]string{"ASPNETCORE_ENVIRONMENT": "Production"})

	fetch := &scriptTransportDCP{osKind: spec.OSLinux}
	if err := d.Pull(context.Background(), fetch, authRC); err != nil {
		return fmt.Errorf("Pull (FETCH): %w", err)
	}
	if len(fetch.scripts) != 1 {
		return fmt.Errorf("Pull should emit a single FETCH script, got %d", len(fetch.scripts))
	}
	w.fetchScript = fetch.scripts[0]

	start := &scriptTransportDCP{osKind: spec.OSLinux}
	if err := d.Start(context.Background(), start, authRC); err != nil {
		return fmt.Errorf("Start (START): %w", err)
	}
	if len(start.scripts) != 1 {
		return fmt.Errorf("Start should emit a single START script, got %d", len(start.scripts))
	}
	w.startScript = start.scripts[0]

	digestSrc := spec.Source{
		Type:   "docker_registry",
		Image:  w.image,
		Digest: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	digestRC := dockerReleaseCtxDCP(dockerPatternDCP(), digestSrc, nil)
	digest := &scriptTransportDCP{osKind: spec.OSLinux}
	if err := d.Pull(context.Background(), digest, digestRC); err != nil {
		return fmt.Errorf("Pull (digest): %w", err)
	}
	w.digestScript = digest.scripts[0]

	rollSrc := spec.Source{Type: "docker_registry", Image: w.image, Tag: w.tag}
	rollRC := dockerReleaseCtxDCP(dockerPatternDCP(), rollSrc, nil)
	oldImage := "sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"
	roll := &scriptTransportDCP{osKind: spec.OSLinux}
	if err := d.RunNew(context.Background(), roll, rollRC, oldImage); err != nil {
		return fmt.Errorf("RunNew (rollback): %w", err)
	}
	w.rollbackScript = roll.scripts[0]

	return nil
}

func matchGolden(name, got string) error {
	goldenPath := filepath.Join("..", "..", "..", "internal", "pattern", "testdata", name)
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", goldenPath, err)
	}
	if got != string(want) {
		return fmt.Errorf("golden mismatch for %s.\n got: %q\nwant: %q", name, got, string(want))
	}
	return nil
}

func (w *dcpWorld) thenFetchMatches(name string) error  { return matchGolden(name, w.fetchScript) }
func (w *dcpWorld) thenStartMatches(name string) error  { return matchGolden(name, w.startScript) }
func (w *dcpWorld) thenDigestMatches(name string) error { return matchGolden(name, w.digestScript) }
func (w *dcpWorld) thenRollbackMatches(name string) error {
	return matchGolden(name, w.rollbackScript)
}

// givenDockerVersionFails arms the fake transport so the first Exec (the
// `docker version` probe) returns a non-zero exit, modeling an unavailable
// docker daemon (DESIGN §8.1 / §9.6 D14).
func (w *dcpWorld) givenDockerVersionFails() error {
	w.preflightTransport = &scriptTransportDCP{osKind: spec.OSLinux, queue: []transport.Result{
		{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"},
	}}
	return nil
}

func (w *dcpWorld) whenPreflightRuns() error {
	var d pattern.DockerContainer
	rc := dockerReleaseCtxDCP(dockerPatternDCP(), spec.Source{
		Type: "docker_registry", Image: w.image, Tag: w.tag,
	}, nil)
	w.preflightErr = d.Preflight(context.Background(), w.preflightTransport, rc)
	return nil
}

func (w *dcpWorld) thenPreflightYields(code, step string) error {
	if w.preflightErr == nil {
		return errors.New("Preflight must fail when `docker version` exits non-zero")
	}
	var se *pattern.StepError
	if !errors.As(w.preflightErr, &se) {
		return fmt.Errorf("expected *pattern.StepError, got %T: %v", w.preflightErr, w.preflightErr)
	}
	if se.Code != code || se.Step != step {
		return fmt.Errorf("preflight failure must be step=%q code=%q, got step=%q code=%q", step, code, se.Step, se.Code)
	}
	// Prove Preflight actually probed `docker version` on the transport (not a
	// vacuous pass) — the acceptance scenario names the failing command.
	if len(w.preflightTransport.scripts) != 1 {
		return fmt.Errorf("Preflight must run exactly one probe, got %d", len(w.preflightTransport.scripts))
	}
	if got := w.preflightTransport.scripts[0]; !strings.Contains(got, "docker version") {
		return fmt.Errorf("Preflight must probe `docker version`, got: %q", got)
	}
	return nil
}

func InitializeScenario_docker_examples_and_packaging_docker_container_pattern(ctx *godog.ScenarioContext) {
	w := &dcpWorld{}

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		*w = dcpWorld{}
		return ctx, nil
	})

	ctx.Step(`^a docker_container spec for image "([^"]*)" tagged "([^"]*)" with registry auth$`, w.givenDockerSpec)
	ctx.Step(`^the D1\.\.D7 scripts are generated$`, w.whenGenerateDSteps)
	ctx.Step(`^the FETCH script matches the committed golden "([^"]*)"$`, w.thenFetchMatches)
	ctx.Step(`^the START script matches the committed golden "([^"]*)"$`, w.thenStartMatches)
	ctx.Step(`^the digest pull script matches the committed golden "([^"]*)"$`, w.thenDigestMatches)
	ctx.Step(`^the rollback run script matches the committed golden "([^"]*)"$`, w.thenRollbackMatches)

	ctx.Step(`^"docker version" fails via a fake transport with a scripted Result queue$`, w.givenDockerVersionFails)
	ctx.Step(`^Preflight runs$`, w.whenPreflightRuns)
	ctx.Step(`^it yields a StepError coded "([^"]*)" at step "([^"]*)"$`, w.thenPreflightYields)
}

func TestE2E_docker_examples_and_packaging_docker_container_pattern(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_docker_examples_and_packaging_docker_container_pattern,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"docker_examples_and_packaging_docker_container_pattern.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run docker container pattern e2e feature")
	}
}
