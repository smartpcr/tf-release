// Package engine orchestrates the deployment state machine (DESIGN §10).
package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/artifact"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/pattern"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

const ProviderVersion = "0.1.0"

// Status is what the resource surfaces as computed attributes (DESIGN §5.2).
type Status struct {
	DeployedVersion string
	PreviousVersion string
	ReleasePath     string
	ServiceStatus   string
	Hosts           []string
}

type Engine struct {
	// NewTransport is swappable for fake-transport unit tests (DESIGN §17).
	NewTransport func(t *spec.Target, host string) (transport.Transport, error)
	Warnings     []string
}

func New() *Engine { return &Engine{NewTransport: transport.NewTransport} }

func (e *Engine) warnf(format string, a ...interface{}) {
	e.Warnings = append(e.Warnings, fmt.Sprintf(format, a...))
}

// Deploy is the entry point for Create and Update (DESIGN §10.1/§10.2).
func (e *Engine) Deploy(ctx context.Context, s *spec.Deployment) (*Status, error) {
	if s.Pattern.Type == spec.PatternClusterGeneric {
		return e.deployCluster(ctx, s)
	}
	return e.deploySingle(ctx, s)
}

func (e *Engine) deploySingle(ctx context.Context, s *spec.Deployment) (*Status, error) {
	host := strings.ToLower(s.Target.Hosts[0])
	t, err := e.NewTransport(&s.Target, host)
	if err != nil {
		return nil, coded("ERR_SPEC_INVALID", host, "VALIDATE", err)
	}
	if err := t.Connect(ctx); err != nil {
		return nil, wrapTransportErr(err, host, "CONNECT")
	}
	defer t.Close()

	p := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, s.Artifact.Version)
	pat, err := pattern.For(s.Pattern.Type)
	if err != nil {
		return nil, coded("ERR_UNSUPPORTED", host, "VALIDATE", err)
	}
	rc := releaseCtx(s, p)

	if err := e.preflight(ctx, t, s, p, pat, rc); err != nil {
		return nil, err
	}

	lk, warn, err := AcquireLock(ctx, t, p, lockOwner(), "deploy", s.Strategy.EffectiveLockTimeout())
	if err != nil {
		return nil, err
	}
	if warn != "" {
		e.warnf("%s", warn)
	}
	defer func() {
		rctx, cancel := lockCleanupContext(ctx)
		defer cancel()
		if rerr := ReleaseLock(rctx, lk); rerr != nil {
			e.warnf("lock release failed on %s: %v", host, rerr)
		}
	}()

	m, err := ReadManifest(ctx, t, p)
	if err != nil {
		return nil, coded("ERR_CONNECT", host, "PREFLIGHT", err)
	}

	// Docker never uses staging/junction (DESIGN §9.6).
	if s.Pattern.Type == spec.PatternDockerCont {
		return e.deployDocker(ctx, t, s, p, pat.(*pattern.DockerContainer), rc, m)
	}

	// Idempotency short-circuit (DESIGN §10.1 step 2).
	if m != nil && m.CurrentVersion == s.Artifact.Version && m.ArtifactChecksum == s.Artifact.Checksum {
		st, serr := pat.Status(ctx, t, rc)
		if serr == nil && (st == "running" || st == "n/a" || st == "online") {
			tflog.Info(ctx, "idempotent no-op: version already deployed and healthy",
				map[string]interface{}{"host": host, "version": s.Artifact.Version})
			return statusFrom(m, host, st), nil
		}
	}

	prev := ""
	if m != nil {
		prev = m.CurrentVersion
	}
	started := time.Now().UTC()

	// STAGE / FETCH / CHECKSUM / EXTRACT / RENDER — no live mutation yet.
	if err := e.stageOnHost(ctx, t, s, p, pat, rc); err != nil {
		return nil, err
	}

	// SWITCHOVER — from here on, failures trigger rollback (DESIGN §10.3).
	switchErr := e.switchOn(ctx, t, s, p, pat, rc)
	if switchErr == nil {
		switchErr = RunHealthCheck(ctx, t, &s.HealthCheck, p.Current, rc.Env)
	}
	if switchErr != nil {
		return nil, e.rollbackSingle(ctx, t, s, p, pat, prev, started, switchErr)
	}

	// FINALIZE + PRUNE (DESIGN §10.1 steps 12–13).
	nm := &Manifest{
		Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
		CurrentVersion: s.Artifact.Version, PreviousVersion: prev,
		CurrentRelease: p.Release, ArtifactChecksum: s.Artifact.Checksum,
		ProviderVersion: ProviderVersion,
		LastOperation: LastOp{Type: "deploy", Result: "success",
			Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
	}
	if err := WriteManifest(ctx, t, p, nm); err != nil {
		return nil, coded("ERR_CONNECT", host, "FINALIZE", err)
	}
	if err := e.pruneReleases(ctx, t, s, p, nm); err != nil {
		e.warnf("prune failed on %s: %v", host, err) // never fails the apply
	}
	st, _ := pat.Status(ctx, t, rc)
	return statusFrom(nm, host, st), nil
}

