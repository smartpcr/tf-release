package pattern

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// dockerRC builds a docker_container ReleaseCtx exactly the way engine.releaseCtx
// does: builtin env merged UNDER the spec environment (DESIGN §9.1). The docker
// D-step scripts (§9.6) are rendered from this context.
func dockerRC(osKind spec.OSKind, pat spec.Pattern, src spec.Source, env map[string]string) ReleaseCtx {
	root := "/opt/deploy"
	if osKind == spec.OSWindows {
		root = `C:\deploy`
	}
	p := layout.NewPaths(osKind, root, "sample-svc", "2.0.0")
	s := &spec.Deployment{
		Metadata:    spec.Metadata{Name: "sample-svc"},
		Target:      spec.Target{OS: osKind},
		Artifact:    spec.Artifact{Type: spec.ArtifactDocker, Version: "2.0.0", Source: src},
		Pattern:     pat,
		Environment: env,
	}
	merged := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: merged}
}

// dockerPattern is the reusable container spec (ports/env/volumes/restart) the
// golden scenarios exercise; run_args carries an extra runtime flag.
func dockerPattern() spec.Pattern {
	return spec.Pattern{
		Type:          spec.PatternDockerCont,
		ContainerName: "sample",
		Ports:         []string{"8080:8080", "9090:9090"},
		Volumes:       []string{"/srv/data:/data"},
		RestartPolicy: "always",
		RunArgs:       []string{"--pull=never"},
	}
}

