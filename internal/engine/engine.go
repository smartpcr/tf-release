// Package engine orchestrates the deployment state machine (DESIGN §10).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/artifact"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/logs"
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

// stepLogger emits the DESIGN §8.5/§15 structured step record. EVERY fixed step
// (VALIDATE CONNECT PREFLIGHT LOCK FETCH CHECKSUM STAGE EXTRACT RENDER CONFIGURE
// STOP SWITCH START HEALTH FINALIZE PRUNE UNLOCK, plus ROLLBACK/FORCE_KILL/
// MOVE_GROUP) is logged through tflog carrying app/host/step/version and a
// numeric duration_ms. A step is emitted whether it succeeded or failed — a
// failed step is still an executed step — so the log is a faithful trace of the
// state machine. Conditional steps (FETCH/CHECKSUM/EXTRACT when a release is
// cached) are simply never entered, so they never appear.
type stepLogger struct {
	app     string
	host    string
	version string
}

func newStepLogger(s *spec.Deployment, host string) stepLogger {
	return stepLogger{app: s.Metadata.Name, host: strings.ToLower(host), version: s.Artifact.Version}
}

// forHost returns a copy of the logger bound to a different host (cluster nodes
// share the same app/version but log per-node).
func (sl stepLogger) forHost(host string) stepLogger {
	sl.host = strings.ToLower(host)
	return sl
}