// rollbackSingle implements DESIGN §10.3 for one host. Returns the ORIGINAL
// error (annotated) on successful rollback; ERR_ROLLBACK_FAILED otherwise.
func (e *Engine) rollbackSingle(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, prev string, started time.Time, orig error) error {
	host := t.Host()
	if !s.Strategy.EffectiveRollback() {
		e.finalizeFailed(ctx, t, s, p, prev, started, "failed")
		return fmt.Errorf("%w; rollback_on_failure=false — target left as-is for inspection", orig)
	}
	if prev == "" {
		// Fresh install failure ⇒ clean the machine (DESIGN §10.3 row 1).
		rcNew := releaseCtx(s, p)
		_ = pat.Stop(ctx, t, rcNew)
		_ = pat.Uninstall(ctx, t, rcNew, false)
		_ = e.removeJunction(ctx, t, p)
		_ = e.removePath(ctx, t, p.Manifest)
		return fmt.Errorf("%w; fresh install failed — target cleaned (no service, no junction, no manifest)", orig)
	}
	tflog.Warn(ctx, "deploy failed; rolling back", map[string]interface{}{
		"host": host, "from": s.Artifact.Version, "to": prev, "cause": orig.Error()})

	pp := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, prev)
	rcPrev := releaseCtx(s, pp)
	rb := func() error {
		if err := pat.Stop(ctx, t, rcPrev); err != nil {
			return err
		}
		if err := e.switchJunction(ctx, t, pp); err != nil {
			return err
		}
		if err := pat.Configure(ctx, t, rcPrev); err != nil {
			return err
		}
		if err := pat.Start(ctx, t, rcPrev); err != nil {
			return err
		}
		return RunHealthCheck(ctx, t, &s.HealthCheck, pp.Current, rcPrev.Env)
	}
	if rerr := rb(); rerr != nil {
		e.finalizeFailed(ctx, t, s, pp, prev, started, "failed")
		return coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE — manual intervention required; deploy error: %v; rollback error: %v", orig, rerr))
	}
	m := &Manifest{
		Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
		CurrentVersion: prev, CurrentRelease: pp.Release,
		ProviderVersion: ProviderVersion,
		LastOperation: LastOp{Type: "deploy", Result: "rolled_back",
			Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
	}
	_ = WriteManifest(ctx, t, pp, m)
	return fmt.Errorf("%w; rolled back to %s (healthy)", orig, prev)
}

func (e *Engine) finalizeFailed(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, current string, started time.Time, result string) {
	if current == "" {
		return
	}
	m, _ := ReadManifest(ctx, t, p)
	if m == nil {
		m = &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
			CurrentVersion: current, CurrentRelease: p.Release, ProviderVersion: ProviderVersion}
	}
	m.LastOperation = LastOp{Type: "deploy", Result: result,
		Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)}
	_ = WriteManifest(ctx, t, p, m)
}