// Scenario: Docker run script golden (DESIGN §9.6 D1..D7). Given a docker_container
// spec, when the D-step scripts are generated, they must match the committed
// goldens — including the login (D1) → tag pull (D2) → logout (D7) FETCH script
// and the rm -f (D4) → run (D5) START script with ports/env/volumes/restart.
func TestDockerRunScriptGoldenLinux(t *testing.T) {
	t.Setenv("LD_REG_PW_ENV", "s3cr3t")
	var d DockerContainer
	rc := dockerRC(spec.OSLinux, dockerPattern(), spec.Source{
		Type:  "docker_registry",
		Image: "registry.example.com/app",
		Tag:   "2.0.0",
		Auth:  spec.SourceAuth{Username: "svc", PasswordEnv: "LD_REG_PW_ENV"},
	}, map[string]string{"ASPNETCORE_ENVIRONMENT": "Production"})

	// FETCH = D1 (login) + D2 (pull) + D7 (logout).
	fp := &scriptTransport{osKind: spec.OSLinux}
	if err := d.Pull(context.Background(), fp, rc); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(fp.scripts) != 1 {
		t.Fatalf("Pull should emit a single FETCH script, got %d", len(fp.scripts))
	}
	checkGolden(t, "docker_pull_linux.golden", fp.scripts[0])
	// D1 login uses --password-stdin (credential never on the command line), and
	// D7 logout must run because D1 ran.
	if !strings.Contains(fp.scripts[0], "docker login 'registry.example.com' -u 'svc' --password-stdin") {
		t.Errorf("FETCH must perform D1 login via --password-stdin:\n%s", fp.scripts[0])
	}
	if !strings.Contains(fp.scripts[0], "docker logout 'registry.example.com'") {
		t.Errorf("FETCH must perform D7 logout because D1 login ran:\n%s", fp.scripts[0])
	}
	if !strings.Contains(fp.scripts[0], "docker pull 'registry.example.com/app:2.0.0'") {
		t.Errorf("FETCH must perform D2 tag pull:\n%s", fp.scripts[0])
	}

	// START = D4 (rm -f, ignore not-found) + D5 (run with ports/env/volumes/restart).
	fr := &scriptTransport{osKind: spec.OSLinux}
	if err := d.Start(context.Background(), fr, rc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(fr.scripts) != 1 {
		t.Fatalf("Start should emit a single START script, got %d", len(fr.scripts))
	}
	checkGolden(t, "docker_run_linux.golden", fr.scripts[0])
	if !strings.Contains(fr.scripts[0], "docker rm -f 'sample'") {
		t.Errorf("START must perform D4 rm -f:\n%s", fr.scripts[0])
	}
	for _, want := range []string{
		"--restart 'always'",
		"-p '8080:8080'", "-p '9090:9090'",
		"-v '/srv/data:/data'",
		`-e 'ASPNETCORE_ENVIRONMENT=Production'`,
		"--pull=never",
		"'registry.example.com/app:2.0.0'",
	} {
		if !strings.Contains(fr.scripts[0], want) {
			t.Errorf("START (D5) missing %q:\n%s", want, fr.scripts[0])
		}
	}
}

// Scenario: Docker digest pull golden (DESIGN §6.3/§9.6 D2). A source with a
// digest must pull by <image>@<digest>, NOT by tag, and with no auth configured
// no login/logout is emitted.
func TestDockerPullDigestGolden(t *testing.T) {
	var d DockerContainer
	rc := dockerRC(spec.OSLinux, dockerPattern(), spec.Source{
		Type:   "docker_registry",
		Image:  "registry.example.com/app",
		Digest: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}, nil)

	fp := &scriptTransport{osKind: spec.OSLinux}
	if err := d.Pull(context.Background(), fp, rc); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	checkGolden(t, "docker_pull_digest.golden", fp.scripts[0])
	if !strings.Contains(fp.scripts[0], "docker pull 'registry.example.com/app@sha256:deadbeef") {
		t.Errorf("digest pull must reference <image>@<digest>:\n%s", fp.scripts[0])
	}
	if strings.Contains(fp.scripts[0], "docker login") || strings.Contains(fp.scripts[0], "docker logout") {
		t.Errorf("no auth ⇒ no login/logout:\n%s", fp.scripts[0])
	}
}

// Scenario: Docker rollback run golden (DESIGN §9.6 D6). Rollback re-runs D4+D5
// against the RECORDED old image id (not the new ref), restoring the previous
// container.
func TestDockerRollbackRunGolden(t *testing.T) {
	var d DockerContainer
	rc := dockerRC(spec.OSLinux, dockerPattern(), spec.Source{
		Type:  "docker_registry",
		Image: "registry.example.com/app",
		Tag:   "2.0.0",
	}, nil)

	fr := &scriptTransport{osKind: spec.OSLinux}
	oldImage := "sha256:0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff"
	if err := d.RunNew(context.Background(), fr, rc, oldImage); err != nil {
		t.Fatalf("RunNew(rollback): %v", err)
	}
	checkGolden(t, "docker_run_rollback.golden", fr.scripts[0])
	if !strings.Contains(fr.scripts[0], oldImage) {
		t.Errorf("rollback run must target the recorded old image id %q:\n%s", oldImage, fr.scripts[0])
	}
	if strings.Contains(fr.scripts[0], "registry.example.com/app:2.0.0") {
		t.Errorf("rollback run must NOT use the new tag ref:\n%s", fr.scripts[0])
	}
}

// Scenario: Docker Windows script golden (DESIGN §9.6 on a windows/PS target).
// The FETCH and START scripts use PowerShell login/pull/run with $LASTEXITCODE
// gating; login/logout still bracket the pull.
func TestDockerRunScriptGoldenWindows(t *testing.T) {
	t.Setenv("LD_REG_PW_ENV", "s3cr3t")
	var d DockerContainer
	rc := dockerRC(spec.OSWindows, dockerPattern(), spec.Source{
		Type:  "docker_registry",
		Image: "registry.example.com/app",
		Tag:   "2.0.0",
		Auth:  spec.SourceAuth{Username: "svc", PasswordEnv: "LD_REG_PW_ENV"},
	}, map[string]string{"ASPNETCORE_ENVIRONMENT": "Production"})

	fp := &scriptTransport{osKind: spec.OSWindows}
	if err := d.Pull(context.Background(), fp, rc); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	checkGolden(t, "docker_pull_windows.golden", fp.scripts[0])
	if !strings.Contains(fp.scripts[0], "$env:LD_REG_PW | docker login 'registry.example.com' -u 'svc' --password-stdin") {
		t.Errorf("windows FETCH must login via --password-stdin:\n%s", fp.scripts[0])
	}
	if !strings.Contains(fp.scripts[0], "docker logout 'registry.example.com'") {
		t.Errorf("windows FETCH must logout because login ran:\n%s", fp.scripts[0])
	}

	fr := &scriptTransport{osKind: spec.OSWindows}
	if err := d.Start(context.Background(), fr, rc); err != nil {
		t.Fatalf("Start: %v", err)
	}
	checkGolden(t, "docker_run_windows.golden", fr.scripts[0])
	if !strings.Contains(fr.scripts[0], "docker rm -f 'sample'") {
		t.Errorf("windows START must perform D4 rm -f:\n%s", fr.scripts[0])
	}
}

// Scenario: D3 image-id inspect. CurrentImageID runs `docker inspect -f
// '{{.Image}}'` and returns the trimmed id; an absent container yields "" with
// no error. This is the recorded rollback anchor (DESIGN §9.6 D3) the engine
// persists into manifest.extra.image_id.
func TestDockerCurrentImageID(t *testing.T) {
	var d DockerContainer
	rc := dockerRC(spec.OSLinux, dockerPattern(), spec.Source{
		Type: "docker_registry", Image: "registry.example.com/app", Tag: "2.0.0",
	}, nil)

	present := &scriptTransport{osKind: spec.OSLinux, queue: []transport.Result{
		{ExitCode: 0, Stdout: "sha256:abc123\n"},
	}}
	got, err := d.CurrentImageID(context.Background(), present, rc)
	if err != nil {
		t.Fatalf("CurrentImageID: %v", err)
	}
	if got != "sha256:abc123" {
		t.Errorf("D3 must return the trimmed image id, got %q", got)
	}
	if len(present.scripts) != 1 || !strings.Contains(present.scripts[0], "docker inspect -f '{{.Image}}'") {
		t.Errorf("D3 must probe `docker inspect -f '{{.Image}}'`, scripts=%v", present.scripts)
	}

	absent := &scriptTransport{osKind: spec.OSLinux, queue: []transport.Result{{ExitCode: 0, Stdout: ""}}}
	got2, err2 := d.CurrentImageID(context.Background(), absent, rc)
	if err2 != nil || got2 != "" {
		t.Errorf("absent container ⇒ empty id, no error; got id=%q err=%v", got2, err2)
	}
}

// Scenario: unsafe environment/argument rendering (evaluator iter2 item 3). A
// container env value carrying shell metacharacters ($(), backtick, $VAR, a
// single quote) must be rendered as a single-quoted literal so neither sh nor
// PowerShell can interpret it — no command substitution, no variable expansion.
func TestDockerRunArgMetacharSafe(t *testing.T) {
	inj := "$(touch pwned)`id`$HOME he'llo"
	pat := dockerPattern()
	src := spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Tag: "2.0.0"}

	// Linux: single-quote with '\'' escaping for the embedded apostrophe.
	rcL := dockerRC(spec.OSLinux, pat, src, map[string]string{"INJ": inj})
	fL := &scriptTransport{osKind: spec.OSLinux}
	if err := (&DockerContainer{}).Start(context.Background(), fL, rcL); err != nil {
		t.Fatalf("Start(linux): %v", err)
	}
	wantL := `-e 'INJ=$(touch pwned)` + "`id`" + `$HOME he'\''llo'`
	if !strings.Contains(fL.scripts[0], wantL) {
		t.Errorf("linux env value must be single-quoted literal (no command substitution):\nwant substring %q\n got %s", wantL, fL.scripts[0])
	}
	// The metacharacters must never appear OUTSIDE a single-quoted context.
	if strings.Contains(fL.scripts[0], `-e "INJ=`) || strings.Contains(fL.scripts[0], "-e INJ=$(") {
		t.Errorf("linux env must not be double-quoted or bare (injection risk):\n%s", fL.scripts[0])
	}

	// Windows/PowerShell: single-quote with '' escaping for the apostrophe.
	rcW := dockerRC(spec.OSWindows, pat, src, map[string]string{"INJ": inj})
	fW := &scriptTransport{osKind: spec.OSWindows}
	if err := (&DockerContainer{}).Start(context.Background(), fW, rcW); err != nil {
		t.Fatalf("Start(windows): %v", err)
	}
	wantW := `-e 'INJ=$(touch pwned)` + "`id`" + `$HOME he''llo'`
	if !strings.Contains(fW.scripts[0], wantW) {
		t.Errorf("windows env value must be single-quoted literal (PowerShell no-expansion):\nwant substring %q\n got %s", wantW, fW.scripts[0])
	}
	// PowerShell backslash-quote escaping (the old, broken form) must be gone.
	if strings.Contains(fW.scripts[0], `\"`) {
		t.Errorf("windows env must not use invalid backslash-quote escaping:\n%s", fW.scripts[0])
	}
}

