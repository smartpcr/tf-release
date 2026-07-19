package pattern

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// DockerContainer implements DESIGN §9.6 (D-steps). Works on linux (sh) and
// windows (PS) targets; docker CLI presence gated in Preflight (D14).
type DockerContainer struct{}

func (d *DockerContainer) shell(t transport.Transport) transport.Shell {
	if t.OS() == spec.OSWindows {
		return transport.ShellPowerShell
	}
	return transport.ShellSh
}

func (d *DockerContainer) run(ctx context.Context, t transport.Transport, step, script string, env map[string]string, timeout int) (transport.Result, error) {
	r, err := t.Exec(ctx, transport.Cmd{Shell: d.shell(t), Script: script, Env: env, TimeoutSec: timeout})
	if err != nil {
		return r, stepErr("ERR_CONNECT", t.Host(), step, err)
	}
	return r, nil
}

func (d *DockerContainer) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	r, err := d.run(ctx, t, "PREFLIGHT", `docker version --format '{{.Server.Version}}'`, nil, 60)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return stepErr("ERR_PREFLIGHT", t.Host(), "PREFLIGHT",
			fmt.Errorf("docker daemon unavailable: %s", truncOut(r)))
	}
	return nil
}

// imageRef resolves tag-vs-digest (DESIGN §6.3 docker_registry).
func imageRef(a *spec.Artifact) string {
	src := a.Source
	if src.Digest != "" {
		return src.Image + "@" + src.Digest
	}
	tag := src.Tag
	if tag == "" {
		tag = a.Version
	}
	return src.Image + ":" + tag
}

// Pull = D1+D2 (+D7 logout). Login credentials travel via env/stdin only. Every
// interpolated value (registry, username, image ref) is single-quoted for the
// target shell via `q` so registry/image/user metacharacters cannot be
// interpreted (DESIGN §9.6 D1/D2).
func (d *DockerContainer) Pull(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	a := &rc.Spec.Artifact
	ref := imageRef(a)
	registry := ""
	if i := strings.Index(a.Source.Image, "/"); i > 0 && strings.ContainsAny(a.Source.Image[:i], ".:") {
		registry = a.Source.Image[:i]
	}
	env := map[string]string{}
	login, logout := "", ""
	if a.Source.Auth.Username != "" && a.Source.Auth.PasswordEnv != "" {
		env["LD_REG_PW"] = os.Getenv(a.Source.Auth.PasswordEnv)
		regArg := ""
		if registry != "" {
			regArg = " " + d.q(t, registry)
		}
		user := d.q(t, a.Source.Auth.Username)
		if t.OS() == spec.OSWindows {
			login = fmt.Sprintf(`$env:LD_REG_PW | docker login%s -u %s --password-stdin
if($LASTEXITCODE -ne 0){ Write-Error 'docker login failed'; exit 40 }`, regArg, user)
			logout = fmt.Sprintf("docker logout%s | Out-Null", regArg)
		} else {
			login = fmt.Sprintf(`printf '%%s' "$LD_REG_PW" | docker login%s -u %s --password-stdin || { echo 'docker login failed' >&2; exit 40; }`, regArg, user)
			logout = fmt.Sprintf("docker logout%s >/dev/null 2>&1 || true", regArg)
		}
	}
	qref := d.q(t, ref)
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`%s
docker pull %s
$code=$LASTEXITCODE
%s
if($code -ne 0){ exit 40 }
exit 0`, login, qref, logout)
	} else {
		script = fmt.Sprintf(`%s
docker pull %s; code=$?
%s
[ $code -eq 0 ] || exit 40
exit 0`, login, qref, logout)
	}
	r, err := d.run(ctx, t, "FETCH", script, env, 1800)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return stepErr("ERR_ARTIFACT_FETCH", t.Host(), "FETCH", fmt.Errorf("%s", truncOut(r)))
	}
	return nil
}

// CurrentImageID = D3, "" when container absent.
func (d *DockerContainer) CurrentImageID(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	qn := d.q(t, rc.Spec.Pattern.ContainerName)
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`docker inspect -f '{{.Image}}' %s 2>$null
if($LASTEXITCODE -ne 0){ Write-Output '' }
exit 0`, qn)
	} else {
		script = fmt.Sprintf(`docker inspect -f '{{.Image}}' %s 2>/dev/null || echo ''`, qn)
	}
	r, err := d.run(ctx, t, "STAGE", script, nil, 60)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

func (d *DockerContainer) runArgs(t transport.Transport, rc ReleaseCtx) string {
	p := rc.Spec.Pattern
	restart := p.RestartPolicy
	if restart == "" {
		restart = "unless-stopped"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-d --name %s --restart %s", d.q(t, p.ContainerName), d.q(t, restart))
	for _, pt := range p.Ports {
		fmt.Fprintf(&b, " -p %s", d.q(t, pt))
	}
	keys := make([]string, 0, len(rc.Env))
	for k := range rc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " -e %s", d.q(t, k+"="+rc.Env[k]))
	}
	for _, v := range p.Volumes {
		fmt.Fprintf(&b, " -v %s", d.q(t, v))
	}
	// run_args are operator-authored raw docker flags (e.g. --pull=never); passed
	// through verbatim, NOT container-controlled data, so they are not re-quoted.
	for _, a := range p.RunArgs {
		fmt.Fprintf(&b, " %s", a)
	}
	return b.String()
}