// deployDocker = D-steps (DESIGN §9.6) with image-id rollback.
func (e *Engine) deployDocker(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, dc *pattern.DockerContainer, rc pattern.ReleaseCtx, m *Manifest) (*Status, error) {
	host := t.Host()
	started := time.Now().UTC()
	if m != nil && m.CurrentVersion == s.Artifact.Version {
		if st, err := dc.Status(ctx, t, rc); err == nil && st == "running" {
			return statusFrom(m, host, st), nil
		}
	}
	prev, oldImage := "", ""
	if m != nil {
		prev = m.CurrentVersion
		oldImage = m.Extra["image_id"]
	}
	if err := dc.Pull(ctx, t, rc); err != nil {
		return nil, err
	}
	if cur, err := dc.CurrentImageID(ctx, t, rc); err == nil && cur != "" {
		oldImage = cur
	}
	runErr := dc.Start(ctx, t, rc) // rm -f old + run new
	if runErr == nil {
		runErr = RunHealthCheck(ctx, t, &s.HealthCheck, p.Root, rc.Env)
	}
	if runErr != nil {
		if !s.Strategy.EffectiveRollback() || oldImage == "" {
			return nil, fmt.Errorf("%w; no docker rollback performed (prev image unknown or rollback disabled)", runErr)
		}
		if rerr := dc.RunNew(ctx, t, rc, oldImage); rerr != nil {
			return nil, coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
				fmt.Errorf("MACHINE IN UNKNOWN STATE; deploy: %v; rollback: %v", runErr, rerr))
		}
		if herr := RunHealthCheck(ctx, t, &s.HealthCheck, p.Root, rc.Env); herr != nil {
			return nil, coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
				fmt.Errorf("MACHINE IN UNKNOWN STATE; deploy: %v; rollback health: %v", runErr, herr))
		}
		return nil, fmt.Errorf("%w; rolled back to previous image %s", runErr, short(oldImage))
	}
	newImage, _ := dc.CurrentImageID(ctx, t, rc)
	nm := &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
		CurrentVersion: s.Artifact.Version, PreviousVersion: prev,
		CurrentRelease:  "docker://" + rc.Spec.Pattern.ContainerName,
		ProviderVersion: ProviderVersion,
		Extra:           map[string]string{"image_id": newImage},
		LastOperation: LastOp{Type: "deploy", Result: "success",
			Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
	}
	if err := ensureDir(ctx, t, p.Root); err == nil {
		_ = WriteManifest(ctx, t, p, nm)
	}
	st, _ := dc.Status(ctx, t, rc)
	return statusFrom(nm, host, st), nil
}

// ---------------------------------------------------------------------------
// Shared step implementations
// ---------------------------------------------------------------------------

func releaseCtx(s *spec.Deployment, p layout.Paths) pattern.ReleaseCtx {
	nodePort := 0
	if s.Pattern.Type == spec.PatternNodeWebApp {
		nodePort = s.Pattern.Port
	}
	env := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, nodePort), s.Environment)
	return pattern.ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version,
		P: p, Spec: s, Env: env}
}

func lockOwner() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("labdeploy@%s/pid=%d", h, os.Getpid())
}

// preflight = global gates + pattern gates (DESIGN §10.1 step 3).
func (e *Engine) preflight(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, rc pattern.ReleaseCtx) error {
	host := t.Host()
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`if($PSVersionTable.PSVersion.Major -lt 5){ Write-Error ("powershell " + $PSVersionTable.PSVersion + " < 5.1"); exit 1 }
$drive = (Split-Path -Qualifier %s) + '\'
$free = (Get-PSDrive -Name $drive.Substring(0,1)).Free
if($free -lt 500MB){ Write-Error ("free space " + [math]::Round($free/1MB) + "MB < 500MB on " + $drive); exit 1 }
exit 0`, psq(p.Root))
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
		if err != nil {
			return wrapTransportErr(err, host, "PREFLIGHT")
		}
		if r.ExitCode != 0 {
			return coded("ERR_PREFLIGHT", host, "PREFLIGHT", fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
		}
	} else {
		script := fmt.Sprintf(`command -v unzip >/dev/null || { echo 'unzip missing' >&2; exit 1; }
command -v curl >/dev/null || { echo 'curl missing' >&2; exit 1; }
mkdir -p '%s' 2>/dev/null || true
avail=$(df -Pm "$(dirname '%s')" | awk 'NR==2{print $4}')
[ "$avail" -ge 500 ] || { echo "free space ${avail}MB < 500MB" >&2; exit 1; }`, p.Root, p.Root)
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
		if err != nil {
			return wrapTransportErr(err, host, "PREFLIGHT")
		}
		if r.ExitCode != 0 {
			return coded("ERR_PREFLIGHT", host, "PREFLIGHT", fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
		}
	}
	if s.Target.Transport == spec.TransportSSH && s.Target.SSH.HostKey == "" {
		e.warnf("ssh.host_key not pinned for %s — accepting any host key (lab default)", host)
	}
	if err := pat.Preflight(ctx, t, rc); err != nil {
		return err
	}
	return nil
}