// Scenario: Docker preflight fail (DESIGN §8.1 / §9.6 D14). Given `docker version`
// failing via a fake transport with a scripted Result queue, Preflight must yield
// a StepError coded ERR_PREFLIGHT at step=PREFLIGHT.
func TestDockerPreflightFail(t *testing.T) {
	var d DockerContainer
	rc := dockerRC(spec.OSLinux, dockerPattern(), spec.Source{
		Type: "docker_registry", Image: "registry.example.com/app", Tag: "2.0.0",
	}, nil)

	// docker daemon unavailable: non-zero exit from `docker version`.
	fail := &scriptTransport{osKind: spec.OSLinux, queue: []transport.Result{
		{ExitCode: 1, Stderr: "Cannot connect to the Docker daemon"},
	}}
	err := d.Preflight(context.Background(), fail, rc)
	if err == nil {
		t.Fatal("Preflight must fail when `docker version` exits non-zero")
	}
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StepError, got %T: %v", err, err)
	}
	if se.Code != "ERR_PREFLIGHT" || se.Step != "PREFLIGHT" {
		t.Errorf("preflight failure must be step=PREFLIGHT code=ERR_PREFLIGHT, got step=%q code=%q", se.Step, se.Code)
	}
	if len(fail.scripts) != 1 || !strings.Contains(fail.scripts[0], "docker version") {
		t.Errorf("Preflight must probe `docker version`, scripts=%v", fail.scripts)
	}

	// Healthy daemon: exit 0 ⇒ no error.
	okT := &scriptTransport{osKind: spec.OSLinux, queue: []transport.Result{{ExitCode: 0, Stdout: "24.0.7"}}}
	if err := d.Preflight(context.Background(), okT, rc); err != nil {
		t.Fatalf("Preflight must succeed when `docker version` exits 0: %v", err)
	}
}