// emit writes one structured step record measuring elapsed time from start.
func (sl stepLogger) emit(ctx context.Context, step string, start time.Time) {
	tflog.Info(ctx, "deploy step", map[string]interface{}{
		"app":         sl.app,
		"host":        sl.host,
		"step":        step,
		"version":     sl.version,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

// timed runs fn and emits the step record afterward regardless of outcome.
func (sl stepLogger) timed(ctx context.Context, step string, fn func() error) error {
	start := time.Now()
	err := fn()
	sl.emit(ctx, step, start)
	return err
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
	sl := newStepLogger(s, host)
	vstart := time.Now()
	t, err := e.NewTransport(&s.Target, host)
	if err != nil {
		sl.emit(ctx, "VALIDATE", vstart)
		return nil, coded("ERR_SPEC_INVALID", host, "VALIDATE", err)
	}
	pat, verr := pattern.For(s.Pattern.Type)
	sl.emit(ctx, "VALIDATE", vstart)
	if verr != nil {
		return nil, coded("ERR_UNSUPPORTED", host, "VALIDATE", verr)
	}
	cstart := time.Now()
	if err := t.Connect(ctx); err != nil {
		sl.emit(ctx, "CONNECT", cstart)
		return nil, wrapTransportErr(err, host, "CONNECT")
	}
	sl.emit(ctx, "CONNECT", cstart)
	defer t.Close()

	p := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, s.Artifact.Version)
	rc := releaseCtx(s, p)

	if err := sl.timed(ctx, "PREFLIGHT", func() error { return e.preflight(ctx, t, s, p, pat, rc) }); err != nil {
		return nil, err
	}

	lstart := time.Now()
	lk, warn, err := AcquireLock(ctx, t, p, lockOwner(), "deploy", s.Strategy.EffectiveLockTimeout())
	sl.emit(ctx, "LOCK", lstart)
	if err != nil {
		return nil, err
	}
	if warn != "" {
		e.warnf("%s", warn)
	}
	defer func() {
		rctx, cancel := lockCleanupContext(ctx)
		defer cancel()
		ustart := time.Now()
		if rerr := ReleaseLock(rctx, lk); rerr != nil {
			e.warnf("lock release failed on %s: %v", host, rerr)
		}
		sl.emit(ctx, "UNLOCK", ustart)
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
	// Best-effort collection of logs.paths + windows_event_logs since operation
	// start, on EVERY exit path from here (success, staging failure, rollback) —
	// DESIGN §6.5. No-op when nothing is configured.
	defer e.collectDeploymentLogs(ctx, t, s, p, started)

	// STAGE / FETCH / CHECKSUM / EXTRACT / RENDER — no live mutation yet.
	if err := e.stageOnHost(ctx, sl, t, s, p, pat, rc); err != nil {
		return nil, err
	}

	// SWITCHOVER — from here on, failures trigger rollback (DESIGN §10.3).
	switchErr := e.switchOn(ctx, sl, t, s, p, pat, rc)
	if switchErr == nil {
		switchErr = sl.timed(ctx, "HEALTH", func() error {
			return RunHealthCheck(ctx, t, &s.HealthCheck, p.Current, rc.Env)
		})
	}
	if switchErr != nil {
		return nil, e.rollbackSingle(ctx, sl, t, s, p, pat, prev, started, switchErr)
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
	ferr := sl.timed(ctx, "FINALIZE", func() error { return WriteManifest(ctx, t, p, nm) })
	if ferr != nil {
		return nil, coded("ERR_CONNECT", host, "FINALIZE", ferr)
	}
	if err := sl.timed(ctx, "PRUNE", func() error { return e.pruneReleases(ctx, t, s, p, nm) }); err != nil {
		e.warnf("prune failed on %s: %v", host, err) // never fails the apply
	}
	st, _ := pat.Status(ctx, t, rc)
	return statusFrom(nm, host, st), nil
}

// rollbackSingle implements DESIGN §10.3 for one host. Returns the ORIGINAL
// error (annotated) on successful rollback; ERR_ROLLBACK_FAILED otherwise.
func (e *Engine) rollbackSingle(ctx context.Context, sl stepLogger, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, prev string, started time.Time, orig error) error {
	host := t.Host()
	if !s.Strategy.EffectiveRollback() {
		e.finalizeFailed(ctx, t, s, p, prev, started, "failed")
		return fmt.Errorf("%w; rollback_on_failure=false — target left as-is for inspection", orig)
	}
	rbStart := time.Now()
	if prev == "" {
		// Fresh install failure ⇒ clean the machine (DESIGN §10.3 row 1).
		rcNew := releaseCtx(s, p)
		_ = pat.Stop(ctx, t, rcNew)
		_ = pat.Uninstall(ctx, t, rcNew, false)
		_ = e.removeJunction(ctx, t, p)
		_ = e.removePath(ctx, t, p.Manifest)
		sl.emit(ctx, "ROLLBACK", rbStart)
		return fmt.Errorf("%w; fresh install failed — target cleaned (no service, no junction, no manifest)", orig)
	}
	tflog.Warn(ctx, "deploy failed; rolling back", map[string]interface{}{
		"host": host, "from": s.Artifact.Version, "to": prev, "cause": orig.Error()})

	pp := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, prev)
	rcPrev := releaseCtx(s, pp)
	rb := func() error {
		if err := sl.timed(ctx, "STOP", func() error { return pat.Stop(ctx, t, rcPrev) }); err != nil {
			return err
		}
		if err := sl.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, t, pp) }); err != nil {
			return err
		}
		if err := sl.timed(ctx, "CONFIGURE", func() error { return pat.Configure(ctx, t, rcPrev) }); err != nil {
			return err
		}
		if err := sl.timed(ctx, "START", func() error { return pat.Start(ctx, t, rcPrev) }); err != nil {
			return err
		}
		return sl.timed(ctx, "HEALTH", func() error {
			return RunHealthCheck(ctx, t, &s.HealthCheck, pp.Current, rcPrev.Env)
		})
	}
	rerr := rb()
	sl.emit(ctx, "ROLLBACK", rbStart)
	if rerr != nil {
		e.finalizeFailed(ctx, t, s, pp, prev, started, "failed")
		return coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — manual intervention required; deploy error: %v; rollback error: %v", host, orig, rerr))
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
	// Best-effort log/event collection on every exit path from here (including
	// image-pull failure) — DESIGN §6.5. No-op when nothing is configured.
	defer e.collectDeploymentLogs(ctx, t, s, p, started)
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
				fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s; deploy: %v; rollback: %v", host, runErr, rerr))
		}
		if herr := RunHealthCheck(ctx, t, &s.HealthCheck, p.Root, rc.Env); herr != nil {
			return nil, coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
				fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s; deploy: %v; rollback health: %v", host, runErr, herr))
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

// collectDeploymentLogs pulls the configured log globs and windows event logs
// from the target into a run-specific local dir (DESIGN §6.5 logs.paths /
// logs.windows_event_logs; events `since` = operation start). Relative globs
// resolve under the app's shared dir (`<app>/shared/`, DESIGN §6.5:339).
// Best-effort: failures are surfaced as warnings, never fail the operation.
func (e *Engine) collectDeploymentLogs(ctx context.Context, t transport.Transport,
	s *spec.Deployment, p layout.Paths, started time.Time) {
	if len(s.Logs.Paths) == 0 && len(s.Logs.WindowsEventLogs) == 0 {
		return
	}
	dest := filepath.Join("labdeploy-logs", fmt.Sprintf("%s-%d", s.Metadata.Name, started.Unix()))
	if len(s.Logs.Paths) > 0 {
		globs := resolveTargetGlobs(t.OS(), p.Shared, s.Logs.Paths)
		if _, w := logs.CollectFiles(ctx, t, globs, filepath.Join(dest, "logs")); len(w) > 0 {
			for _, x := range w {
				e.warnf("%s", x)
			}
		}
	}
	if len(s.Logs.WindowsEventLogs) > 0 {
		if _, w := logs.CollectEventLogs(ctx, t, s.Logs.WindowsEventLogs, started, filepath.Join(dest, "events")); len(w) > 0 {
			for _, x := range w {
				e.warnf("%s", x)
			}
		}
	}
}

// resolveTargetGlobs resolves each glob against base on the target: absolute
// globs pass through unchanged; relative globs are joined under base using the
// target OS separator. Used for deployment logs.paths and TestRun collect.logs
// (`app_shared_of`).
func resolveTargetGlobs(os spec.OSKind, base string, globs []string) []string {
	out := make([]string, 0, len(globs))
	for _, g := range globs {
		if isAbsTargetPath(os, g) {
			out = append(out, g)
			continue
		}
		if os == spec.OSWindows {
			out = append(out, base+`\`+strings.ReplaceAll(strings.TrimLeft(g, `\/`), "/", `\`))
		} else {
			out = append(out, base+"/"+strings.TrimLeft(g, "/"))
		}
	}
	return out
}

func isAbsTargetPath(os spec.OSKind, g string) bool {
	if os == spec.OSWindows {
		return strings.HasPrefix(g, `\\`) || (len(g) >= 2 && g[1] == ':')
	}
	return strings.HasPrefix(g, "/")
}

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
mkdir -p %s 2>/dev/null || true
avail=$(df -Pm "$(dirname %s)" | awk 'NR==2{print $4}')
[ "$avail" -ge 500 ] || { echo "free space ${avail}MB < 500MB" >&2; exit 1; }`, shq(p.Root), shq(p.Root))
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
// Every fixed step is logged through sl in execution order; conditional steps
// (FETCH/CHECKSUM/EXTRACT) are simply not entered when the release is cached, so
// they never appear in the log (DESIGN §10.5 / test "conditional steps").
func (e *Engine) stageOnHost(ctx context.Context, sl stepLogger, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, rc pattern.ReleaseCtx) (err error) {
	host := t.Host()
	sl = sl.forHost(host)
	stageStart := time.Now()
	if err := ensureLayout(ctx, t, p); err != nil {
		sl.emit(ctx, "STAGE", stageStart)
		return coded("ERR_CONNECT", host, "STAGE", err)
	}
	// STAGING WIPE (start): staging/ is scratch, wiped whole at the start of
	// EVERY op — cached or not (DESIGN §9.1 "wiped at start & end of every op").
	if err := e.wipeStaging(ctx, t, p); err != nil {
		sl.emit(ctx, "STAGE", stageStart)
		return coded("ERR_CONNECT", host, "STAGE", err)
	}
	sl.emit(ctx, "STAGE", stageStart)
	// STAGING WIPE (end): declared FIRST so it runs LAST (LIFO) — after the
	// incomplete-release cleanup below has settled `err`. A cleanup failure is
	// surfaced as the op error when the op otherwise succeeded (never leave a
	// dirty staging silently); when the op already failed we warn so the root
	// cause is preserved.
	defer func() {
		if werr := e.wipeStaging(ctx, t, p); werr != nil {
			if err == nil {
				err = coded("ERR_CONNECT", host, "STAGE", fmt.Errorf("staging cleanup: %w", werr))
			} else {
				e.warnf("staging cleanup failed on %s after prior error: %v", host, werr)
			}
		}
	}()
	// INCOMPLETE-RELEASE CLEANUP: declared SECOND so it runs FIRST (before the
	// staging wipe), while `err` still reflects the genuine operation result.
	// Any pre-switch failure (fetch/extract/deps/marker/render/post_install)
	// after THIS op created the release removes the partial release so only a
	// staging-only failure state remains (DESIGN §10.2). A cached release we did
	// NOT create survives a render/post_install error. Cleanup failures are
	// joined onto the op error so they cannot masquerade as a clean failure.
	createdRelease := false
	defer func() {
		if err != nil && createdRelease {
			err = e.cleanupIncompleteRelease(ctx, t, p, host, err)
		}
	}()
	cached, cerr := e.releaseCached(ctx, t, p, s.Artifact.Version, s.Artifact.Checksum)
	if cerr != nil {
		return coded("ERR_CONNECT", host, "STAGE", cerr)
	}
	if !cached {
		fetchStart := time.Now()
		ferr := e.fetchToStaging(ctx, t, s, p)
		sl.emit(ctx, "FETCH", fetchStart)
		if ferr != nil {
			return ferr // fetch writes only to staging; release tree untouched
		}
		// FETCH already verified the artifact checksum (target-pull remote
		// verify / runner-push local verify); record CHECKSUM as its own step.
		sl.emit(ctx, "CHECKSUM", time.Now())
		// extract creates the release dir — from here the release is "ours" and
		// every failure below trips the incomplete-release cleanup defer.
		createdRelease = true
		if xerr := sl.timed(ctx, "EXTRACT", func() error { return e.extract(ctx, t, p) }); xerr != nil {
			return xerr
		}
		if n, ok := pat.(*pattern.NodeWebApp); ok {
			if derr := n.InstallDeps(ctx, t, rc); derr != nil {
				return derr
			}
		}
		if merr := e.writeReleaseMarker(ctx, t, p, s); merr != nil {
			return coded("ERR_CONNECT", host, "STAGE", merr)
		}
	} else {
		tflog.Info(ctx, "release cached; skipping fetch/extract",
			map[string]interface{}{"host": host, "version": s.Artifact.Version})
	}
	// RENDER: files land in the release dir every apply (DESIGN §6.4). A failure
	// here on a freshly-created release trips the incomplete-release cleanup.
	renderStart := time.Now()
	for _, f := range s.Files {
		var dest string
		if t.OS() == spec.OSWindows {
			dest = p.Release + `\` + strings.ReplaceAll(strings.TrimLeft(f.Path, `\/`), "/", `\`)
		} else {
			dest = p.Release + "/" + strings.TrimLeft(f.Path, "/")
		}
		if werr := writeSmallFile(ctx, t, dest, f.Content); werr != nil {
			sl.emit(ctx, "RENDER", renderStart)
			return coded("ERR_EXTRACT", host, "RENDER", werr)
		}
	}
	sl.emit(ctx, "RENDER", renderStart)
	// post_install hook runs in release dir after extract, before switchover;
	// non-zero exit ⇒ ERR_SERVICE_INSTALL (DESIGN §6.4).
	if hook := s.Pattern.PostInstall; hook != "" {
		var r transport.Result
		var xerr error
		if t.OS() == spec.OSWindows {
			r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Env: rc.Env, TimeoutSec: 600,
				Script: fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(p.Release), hook)})
		} else {
			r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Env: rc.Env, TimeoutSec: 600,
				Script: fmt.Sprintf("cd %s && %s", shq(p.Release), hook)})
		}
		if xerr != nil {
			return wrapTransportErr(xerr, host, "STAGE")
		}
		if r.ExitCode != 0 {
			return coded("ERR_SERVICE_INSTALL", host, "STAGE",
				fmt.Errorf("post_install exit=%d: %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout)))
		}
	}
	return nil
}

// cleanupIncompleteRelease removes a release dir that THIS op created after a
// pre-switch failure (DESIGN §10.2 staging-only failure state) and JOINS any
// removal error onto the operation error, so a failed cleanup cannot masquerade
// as a clean staging-only failure.
func (e *Engine) cleanupIncompleteRelease(ctx context.Context, t transport.Transport, p layout.Paths, host string, opErr error) error {
	if rerr := e.removePath(ctx, t, p.Release); rerr != nil {
		return errors.Join(opErr, coded("ERR_CONNECT", host, "STAGE",
			fmt.Errorf("incomplete-release cleanup: %w", rerr)))
	}
	return opErr
}

// switchOn = STOP + SWITCH + CONFIGURE + START for one host (DESIGN §10.1 7–10).
func (e *Engine) switchOn(ctx context.Context, sl stepLogger, t transport.Transport, s *spec.Deployment,
	p layout.Paths, pat pattern.Pattern, rc pattern.ReleaseCtx) error {
	sl = sl.forHost(t.Host())
	if err := sl.timed(ctx, "STOP", func() error { return pat.Stop(ctx, t, rc) }); err != nil {
		return err
	}
	if err := sl.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, t, p) }); err != nil {
		return err
	}
	if err := sl.timed(ctx, "CONFIGURE", func() error { return pat.Configure(ctx, t, rc) }); err != nil {
		return err
	}
	return sl.timed(ctx, "START", func() error { return pat.Start(ctx, t, rc) })
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
		Script: fmt.Sprintf(`mkdir -p %s`, shq(dir)), TimeoutSec: 60})
	if err != nil || r.ExitCode != 0 {
		return fmt.Errorf("mkdir %s: err=%v", dir, err)
	}
	return nil
}

const releaseMarker = ".labdeploy-release.json"

// releaseCached reports whether the on-host release marker proves the wanted
// version is already fully extracted. It parses `.labdeploy-release.json` and
// requires ALL of {version, sha256, extracted_at} to be present and for version
// + sha256 to match the request; a missing, malformed, or partially-written
// marker is treated as "not cached" so the release is re-fetched/extracted.
func (e *Engine) releaseCached(ctx context.Context, t transport.Transport, p layout.Paths, wantVersion, wantChecksum string) (bool, error) {
	marker := p.Release + sepFor(t.OS()) + releaseMarker
	raw, ok, err := readSmallFile(ctx, t, marker)
	if err != nil || !ok {
		return false, err
	}
	var m struct {
		Version     string `json:"version"`
		SHA256      string `json:"sha256"`
		ExtractedAt string `json:"extracted_at"`
	}
	if jerr := json.Unmarshal([]byte(raw), &m); jerr != nil {
		return false, nil // malformed marker ⇒ re-stage rather than trust it
	}
	if m.Version == "" || m.SHA256 == "" || m.ExtractedAt == "" {
		return false, nil // incomplete marker ⇒ re-stage
	}
	return m.Version == wantVersion && m.SHA256 == wantChecksum, nil
}

func (e *Engine) writeReleaseMarker(ctx context.Context, t transport.Transport, p layout.Paths, s *spec.Deployment) error {
	marker := p.Release + sepFor(t.OS()) + releaseMarker
	content := fmt.Sprintf("{\n  \"version\": %q,\n  \"sha256\": %q,\n  \"extracted_at\": %q\n}\n",
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
	shell := transport.ShellSh
	if t.OS() == spec.OSWindows {
		shell = transport.ShellPowerShell
	}
	r, err := t.Exec(ctx, transport.Cmd{Shell: shell, Script: extractScript(p), TimeoutSec: 900})
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
	shell := transport.ShellSh
	if t.OS() == spec.OSWindows {
		shell = transport.ShellPowerShell
	}
	r, err := t.Exec(ctx, transport.Cmd{Shell: shell, Script: switchScript(p), TimeoutSec: 60})
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
		Script: fmt.Sprintf(`rm -f %s`, shq(p.Current)), TimeoutSec: 60})
	return err
}

func (e *Engine) removePath(ctx context.Context, t transport.Transport, path string) error {
	if t.OS() == spec.OSWindows {
		// SilentlyContinue makes a missing path a no-op (idempotent), but a
		// real removal failure (locked/permission) leaves the path present, so
		// we re-check and exit nonzero to surface it to the caller.
		script := fmt.Sprintf(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue %s
if(Test-Path %s){ Write-Error 'remove failed'; exit 1 }
exit 0`, psq(path), psq(path))
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 300})
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("remove %s: %s", path, strings.TrimSpace(r.Stderr+r.Stdout))
		}
		return nil
	}
	// `rm -rf` is a no-op on a missing path; the `[ ! -e ]` guard turns a real
	// removal failure into a nonzero exit the caller can propagate.
	script := fmt.Sprintf("rm -rf %s\n[ ! -e %s ]", shq(path), shq(path))
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 300})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("remove %s: %s", path, strings.TrimSpace(r.Stderr+r.Stdout))
	}
	return nil
}