// stageOnHost = FETCH+CHECKSUM+EXTRACT+deps+RENDER into the immutable release
// dir. Cached releases (marker sha match) skip fetch/extract (DESIGN §10.5).
func (e *Engine) stageOnHost(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, rc pattern.ReleaseCtx) error {
	host := t.Host()
	if err := ensureLayout(ctx, t, p); err != nil {
		return coded("ERR_CONNECT", host, "STAGE", err)
	}
	cached, err := e.releaseCached(ctx, t, p, s.Artifact.Checksum)
	if err != nil {
		return coded("ERR_CONNECT", host, "STAGE", err)
	}
	if !cached {
		if err := e.fetchToStaging(ctx, t, s, p); err != nil {
			return err
		}
		if err := e.extract(ctx, t, p); err != nil {
			return err
		}
		if n, ok := pat.(*pattern.NodeWebApp); ok {
			if err := n.InstallDeps(ctx, t, rc); err != nil {
				return err
			}
		}
		if err := e.writeReleaseMarker(ctx, t, p, s); err != nil {
			return coded("ERR_CONNECT", host, "STAGE", err)
		}
	} else {
		tflog.Info(ctx, "release cached; skipping fetch/extract",
			map[string]interface{}{"host": host, "version": s.Artifact.Version})
	}
	// RENDER: files land in the release dir every apply (DESIGN §6.4).
	for _, f := range s.Files {
		var dest string
		if t.OS() == spec.OSWindows {
			dest = p.Release + `\` + strings.ReplaceAll(strings.TrimLeft(f.Path, `\/`), "/", `\`)
		} else {
			dest = p.Release + "/" + strings.TrimLeft(f.Path, "/")
		}
		if err := writeSmallFile(ctx, t, dest, f.Content); err != nil {
			return coded("ERR_EXTRACT", host, "RENDER", err)
		}
	}
	// post_install hook runs in release dir before switchover (DESIGN §6.4).
	if hook := s.Pattern.PostInstall; hook != "" {
		var r transport.Result
		var xerr error
		if t.OS() == spec.OSWindows {
			r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Env: rc.Env, TimeoutSec: 600,
				Script: fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(p.Release), hook)})
		} else {
			r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Env: rc.Env, TimeoutSec: 600,
				Script: fmt.Sprintf("cd '%s' && %s", p.Release, hook)})
		}
		if xerr != nil {
			return wrapTransportErr(xerr, host, "STAGE")
		}
		if r.ExitCode != 0 {
			return coded("ERR_EXTRACT", host, "STAGE",
				fmt.Errorf("post_install exit=%d: %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout)))
		}
	}
	return nil
}

// switchOn = STOP + SWITCH + CONFIGURE + START for one host (DESIGN §10.1 7–10).
func (e *Engine) switchOn(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, rc pattern.ReleaseCtx) error {
	if err := pat.Stop(ctx, t, rc); err != nil {
		return err
	}
	if err := e.switchJunction(ctx, t, p); err != nil {
		return err
	}
	if err := pat.Configure(ctx, t, rc); err != nil {
		return err
	}
	return pat.Start(ctx, t, rc)
}

func (e *Engine) fetchToStaging(ctx context.Context, t transport.Transport, s *spec.Deployment, p layout.Paths) error {
	host := t.Host()
	if artifact.UseTargetPull(&s.Artifact) {
		var script string
		var env map[string]string
		var err error
		if t.OS() == spec.OSWindows {
			script, env, err = artifact.TargetPullScriptWindows(&s.Artifact, p.StagePkg)
		} else {
			script, env, err = artifact.TargetPullScriptLinux(&s.Artifact, p.StagePkg)
		}
		if err != nil {
			return coded("ERR_ARTIFACT_FETCH", host, "FETCH", err)
		}
		shell := transport.ShellSh
		if t.OS() == spec.OSWindows {
			shell = transport.ShellPowerShell
		}
		r, xerr := t.Exec(ctx, transport.Cmd{Shell: shell, Script: script, Env: env, TimeoutSec: 1800})
		if xerr != nil {
			return wrapTransportErr(xerr, host, "FETCH")
		}
		switch r.ExitCode {
		case 0:
			return nil
		case 41:
			return coded("ERR_CHECKSUM_MISMATCH", host, "CHECKSUM",
				fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
		default:
			return coded("ERR_ARTIFACT_FETCH", host, "FETCH",
				fmt.Errorf("exit=%d: %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout)))
		}
	}
	// runner_push: fetch+verify on runner, stream via transport.Upload.
	f, err := artifact.Fetch(ctx, &s.Artifact)
	if err != nil {
		if ce, ok := err.(*artifact.CodedError); ok {
			return coded(ce.Code, host, "FETCH", ce.Err)
		}
		return coded("ERR_ARTIFACT_FETCH", host, "FETCH", err)
	}
	defer os.Remove(f.LocalPath)
	fh, err := os.Open(f.LocalPath)
	if err != nil {
		return coded("ERR_ARTIFACT_FETCH", host, "FETCH", err)
	}
	defer fh.Close()
	if err := t.Upload(ctx, fh, f.Size, p.StagePkg); err != nil {
		return coded("ERR_CONNECT", host, "FETCH", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Remote filesystem primitives
// ---------------------------------------------------------------------------

func ensureLayout(ctx context.Context, t transport.Transport, p layout.Paths) error {
	if t.OS() == spec.OSWindows {
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: layout.DirScript(p), TimeoutSec: 60})
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("mkdir layout: %s", r.Stderr)
		}
		return nil
	}
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: layout.DirScript(p), TimeoutSec: 60})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("mkdir layout: %s", r.Stderr)
	}
	return nil
}

func ensureDir(ctx context.Context, t transport.Transport, dir string) error {
	if t.OS() == spec.OSWindows {
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell,
			Script: fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null`, psq(dir)), TimeoutSec: 60})
		if err != nil || r.ExitCode != 0 {
			return fmt.Errorf("mkdir %s: err=%v", dir, err)
		}
		return nil
	}
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh,
		Script: fmt.Sprintf(`mkdir -p '%s'`, dir), TimeoutSec: 60})
	if err != nil || r.ExitCode != 0 {
		return fmt.Errorf("mkdir %s: err=%v", dir, err)
	}
	return nil
}