// Scenario: complete container-name quoting (evaluator iter3 item 4). The
// container name flows into D2 rm -f, D3 inspect (CurrentImageID), Status,
// Stop and Uninstall. A name carrying shell metacharacters must be rendered as
// a single-quoted literal on EVERY path so neither sh nor PowerShell can
// interpret it; the raw metacharacter sequence must never appear unquoted.
func TestDockerContainerNameMetacharSafe(t *testing.T) {
	inj := "svc; rm -rf / #$(id)"
	src := spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Tag: "2.0.0"}

	for _, os := range []spec.OSKind{spec.OSLinux, spec.OSWindows} {
		pat := dockerPattern()
		pat.ContainerName = inj
		rc := dockerRC(os, pat, src, nil)
		wantLit := "'" + inj + "'" // sh and PS both single-quote; no apostrophe in inj

		type probe struct {
			name string
			run  func(tr *scriptTransport) error
		}
		d := &DockerContainer{}
		probes := []probe{
			{"Start(rm -f + run)", func(tr *scriptTransport) error { return d.Start(context.Background(), tr, rc) }},
			{"CurrentImageID(inspect)", func(tr *scriptTransport) error {
				tr.queue = []transport.Result{{ExitCode: 0, Stdout: ""}}
				_, err := d.CurrentImageID(context.Background(), tr, rc)
				return err
			}},
			{"Status(inspect)", func(tr *scriptTransport) error {
				tr.queue = []transport.Result{{ExitCode: 0, Stdout: "running"}}
				_, err := d.Status(context.Background(), tr, rc)
				return err
			}},
			{"Stop", func(tr *scriptTransport) error { return d.Stop(context.Background(), tr, rc) }},
			{"Uninstall", func(tr *scriptTransport) error { return d.Uninstall(context.Background(), tr, rc, false) }},
		}
		for _, pr := range probes {
			tr := &scriptTransport{osKind: os}
			if err := pr.run(tr); err != nil {
				t.Fatalf("%s/%s: %v", os, pr.name, err)
			}
			joined := strings.Join(tr.scripts, "\n")
			if !strings.Contains(joined, wantLit) {
				t.Errorf("%s/%s: container name must be single-quoted literal %q:\n%s", os, pr.name, wantLit, joined)
			}
			// The bare, unquoted injection must never appear (would allow `rm -rf /`).
			bare := "-f " + inj
			if strings.Contains(joined, bare) {
				t.Errorf("%s/%s: container name rendered unquoted (injection):\n%s", os, pr.name, joined)
			}
		}
	}
}

