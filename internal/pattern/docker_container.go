package pattern

import (
	"context"
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

// Pull = D1+D2 (+D7 logout). Login credentials travel via env/stdin only.
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
		if t.OS() == spec.OSWindows {
			login = fmt.Sprintf(`$env:LD_REG_PW | docker login %s -u %s --password-stdin
if($LASTEXITCODE -ne 0){ Write-Error 'docker login failed'; exit 40 }`, registry, psq(a.Source.Auth.Username))
			logout = fmt.Sprintf("docker logout %s | Out-Null", registry)
		} else {
			login = fmt.Sprintf(`printf '%%s' "$LD_REG_PW" | docker login %s -u '%s' --password-stdin || { echo 'docker login failed' >&2; exit 40; }`, registry, a.Source.Auth.Username)
			logout = fmt.Sprintf("docker logout %s >/dev/null 2>&1 || true", registry)
		}
	}
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`%s
docker pull %s
$code=$LASTEXITCODE
%s
if($code -ne 0){ exit 40 }
exit 0`, login, ref, logout)
	} else {
		script = fmt.Sprintf(`%s
docker pull '%s'; code=$?
%s
[ $code -eq 0 ] || exit 40
exit 0`, login, ref, logout)
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
	name := rc.Spec.Pattern.ContainerName
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`docker inspect -f '{{.Image}}' %s 2>$null
if($LASTEXITCODE -ne 0){ Write-Output '' }
exit 0`, name)
	} else {
		script = fmt.Sprintf(`docker inspect -f '{{.Image}}' '%s' 2>/dev/null || echo ''`, name)
	}
	r, err := d.run(ctx, t, "STAGE", script, nil, 60)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

func (d *DockerContainer) runArgs(rc ReleaseCtx) string {
	p := rc.Spec.Pattern
	restart := p.RestartPolicy
	if restart == "" {
		restart = "unless-stopped"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-d --name %s --restart %s", p.ContainerName, restart)
	for _, pt := range p.Ports {
		fmt.Fprintf(&b, " -p %s", pt)
	}
	keys := make([]string, 0, len(rc.Env))
	for k := range rc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, " -e %s", shellKV(k, rc.Env[k]))
	}
	for _, v := range p.Volumes {
		fmt.Fprintf(&b, " -v %s", v)
	}
	for _, a := range p.RunArgs {
		fmt.Fprintf(&b, " %s", a)
	}
	return b.String()
}

func shellKV(k, v string) string { return "\"" + k + "=" + strings.ReplaceAll(v, `"`, `\"`) + "\"" }

// RunNew = D4+D5 with a given image ref (new ref, or old image id on rollback).
func (d *DockerContainer) RunNew(ctx context.Context, t transport.Transport, rc ReleaseCtx, ref string) error {
	name := rc.Spec.Pattern.ContainerName
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`docker rm -f %s 2>$null | Out-Null
docker run %s %s
if($LASTEXITCODE -ne 0){ exit %d }
exit 0`, name, d.runArgs(rc), ref, ExitServiceStart)
	} else {
		script = fmt.Sprintf(`docker rm -f '%s' >/dev/null 2>&1 || true
docker run %s '%s' || exit %d
exit 0`, name, d.runArgs(rc), ref, ExitServiceStart)
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
	name := rc.Spec.Pattern.ContainerName
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf("docker stop %s 2>$null | Out-Null\nexit 0", name)
	} else {
		script = fmt.Sprintf("docker stop '%s' >/dev/null 2>&1 || true", name)
	}
	_, err := d.run(ctx, t, "STOP", script, nil, 120)
	return err
}

func (d *DockerContainer) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return d.RunNew(ctx, t, rc, imageRef(&rc.Spec.Artifact))
}

func (d *DockerContainer) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	name := rc.Spec.Pattern.ContainerName
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`$s = docker inspect -f '{{.State.Running}}' %s 2>$null
if($LASTEXITCODE -ne 0){ Write-Output 'not_installed'; exit 0 }
if($s -match 'true'){ Write-Output 'running' } else { Write-Output 'stopped' }
exit 0`, name)
	} else {
		script = fmt.Sprintf(`s=$(docker inspect -f '{{.State.Running}}' '%s' 2>/dev/null) || { echo not_installed; exit 0; }
[ "$s" = "true" ] && echo running || echo stopped`, name)
	}
	r, err := d.run(ctx, t, "STATUS", script, nil, 60)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

func (d *DockerContainer) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	name := rc.Spec.Pattern.ContainerName
	var script string
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf("docker rm -f %s 2>$null | Out-Null\nexit 0", name)
	} else {
		script = fmt.Sprintf("docker rm -f '%s' >/dev/null 2>&1 || true", name)
	}
	_, err := d.run(ctx, t, "CONFIGURE", script, nil, 120)
	return err
}