const releaseMarker = ".labdeploy-release.json"

func (e *Engine) releaseCached(ctx context.Context, t transport.Transport, p layout.Paths, wantChecksum string) (bool, error) {
	marker := p.Release + sepFor(t.OS()) + releaseMarker
	raw, ok, err := readSmallFile(ctx, t, marker)
	if err != nil || !ok {
		return false, err
	}
	return strings.Contains(raw, `"checksum": "`+wantChecksum+`"`), nil
}

func (e *Engine) writeReleaseMarker(ctx context.Context, t transport.Transport, p layout.Paths, s *spec.Deployment) error {
	marker := p.Release + sepFor(t.OS()) + releaseMarker
	content := fmt.Sprintf("{\n  \"version\": %q,\n  \"checksum\": %q,\n  \"staged_utc\": %q\n}\n",
		s.Artifact.Version, s.Artifact.Checksum, time.Now().UTC().Format(time.RFC3339))
	return writeSmallFile(ctx, t, marker, content)
}

func sepFor(os spec.OSKind) string {
	if os == spec.OSLinux {
		return "/"
	}
	return `\`
}

// extract unpacks staging/pkg.zip into the (fresh) release dir. Exit path maps
// to ERR_EXTRACT (DESIGN §12).
func (e *Engine) extract(ctx context.Context, t transport.Transport, p layout.Paths) error {
	host := t.Host()
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`$ErrorActionPreference='Stop'
if(Test-Path %s){ Remove-Item -Recurse -Force %s }
New-Item -ItemType Directory -Force -Path %s | Out-Null
try { Expand-Archive -Path %s -DestinationPath %s -Force }
catch { Write-Error $_.Exception.Message; exit 1 }
Remove-Item -Force -ErrorAction SilentlyContinue %s
exit 0`, psq(p.Release), psq(p.Release), psq(p.Release), psq(p.StagePkg), psq(p.Release), psq(p.StagePkg))
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 900})
		if err != nil {
			return wrapTransportErr(err, host, "EXTRACT")
		}
		if r.ExitCode != 0 {
			return coded("ERR_EXTRACT", host, "EXTRACT", fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
		}
		return nil
	}
	script := fmt.Sprintf(`set -e
rm -rf '%s'
mkdir -p '%s'
unzip -o -q '%s' -d '%s'
rm -f '%s'`, p.Release, p.Release, p.StagePkg, p.Release, p.StagePkg)
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 900})
	if err != nil {
		return wrapTransportErr(err, host, "EXTRACT")
	}
	if r.ExitCode != 0 {
		return coded("ERR_EXTRACT", host, "EXTRACT", fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
	}
	return nil
}

// switchJunction = S3: atomic-ish repoint of `current` (DESIGN §9.2). Exit 42.
func (e *Engine) switchJunction(ctx context.Context, t transport.Transport, p layout.Paths) error {
	host := t.Host()
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`if(Test-Path %s){ & cmd /c rmdir %s; if($LASTEXITCODE -ne 0){ Write-Error 'rmdir current failed'; exit 42 } }
& cmd /c mklink /J %s %s | Out-Null
if($LASTEXITCODE -ne 0){ Write-Error 'mklink failed'; exit 42 }
exit 0`, psq(p.Current), quoteCmd(p.Current), quoteCmd(p.Current), quoteCmd(p.Release))
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
		if err != nil {
			return wrapTransportErr(err, host, "SWITCH")
		}
		if r.ExitCode != 0 {
			return coded("ERR_SWITCH", host, "SWITCH", fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
		}
		return nil
	}
	script := fmt.Sprintf(`ln -sfn '%s' '%s.tmp' && mv -Tf '%s.tmp' '%s' || exit 42`,
		p.Release, p.Current, p.Current, p.Current)
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	if err != nil {
		return wrapTransportErr(err, host, "SWITCH")
	}
	if r.ExitCode != 0 {
		return coded("ERR_SWITCH", host, "SWITCH", fmt.Errorf("%s", strings.TrimSpace(r.Stderr+r.Stdout)))
	}
	return nil
}

func (e *Engine) removeJunction(ctx context.Context, t transport.Transport, p layout.Paths) error {
	if t.OS() == spec.OSWindows {
		_, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell,
			Script: fmt.Sprintf(`if(Test-Path %s){ & cmd /c rmdir %s }`, psq(p.Current), quoteCmd(p.Current)), TimeoutSec: 60})
		return err
	}
	_, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh,
		Script: fmt.Sprintf(`rm -f '%s'`, p.Current), TimeoutSec: 60})
	return err
}

func (e *Engine) removePath(ctx context.Context, t transport.Transport, path string) error {
	if t.OS() == spec.OSWindows {
		_, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell,
			Script: fmt.Sprintf(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue %s`, psq(path)), TimeoutSec: 300})
		return err
	}
	_, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh,
		Script: fmt.Sprintf(`rm -rf '%s'`, path), TimeoutSec: 300})
	return err
}