// wipeStaging removes the entire staging directory tree and recreates it empty
// (DESIGN §9.1: staging/ is "wiped at start & end of every op"). Called before
// fetch and, via defer, on every stage exit regardless of success or failure so
// that fetch/checksum/extract failures never leave partial artifacts behind.
func (e *Engine) wipeStaging(ctx context.Context, t transport.Transport, p layout.Paths) error {
	if err := e.removePath(ctx, t, p.Staging); err != nil {
		return err
	}
	return ensureDir(ctx, t, p.Staging)
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
			Script: fmt.Sprintf(`for d in %s/*/; do [ -d "$d" ] && printf '%%s|%%s\n' "$(basename "$d")" "$(stat -c %%Y "$d")"; done`, shq(p.Releases))})
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
	// keep_releases is the TOTAL retention budget (DESIGN CAP-02: keep=2 ⇒ exactly
	// 2 dirs = newest+previous). current + previous are ALWAYS retained and count
	// toward the budget; remaining slots go to the newest non-protected releases.
	protected := map[string]bool{m.CurrentVersion: true}
	if m.PreviousVersion != "" {
		protected[m.PreviousVersion] = true
	}
	protectedExisting := 0
	for _, r := range rels {
		if protected[r.name] {
			protectedExisting++
		}
	}
	slots := keep - protectedExisting // budget left for non-protected releases
	if slots < 0 {
		slots = 0 // protected (current+previous) is never deleted, even if > keep
	}
	filled := 0
	var firstErr error
	for _, r := range rels { // newest first
		if protected[r.name] {
			continue // current/previous: always retained
		}
		if filled < slots {
			filled++
			continue
		}
		if err := e.removePath(ctx, t, p.Releases+sepFor(t.OS())+r.name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("prune %s: %w", r.name, err)
		}
	}
	return firstErr
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