// Scenario: same-version configuration fingerprint (evaluator iter3 item 3). The
// engine keys idempotency off ConfigFingerprint so a same-version change to the
// mutable container settings is NOT a silent no-op. The fingerprint must change
// when image ref, ports, env, volumes, restart policy or run_args change, and be
// stable (order-insensitive for ports/volumes/env) otherwise.
func TestDockerConfigFingerprint(t *testing.T) {
	d := &DockerContainer{}
	src := spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Tag: "2.0.0"}
	base := dockerRC(spec.OSLinux, dockerPattern(), src, map[string]string{"A": "1", "B": "2"})
	baseFP := d.ConfigFingerprint(base)

	// Identical config ⇒ identical fingerprint (no-op).
	if got := d.ConfigFingerprint(dockerRC(spec.OSLinux, dockerPattern(), src, map[string]string{"A": "1", "B": "2"})); got != baseFP {
		t.Errorf("identical config must produce identical fingerprint:\n base=%s\n got =%s", baseFP, got)
	}

	// Each mutable dimension must move the fingerprint.
	mut := map[string]ReleaseCtx{
		"image tag": dockerRC(spec.OSLinux, dockerPattern(),
			spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Tag: "2.0.1"},
			map[string]string{"A": "1", "B": "2"}),
	}
	patDigest := dockerPattern()
	mut["digest"] = dockerRC(spec.OSLinux, patDigest,
		spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Digest: "sha256:deadbeef"},
		map[string]string{"A": "1", "B": "2"})
	patPorts := dockerPattern()
	patPorts.Ports = []string{"1234:1234"}
	mut["ports"] = dockerRC(spec.OSLinux, patPorts, src, map[string]string{"A": "1", "B": "2"})
	patVols := dockerPattern()
	patVols.Volumes = []string{"/x:/y"}
	mut["volumes"] = dockerRC(spec.OSLinux, patVols, src, map[string]string{"A": "1", "B": "2"})
	patRestart := dockerPattern()
	patRestart.RestartPolicy = "no"
	mut["restart"] = dockerRC(spec.OSLinux, patRestart, src, map[string]string{"A": "1", "B": "2"})
	patArgs := dockerPattern()
	patArgs.RunArgs = []string{"--pull=always"}
	mut["run_args"] = dockerRC(spec.OSLinux, patArgs, src, map[string]string{"A": "1", "B": "2"})
	mut["env"] = dockerRC(spec.OSLinux, dockerPattern(), src, map[string]string{"A": "9", "B": "2"})
	for name, rc := range mut {
		if got := d.ConfigFingerprint(rc); got == baseFP {
			t.Errorf("changing %s must change the fingerprint, but it stayed %s", name, baseFP)
		}
	}

	// Order-insensitive for ports/volumes/env (map + sorted slices).
	patReorder := dockerPattern()
	patReorder.Ports = []string{"9090:9090", "8080:8080"}
	reorder := dockerRC(spec.OSLinux, patReorder, src, map[string]string{"B": "2", "A": "1"})
	if got := d.ConfigFingerprint(reorder); got != baseFP {
		t.Errorf("reordered ports/env must NOT change the fingerprint:\n base=%s\n got =%s", baseFP, got)
	}
}