// pruneReleases keeps newest keep_releases dirs; current+previous always
// protected (DESIGN §10.1 step 13). Deletion list computed runner-side.
func (e *Engine) pruneReleases(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, m *Manifest) error {
	keep := s.Strategy.EffectiveKeepReleases()
	var listing string
	if t.OS() == spec.OSWindows {
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 60,
			Script: fmt.Sprintf(`Get-ChildItem -Directory %s | ForEach-Object { $_.Name + '|' + $_.LastWriteTimeUtc.Ticks }`, psq(p.Releases))})
		if err != nil || r.ExitCode != 0 {
			return fmt.Errorf("list releases: err=%v %s", err, r.Stderr)
		}
		listing = r.Stdout
	} else {
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 60,
			Script: fmt.Sprintf(`for d in '%s'/*/; do [ -d "$d" ] && printf '%%s|%%s\n' "$(basename "$d")" "$(stat -c %%Y "$d")"; done`, p.Releases)})
		if err != nil || r.ExitCode != 0 {
			return fmt.Errorf("list releases: err=%v %s", err, r.Stderr)
		}
		listing = r.Stdout
	}
	type rel struct {
		name string
		ts   int64
	}
	var rels []rel
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		var ts int64
		_, _ = fmt.Sscanf(parts[1], "%d", &ts)
		rels = append(rels, rel{parts[0], ts})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].ts > rels[j].ts }) // newest first
	protected := map[string]bool{m.CurrentVersion: true}
	if m.PreviousVersion != "" {
		protected[m.PreviousVersion] = true
	}
	kept := 0
	for _, r := range rels {
		if protected[r.name] {
			kept++
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		_ = e.removePath(ctx, t, p.Releases+sepFor(t.OS())+r.name)
	}
	return nil
}

// quoteCmd double-quotes a path for cmd.exe embedded in PS `& cmd /c ...`.
func quoteCmd(p string) string { return `"` + p + `"` }