// q single-quotes one docker run argument for the target shell so container-
// controlled data (env values, ports, volumes, image ref) cannot be interpreted
// by sh or PowerShell — closing the `$(...)`/backtick/`$VAR`/quote command-
// injection hole a bare double-quoted `-e "K=V"` left open (DESIGN §9.6 D5). sh
// escapes an embedded `'` as `'\''`; PowerShell doubles it as `''` (both literal).
func (d *DockerContainer) q(t transport.Transport, s string) string {
	if t.OS() == spec.OSWindows {
		return psq(s)
	}
	return shq(s)
}

// RunNew = D4+D5 with a given image ref (new ref, or old image id on rollback).
func (d *DockerContainer) RunNew(ctx context.Context, t transport.Transport, rc ReleaseCtx, ref string) error {
	name := rc.Spec.Pattern.ContainerName
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`docker rm -f %s 2>$null | Out-Null
docker run %s %s
if($LASTEXITCODE -ne 0){ exit %d }
exit 0`, d.q(t, name), d.runArgs(t, rc), d.q(t, ref), ExitServiceStart)
	} else {
		script = fmt.Sprintf(`docker rm -f %s >/dev/null 2>&1 || true
docker run %s %s || exit %d
exit 0`, d.q(t, name), d.runArgs(t, rc), d.q(t, ref), ExitServiceStart)
	}
	r, err := d.run(ctx, t, "START", script, nil, 300)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "START", r)
	}
	return nil
}

// --- Pattern interface plumbing (engine drives Pull/RunNew via type assert) --

func (d *DockerContainer) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil // container definition applied in RunNew
}

func (d *DockerContainer) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	qn := d.q(t, rc.Spec.Pattern.ContainerName)
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf("docker stop %s 2>$null | Out-Null\nexit 0", qn)
	} else {
		script = fmt.Sprintf("docker stop %s >/dev/null 2>&1 || true", qn)
	}
	_, err := d.run(ctx, t, "STOP", script, nil, 120)
	return err
}

func (d *DockerContainer) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return d.RunNew(ctx, t, rc, imageRef(&rc.Spec.Artifact))
}

func (d *DockerContainer) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	qn := d.q(t, rc.Spec.Pattern.ContainerName)
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`$s = docker inspect -f '{{.State.Running}}' %s 2>$null
if($LASTEXITCODE -ne 0){ Write-Output 'not_installed'; exit 0 }
if($s -match 'true'){ Write-Output 'running' } else { Write-Output 'stopped' }
exit 0`, qn)
	} else {
		script = fmt.Sprintf(`s=$(docker inspect -f '{{.State.Running}}' %s 2>/dev/null) || { echo not_installed; exit 0; }
[ "$s" = "true" ] && echo running || echo stopped`, qn)
	}
	r, err := d.run(ctx, t, "STATUS", script, nil, 60)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

func (d *DockerContainer) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	qn := d.q(t, rc.Spec.Pattern.ContainerName)
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf("docker rm -f %s 2>$null | Out-Null\nexit 0", qn)
	} else {
		script = fmt.Sprintf("docker rm -f %s >/dev/null 2>&1 || true", qn)
	}
	_, err := d.run(ctx, t, "CONFIGURE", script, nil, 120)
	return err
}

// ConfigFingerprint hashes the MUTABLE docker_container settings — image ref
// (tag or digest), container name, restart policy, ports, volumes, run_args and
// the rendered env — so the engine can tell a same-version CONFIGURATION change
// (new tag/digest, ports, env, volumes, restart, run args) apart from a true
// no-op and avoid silently ignoring it (DESIGN §10.1). Ports/volumes/env are
// order-normalized; run_args keep author order (docker flag order is meaningful).
func (d *DockerContainer) ConfigFingerprint(rc ReleaseCtx) string {
	p := rc.Spec.Pattern
	restart := p.RestartPolicy
	if restart == "" {
		restart = "unless-stopped"
	}
	h := sha256.New()
	fmt.Fprintf(h, "ref=%s\n", imageRef(&rc.Spec.Artifact))
	fmt.Fprintf(h, "name=%s\n", p.ContainerName)
	fmt.Fprintf(h, "restart=%s\n", restart)
	ports := append([]string(nil), p.Ports...)
	sort.Strings(ports)
	for _, x := range ports {
		fmt.Fprintf(h, "port=%s\n", x)
	}
	vols := append([]string(nil), p.Volumes...)
	sort.Strings(vols)
	for _, x := range vols {
		fmt.Fprintf(h, "vol=%s\n", x)
	}
	for _, x := range p.RunArgs {
		fmt.Fprintf(h, "arg=%s\n", x)
	}
	keys := make([]string, 0, len(rc.Env))
	for k := range rc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "env=%s=%s\n", k, rc.Env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