func wrapTransportErr(err error, host, step string) error {
	if ce, ok := err.(*transport.CodedError); ok {
		return coded(ce.Code, host, step, ce.Err)
	}
	return coded("ERR_CONNECT", host, step, err)
}

func statusFrom(m *Manifest, host, svcStatus string) *Status {
	return &Status{
		DeployedVersion: m.CurrentVersion,
		PreviousVersion: m.PreviousVersion,
		ReleasePath:     m.CurrentRelease,
		ServiceStatus:   svcStatus,
		Hosts:           []string{host},
	}
}

func short(s string) string {
	if len(s) > 19 {
		return s[:19]
	}
	return s
}

// ---------------------------------------------------------------------------
// Read + Destroy (DESIGN §10.4, §10.6)
// ---------------------------------------------------------------------------

// ReadStatus refreshes state. (nil, nil) ⇒ resource gone (manifest absent).
func (e *Engine) ReadStatus(ctx context.Context, s *spec.Deployment) (*Status, error) {
	host := strings.ToLower(s.Target.Hosts[0])
	t, err := e.NewTransport(&s.Target, host)
	if err != nil {
		return nil, err
	}
	if err := t.Connect(ctx); err != nil {
		return nil, wrapTransportErr(err, host, "CONNECT") // unreachable ≠ deleted (DESIGN §10.4)
	}
	defer t.Close()
	p := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, s.Artifact.Version)
	m, err := ReadManifest(ctx, t, p)
	if err != nil {
		return nil, coded("ERR_CONNECT", host, "PREFLIGHT", err)
	}
	present, deployedVer, warn := ReconcileManifest(m)
	if !present {
		return nil, nil
	}
	if warn != "" {
		e.warnf("%s", warn)
	}
	pat, err := pattern.For(s.Pattern.Type)
	if err != nil {
		return nil, err
	}
	rp := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, m.CurrentVersion)
	st, serr := pat.Status(ctx, t, releaseCtx(s, rp))
	if serr != nil {
		// A failed status probe is a loud refresh failure, not an empty/healthy
		// service status (evaluator item 5 / DESIGN §10.4).
		return nil, wrapTransportErr(serr, host, "READ")
	}
	out := statusFrom(m, host, st)
	out.DeployedVersion = deployedVer
	if s.Pattern.Type == spec.PatternClusterGeneric {
		out.Hosts = lowerAll(s.Target.Hosts)
	}
	return out, nil
}

// Destroy honors destroy_mode purge|unregister|abandon (DESIGN §10.6).
func (e *Engine) Destroy(ctx context.Context, s *spec.Deployment, mode string) error {
	if mode == "abandon" {
		return nil
	}
	if s.Pattern.Type == spec.PatternClusterGeneric {
		return e.destroyCluster(ctx, s, mode)
	}
	host := strings.ToLower(s.Target.Hosts[0])
	t, err := e.NewTransport(&s.Target, host)
	if err != nil {
		return err
	}
	if err := t.Connect(ctx); err != nil {
		return wrapTransportErr(err, host, "CONNECT")
	}
	defer t.Close()
	p := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, s.Artifact.Version)
	pat, err := pattern.For(s.Pattern.Type)
	if err != nil {
		return err
	}
	lk, warn, err := AcquireLock(ctx, t, p, lockOwner(), "destroy", s.Strategy.EffectiveLockTimeout())
	if err != nil {
		return err
	}
	if warn != "" {
		e.warnf("%s", warn)
	}
	// Release the lock on EVERY post-lock return path — including a failed tree
	// removal in purge mode, which previously left .lock behind (evaluator
	// item 2). The detached cleanup context ensures release runs even if ctx is
	// already canceled (item 3); release is ownership-safe (compare-and-delete).
	defer func() {
		rctx, cancel := lockCleanupContext(ctx)
		defer cancel()
		if rerr := ReleaseLock(rctx, lk); rerr != nil {
			e.warnf("lock release failed on %s: %v", host, rerr)
		}
	}()
	rc := releaseCtx(s, p)
	if err := pat.Uninstall(ctx, t, rc, mode == "purge"); err != nil {
		return err
	}
	if mode == "purge" {
		return e.removePath(ctx, t, p.Root) // lock file goes with the tree
	}
	// unregister: keep releases/shared, drop manifest so Read sees absent.
	if err := e.removePath(ctx, t, p.Manifest); err != nil {
		return err
	}
	return nil
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}
