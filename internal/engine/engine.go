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

// warnOnce appends msg only if it is not already present. It exists so
// per-host gates (e.g. the DESIGN §11 insecure-transport notices in preflight,
// which the cluster path runs for every node) surface EXACTLY ONE WARN diag per
// apply rather than one per host.
func (e *Engine) warnOnce(msg string) {
	for _, w := range e.Warnings {
		if w == msg {
			return
		}
	}
	e.Warnings = append(e.Warnings, msg)
}

// InsecureTransportWarnings returns the DESIGN §11 lab-insecure transport WARN
// messages for a spec: an unpinned SSH host_key ("" ⇒ accept any host key) and/or
// WinRM insecure_skip_verify=true (TLS certificate verification disabled). It is the
// SINGLE SOURCE OF TRUTH for those message strings, consumed both by the apply-time
// preflight (deduped to exactly one per apply via warnOnce) and by the provider surface
// tests that assert the framework WARN diagnostic wording. A secure spec returns nil.
func InsecureTransportWarnings(s *spec.Deployment) []string {
	var out []string
	if s.Target.Transport == spec.TransportSSH && s.Target.SSH.HostKey == "" {
		out = append(out, "ssh.host_key not pinned — accepting any host key (lab default; DESIGN §11)")
	}
	if s.Target.Transport == spec.TransportWinRM &&
		s.Target.WinRM.InsecureSkipVerify != nil && *s.Target.WinRM.InsecureSkipVerify {
		out = append(out, "winrm.insecure_skip_verify=true — TLS certificate verification disabled (lab default; DESIGN §11)")
	}
	return out
}

// warnInsecureTransport emits the DESIGN §11 lab-insecure transport notices EXACTLY
// ONCE per apply. The messages omit the host so warnOnce dedups them to a single notice
// even though the cluster path preflights every node; ReadStatus never calls preflight,
// so a plan/refresh stays silent.
func (e *Engine) warnInsecureTransport(s *spec.Deployment) {
	for _, w := range InsecureTransportWarnings(s) {
		e.warnOnce(w)
	}
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

// isChecksumErr reports whether err (or any error it wraps) is a coded
// ERR_CHECKSUM_MISMATCH, so the caller can record the CHECKSUM step as the
// failing step even though verification happens inside the fetch.
func isChecksumErr(err error) bool {
	if err == nil {
		return false
	}
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code == "ERR_CHECKSUM_MISMATCH"
	}
	return false
}

// Update is the entry point for a resource Update (DESIGN §10.2). When the desired
// spec leaves the artifact (version + checksum) unchanged from prior — a
// configuration-only change — Deploy's idempotency short-circuit (§10.1 step 2)
// would skip CONFIGURE and silently drop the new mutable settings while Terraform
// records them. For a single-target windows_service that case is routed through the
// transactional, rollback-safe Reconfigure path so the change is actually applied.
// Every other case (a new/changed artifact, or a pattern/topology Reconfigure does
// not cover) falls through to Deploy, which is itself idempotent.
func (e *Engine) Update(ctx context.Context, s, prior *spec.Deployment) (*Status, error) {
	if reconfigurable(s, prior) {
		st, err := e.Reconfigure(ctx, s, prior)
		if err != nil {
			return nil, err
		}
		if st != nil {
			return st, nil
		}
		// Reconfigure returns (nil, nil) when nothing is deployed yet (no manifest).
		// A configuration-only "update" against an absent deployment is meaningless;
		// fall through to a full Deploy rather than returning a nil status that the
		// caller would dereference.
	}
	return e.Deploy(ctx, s)
}

// reconfigurable reports whether a same-artifact configuration change should be
// applied via Reconfigure rather than Deploy. Reconfigure is single-host and
// defined for the SCM-registered service patterns (windows_service and the
// node_web_app / dotnet_api patterns that delegate their Configure/Stop/Start/
// Status verbs to WindowsService); anything else defers to Deploy.
// artifactUnchanged gates it so version upgrades always deploy.
//
// node_web_app is included even when install_deps toggles: Reconfigure runs the
// pattern Preflight (node/dotnet tool checks) and, for node_web_app, re-runs
// InstallDeps (`npm ci --omit=dev`) at the pre-activation point, so a same-
// artifact install_deps=false→true change actually installs dependencies instead
// of being swallowed by Deploy's unchanged-artifact idempotency short-circuit.
func reconfigurable(s, prior *spec.Deployment) bool {
	if prior == nil || s == nil {
		return false
	}
	switch s.Pattern.Type {
	case spec.PatternWindowsService, spec.PatternNodeWebApp, spec.PatternDotnetAPI:
		// applied via Preflight + Stop/Configure/Start (+ node InstallDeps) on the
		// deployed release, all handled by Reconfigure.
	default:
		return false
	}
	if len(s.Target.Hosts) != 1 {
		return false
	}
	return artifactUnchanged(s, prior)
}

// artifactUnchanged reports whether two specs point at the same artifact — the
// signal that a change is configuration-only.
func artifactUnchanged(s, prior *spec.Deployment) bool {
	return s.Artifact.Version == prior.Artifact.Version &&
		s.Artifact.Checksum == prior.Artifact.Checksum
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
		return e.deployDocker(ctx, sl, t, s, p, pat.(*pattern.DockerContainer), rc, m)
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
	prevChecksum := ""
	if m != nil {
		prev = m.CurrentVersion
		prevChecksum = m.ArtifactChecksum
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
		return nil, e.rollbackSingle(ctx, sl, t, s, p, pat, prev, prevChecksum, started, switchErr)
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
	p layout.Paths, pat pattern.Pattern, prev, prevChecksum string, started time.Time, orig error) error {
	host := t.Host()
	if !s.Strategy.EffectiveRollback() {
		// §10.2 last row: record last_operation=failed so Read reports drift. A
		// failed-manifest write error must not be swallowed — it is folded into the
		// surfaced error so the caller learns the failed state was not persisted
		// (evaluator item 4). On a FRESH failure (prev=="") the attempted version
		// is recorded so §10.2 persistence still happens instead of no-op'ing on an
		// empty version (evaluator iter5 item 1).
		failVer := prev
		if failVer == "" {
			failVer = s.Artifact.Version
		}
		ferr := e.finalizeFailed(ctx, sl, t, s, p, failVer, started, "failed")
		out := fmt.Errorf("%w; rollback_on_failure=false — target left as-is for inspection", orig)
		if ferr != nil {
			return coded("ERR_CONNECT", host, "FINALIZE",
				fmt.Errorf("%v; failed-manifest write error: %v", out, ferr))
		}
		return out
	}
	rbStart := time.Now()
	if prev == "" {
		// Fresh install failure ⇒ clean the machine (DESIGN §10.3 row 1). Every
		// cleanup action's error is collected: a failed cleanup means the machine
		// is NOT actually clean, so we surface ERR_ROLLBACK_FAILED rather than
		// falsely report a cleaned target (evaluator iter2 item 6).
		rcNew := releaseCtx(s, p)
		rcNew.EmitStep = sl.forHost(host).emit
		var cleanupErrs []error
		if err := pat.Stop(ctx, t, rcNew); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("stop: %w", err))
		}
		if err := pat.Uninstall(ctx, t, rcNew, false); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("uninstall: %w", err))
		}
		if err := e.removeJunction(ctx, t, p); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("junction: %w", err))
		}
		// Remove the release tree THIS op created. For patterns whose hooks run
		// PRE-switch (windows_service post_install), stageOnHost's incomplete-
		// release cleanup already did this; but console_app runs post_install and
		// verify_command POST-switch (DESIGN §9.3), so a fresh console failure
		// reaches here with the release still on disk. Deleting it honors the
		// §10.2 fresh-install contract ("machine clean") for every pattern
		// (evaluator iter2 item 2). Idempotent: removePath no-ops when absent.
		if err := e.removePath(ctx, t, p.Release); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("release: %w", err))
		}
		if err := e.removePath(ctx, t, p.Manifest); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("manifest: %w", err))
		}
		sl.emit(ctx, "ROLLBACK", rbStart)
		if len(cleanupErrs) > 0 {
			// §10.6: persist a failed manifest recording the unknown state before
			// surfacing ERR_ROLLBACK_FAILED. The fresh cleanup may have removed the
			// manifest, so write one at the attempted (current) version recording
			// last_operation.result=failed; a write failure is folded into the
			// diagnostic so we never claim persistence that did not happen.
			ferr := e.finalizeFailed(ctx, sl, t, s, p, s.Artifact.Version, started, "failed")
			detail := fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — manual intervention required; deploy error: %v; fresh-install cleanup error: %v",
				host, orig, errors.Join(cleanupErrs...))
			if ferr != nil {
				detail = fmt.Errorf("%v; failed-manifest write error: %v", detail, ferr)
			}
			return coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK", detail)
		}
		return fmt.Errorf("%w; fresh install failed — target cleaned (no service, no junction, no manifest)", orig)
	}
	tflog.Warn(ctx, "deploy failed; rolling back", map[string]interface{}{
		"host": host, "from": s.Artifact.Version, "to": prev, "cause": orig.Error()})

	pp := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, prev)
	// Restore the PREVIOUS release: thread `prev` so LD_VERSION/ReleaseCtx.Version
	// reflect the version being brought back up, not the failed s.Artifact.Version
	// (DESIGN §9.1). Otherwise Configure would bake the new version's LD_VERSION
	// into the restored previous-version service's SCM Environment value.
	rcPrev := releaseCtxVersion(s, pp, prev)
	rcPrev.EmitStep = sl.forHost(host).emit
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
		fmErr := e.finalizeFailed(ctx, sl, t, s, pp, prev, started, "failed")
		detail := fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — manual intervention required; deploy error: %v; rollback error: %v", host, orig, rerr)
		if fmErr != nil {
			detail = fmt.Errorf("%v; failed-manifest write error: %v", detail, fmErr)
		}
		return coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK", detail)
	}
	// Restore succeeded: persist a manifest reflecting the previous version as the
	// current state, INCLUDING its artifact checksum so the recorded manifest is
	// consistent with what is actually running. A WriteManifest failure leaves the
	// manifest out of sync with the restored machine, so it is surfaced as
	// ERR_ROLLBACK_FAILED rather than silently ignored (evaluator iter2 item 7).
	m := &Manifest{
		Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
		CurrentVersion: prev, CurrentRelease: pp.Release, ArtifactChecksum: prevChecksum,
		ProviderVersion: ProviderVersion,
		LastOperation: LastOp{Type: "deploy", Result: "rolled_back",
			Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
	}
	if werr := sl.timed(ctx, "FINALIZE", func() error { return WriteManifest(ctx, t, pp, m) }); werr != nil {
		return coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — restored to %s but manifest write failed: %v; deploy error: %v",
				host, prev, werr, orig))
	}
	return fmt.Errorf("%w; rolled back to %s (healthy)", orig, prev)
}

// finalizeFailed persists a manifest recording last_operation.result=<result>
// (DESIGN §10.2/§10.6). The manifest write is emitted as a structured FINALIZE
// step (every executed fixed step, including failure/rollback occurrences, must
// carry app/host/step/version/duration_ms — evaluator iter5 item 3).
func (e *Engine) finalizeFailed(ctx context.Context, sl stepLogger, t transport.Transport, s *spec.Deployment,
	p layout.Paths, current string, started time.Time, result string) error {
	if current == "" {
		return nil
	}
	m, _ := ReadManifest(ctx, t, p)
	if m == nil {
		m = &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
			CurrentVersion: current, CurrentRelease: p.Release, ProviderVersion: ProviderVersion}
	}
	m.LastOperation = LastOp{Type: "deploy", Result: result,
		Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)}
	if err := sl.timed(ctx, "FINALIZE", func() error { return WriteManifest(ctx, t, p, m) }); err != nil {
		e.warnf("failed-manifest write on %s: %v", t.Host(), err)
		return err
	}
	return nil
}

// deployDocker = D-steps (DESIGN §9.6) with image-id rollback. Emits the same
// structured step records as the file-based path for its executed steps: FETCH
// (image pull), START (rm -f old + run new), HEALTH, FINALIZE (manifest write),
// and ROLLBACK on failure — each carrying app/host/step/version/duration_ms.
func (e *Engine) deployDocker(ctx context.Context, sl stepLogger, t transport.Transport, s *spec.Deployment,
	p layout.Paths, dc *pattern.DockerContainer, rc pattern.ReleaseCtx, m *Manifest) (*Status, error) {
	host := t.Host()
	sl = sl.forHost(host)
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
	fetchStart := time.Now()
	perr := dc.Pull(ctx, t, rc)
	sl.emit(ctx, "FETCH", fetchStart)
	if perr != nil {
		return nil, perr
	}
	if cur, err := dc.CurrentImageID(ctx, t, rc); err == nil && cur != "" {
		oldImage = cur
	}
	runErr := sl.timed(ctx, "START", func() error { return dc.Start(ctx, t, rc) }) // rm -f old + run new
	if runErr == nil {
		runErr = sl.timed(ctx, "HEALTH", func() error {
			return RunHealthCheck(ctx, t, &s.HealthCheck, p.Root, rc.Env)
		})
	}
	if runErr != nil {
		if !s.Strategy.EffectiveRollback() || oldImage == "" {
			// §10.2: record last_operation=failed so Read reports drift even when no
			// rollback is performed (rollback disabled or previous image unknown).
			// Record at the previous version if known, else the attempted version so
			// a fresh docker failure still persists failed state (evaluator iter5 item 2).
			failVer := prev
			if failVer == "" {
				failVer = s.Artifact.Version
			}
			fmErr := e.dockerFinalizeFailed(ctx, sl, t, s, p, failVer, oldImage, started)
			out := fmt.Errorf("%w; no docker rollback performed (prev image unknown or rollback disabled)", runErr)
			if fmErr != nil {
				return nil, coded("ERR_CONNECT", host, "FINALIZE",
					fmt.Errorf("%v; failed-manifest write error: %v", out, fmErr))
			}
			return nil, out
		}
		rbStart := time.Now()
		// §9.6 D6 / §10.4: restore the PREVIOUS release. The restored container must
		// advertise the previous version, so rebuild the pattern context at `prev`
		// (LD_VERSION and the -e env baked into `docker run` reflect the running
		// release) rather than reusing the failed new-version rc. Health is rechecked
		// with the same previous-version env.
		rbRC := rc
		if prev != "" {
			rbRC = releaseCtxVersion(s, p, prev)
		}
		rerr := dc.RunNew(ctx, t, rbRC, oldImage)
		if rerr == nil {
			rerr = RunHealthCheck(ctx, t, &s.HealthCheck, p.Root, rbRC.Env)
		}
		sl.emit(ctx, "ROLLBACK", rbStart)
		if rerr != nil {
			// §10.6: persist a failed manifest before surfacing ERR_ROLLBACK_FAILED.
			// When the old container exists but no prior manifest does (prev==""),
			// record at the attempted version so the failed state is still persisted
			// (evaluator iter6 item 2).
			failVer := prev
			if failVer == "" {
				failVer = s.Artifact.Version
			}
			fmErr := e.dockerFinalizeFailed(ctx, sl, t, s, p, failVer, oldImage, started)
			detail := fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — manual intervention required; deploy: %v; rollback: %v", host, runErr, rerr)
			if fmErr != nil {
				detail = fmt.Errorf("%v; failed-manifest write error: %v", detail, fmErr)
			}
			return nil, coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK", detail)
		}
		// §10.2/§10.4: the container is back on the previous image and running.
		// Persist a rolled_back manifest at the previous version (image id = the
		// restored image) so Read reports the restored state instead of treating the
		// resource as absent — mirroring the file-based rollback (rollbackSingle) and
		// the docker failed-rollback branch, both of which persist a manifest. A
		// FINALIZE failure here leaves the machine restored but unrecorded, so it is
		// surfaced as ERR_ROLLBACK_FAILED (UNKNOWN STATE) exactly like the file path.
		// Skipped when no previous version is known (unmanaged container): there is
		// no version to record.
		if prev != "" {
			rm := &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
				CurrentVersion: prev, CurrentRelease: "docker://" + rc.Spec.Pattern.ContainerName,
				ProviderVersion: ProviderVersion,
				Extra:           map[string]string{"image_id": oldImage},
				LastOperation: LastOp{Type: "deploy", Result: "rolled_back",
					Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
			}
			if werr := sl.timed(ctx, "FINALIZE", func() error {
				if derr := ensureDir(ctx, t, p.Root); derr != nil {
					return derr
				}
				return WriteManifest(ctx, t, p, rm)
			}); werr != nil {
				return nil, coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK",
					fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — restored to previous image %s but manifest write failed: %v; deploy error: %v",
						host, short(oldImage), werr, runErr))
			}
		}
		return nil, fmt.Errorf("%w; rolled back to previous image %s", runErr, short(oldImage))
	}
	// D3-equivalent post-run inspect: the recorded image id backs a FUTURE
	// rollback, so a missing/failed inspect means the deploy is NOT durably
	// recorded — fail closed instead of persisting a success manifest with an
	// empty image_id (evaluator iter2 item 1).
	newImage, ierr := dc.CurrentImageID(ctx, t, rc)
	if ierr != nil || strings.TrimSpace(newImage) == "" {
		return nil, coded("ERR_CONNECT", host, "FINALIZE",
			fmt.Errorf("container started but its image id could not be recorded (inspect err=%v, image_id=%q); deploy not durably recorded", ierr, newImage))
	}
	nm := &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
		CurrentVersion: s.Artifact.Version, PreviousVersion: prev,
		CurrentRelease:  "docker://" + rc.Spec.Pattern.ContainerName,
		ProviderVersion: ProviderVersion,
		Extra:           map[string]string{"image_id": newImage},
		LastOperation: LastOp{Type: "deploy", Result: "success",
			Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
	}
	// A FINALIZE failure means the new container is running but its manifest was
	// not persisted — the deploy is NOT durably recorded, so fail closed instead
	// of reporting success (evaluator item 9).
	if ferr := sl.timed(ctx, "FINALIZE", func() error {
		if err := ensureDir(ctx, t, p.Root); err != nil {
			return err
		}
		return WriteManifest(ctx, t, p, nm)
	}); ferr != nil {
		return nil, coded("ERR_CONNECT", host, "FINALIZE", ferr)
	}
	st, _ := dc.Status(ctx, t, rc)
	return statusFrom(nm, host, st), nil
}

// dockerFinalizeFailed persists a failed manifest for the docker path (DESIGN
// §10.2/§10.6) recording last_operation.result=failed. The manifest write is
// emitted as a structured FINALIZE step (evaluator iter5 item 3).
func (e *Engine) dockerFinalizeFailed(ctx context.Context, sl stepLogger, t transport.Transport, s *spec.Deployment,
	p layout.Paths, prev, prevImage string, started time.Time) error {
	if prev == "" {
		return nil
	}
	m := &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
		CurrentVersion: prev, CurrentRelease: "docker://" + s.Pattern.ContainerName,
		ProviderVersion: ProviderVersion,
		Extra:           map[string]string{"image_id": prevImage},
		LastOperation: LastOp{Type: "deploy", Result: "failed",
			Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
	}
	if err := sl.timed(ctx, "FINALIZE", func() error {
		if derr := ensureDir(ctx, t, p.Root); derr != nil {
			return derr
		}
		return WriteManifest(ctx, t, p, m)
	}); err != nil {
		e.warnf("docker failed-manifest write on %s: %v", t.Host(), err)
		return err
	}
	return nil
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

// releaseCtx builds the pattern context for a FORWARD operation, whose release
// version is the spec's desired artifact version (paths and env agree).
func releaseCtx(s *spec.Deployment, p layout.Paths) pattern.ReleaseCtx {
	return releaseCtxVersion(s, p, s.Artifact.Version)
}

// releaseCtxVersion builds the pattern context for the release identified by
// `version` — the version actually being (re)configured on the target, which is
// NOT necessarily s.Artifact.Version. layout.Paths carries only the version-
// independent `current` junction (LD_RELEASE_DIR), so the running version has to
// be threaded in explicitly. Rollback/restore MUST pass the PREVIOUS version so
// LD_VERSION (and ReleaseCtx.Version) reflect the running release per DESIGN
// §9.1: windows_service bakes rc.Env — including LD_VERSION — into the SCM
// Environment value, so a restored previous-version service would otherwise come
// up advertising the failed/new version and its /health body `v=<LD_VERSION>`
// (§18) would be wrong.
func releaseCtxVersion(s *spec.Deployment, p layout.Paths, version string) pattern.ReleaseCtx {
	nodePort := 0
	if s.Pattern.Type == spec.PatternNodeWebApp {
		nodePort = s.Pattern.Port
	}
	env := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, version, p, nodePort), s.Environment)
	return pattern.ReleaseCtx{App: s.Metadata.Name, Version: version,
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
	// DESIGN §11: lab-insecure transport settings are permitted but MUST emit
	// exactly one WARN diag per apply — see warnInsecureTransport (dedups across
	// the cluster path's per-host preflights).
	e.warnInsecureTransport(s)
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
	// NOTE: staging-dir prep (ensureLayout + wipe) runs first physically, but the
	// canonical step order (implementation-plan.md:224 / DESIGN §8.5) is
	// FETCH → CHECKSUM → STAGE → EXTRACT. The STAGE record is therefore emitted
	// AFTER fetch+verify below (the verified package is what gets "staged" for
	// extraction), so the emitted trace matches the pinned fixed-step order.
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
		// FETCH covers the download; CHECKSUM covers artifact verification. Both
		// are emitted even on failure so the step trace always records where
		// staging stopped. A checksum-mismatch is surfaced by fetchToStaging as a
		// CodedError(ERR_CHECKSUM_MISMATCH): the download reached the verify
		// stage, so FETCH is recorded as done and CHECKSUM is recorded as the
		// failing step.
		sl.emit(ctx, "FETCH", fetchStart)
		if isChecksumErr(ferr) {
			sl.emit(ctx, "CHECKSUM", time.Now())
			return ferr
		}
		if ferr != nil {
			return ferr // fetch writes only to staging; release tree untouched
		}
		sl.emit(ctx, "CHECKSUM", time.Now())
		// STAGE: the verified package is now staged, ready for extraction.
		sl.emit(ctx, "STAGE", stageStart)
		// extract creates the release dir — from here the release is "ours" and
		// every failure below trips the incomplete-release cleanup defer.
		createdRelease = true
		if xerr := sl.timed(ctx, "EXTRACT", func() error { return e.extract(ctx, t, p) }); xerr != nil {
			return xerr
		}
		if merr := e.writeReleaseMarker(ctx, t, p, s); merr != nil {
			return coded("ERR_CONNECT", host, "STAGE", merr)
		}
	} else {
		// Cached release: FETCH/CHECKSUM/EXTRACT are skipped (conditional steps),
		// but STAGE still records that the cached package is staged for this op.
		sl.emit(ctx, "STAGE", stageStart)
		tflog.Info(ctx, "release cached; skipping fetch/extract",
			map[string]interface{}{"host": host, "version": s.Artifact.Version})
	}
	// node_web_app dependency install (`npm ci --omit=dev`) runs for BOTH freshly
	// extracted AND cached releases (DESIGN §9.4). A cached release previously
	// deployed with install_deps=false must still get its deps installed (and its
	// package-lock.json validated) when this apply flips install_deps=true — so
	// InstallDeps cannot live only on the fresh-extract path. It no-ops for other
	// patterns and when install_deps=false. On a freshly-created release a failure
	// trips the incomplete-release cleanup (createdRelease=true); on a cached
	// release we did not create the tree survives for inspection.
	if n, ok := pat.(*pattern.NodeWebApp); ok {
		if derr := n.InstallDeps(ctx, t, rc); derr != nil {
			return derr
		}
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
	// non-zero exit ⇒ ERR_SERVICE_INSTALL (DESIGN §6.4). console_app is the sole
	// exception: DESIGN §9.3 sequences its post_install AFTER switch in <cur>, so
	// the ConsoleApp pattern owns that hook (in Configure) — running it here too
	// would execute post_install twice.
	if err := e.runPostInstall(ctx, t, s, p.Release, rc, host); err != nil {
		return err
	}
	return nil
}

// runPostInstall executes the pattern's post_install hook in the given release
// directory (DESIGN §6.4). It is a no-op when no hook is configured or for the
// console_app pattern (which sequences its own hook after switchover). It is
// invoked both from staging (fresh/upgrade deploys) and from Reconfigure, so a
// configuration-only update that changes post_install actually runs the new hook
// rather than silently recording it in Terraform state.
func (e *Engine) runPostInstall(ctx context.Context, t transport.Transport, s *spec.Deployment,
	releasePath string, rc pattern.ReleaseCtx, host string) error {
	hook := s.Pattern.PostInstall
	if hook == "" || s.Pattern.Type == spec.PatternConsoleApp {
		return nil
	}
	var r transport.Result
	var xerr error
	if t.OS() == spec.OSWindows {
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Env: rc.Env, TimeoutSec: 600,
			Script: fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(releasePath), hook)})
	} else {
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Env: rc.Env, TimeoutSec: 600,
			Script: fmt.Sprintf("cd %s && %s", shq(releasePath), hook)})
	}
	if xerr != nil {
		return wrapTransportErr(xerr, host, "STAGE")
	}
	if r.ExitCode != 0 {
		return coded("ERR_SERVICE_INSTALL", host, "STAGE",
			fmt.Errorf("post_install exit=%d: %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout)))
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
	rc.EmitStep = sl.emit
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
			// A checksum mismatch is a CHECKSUM-step failure even though it is
			// detected during the runner-side fetch+verify (DESIGN §8.5: the
			// diagnostic step must match the actual failing operation).
			step := "FETCH"
			if ce.Code == "ERR_CHECKSUM_MISMATCH" {
				step = "CHECKSUM"
			}
			return coded(ce.Code, host, step, ce.Err)
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
	// Read reports the CURRENT deployed state, so the pattern context's version
	// MUST be the manifest's current_version — NOT s.Artifact.Version (the desired
	// version). Otherwise console_app.Status, which compares the on-host
	// `.labdeploy-release.json` marker version against ReleaseCtx.Version, would
	// falsely report drift whenever a plan desires a version different from what
	// is deployed (evaluator iter2 item 1). Marker/manifest agreement ⇒ n/a.
	st, serr := pat.Status(ctx, t, releaseCtxVersion(s, rp, m.CurrentVersion))
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

// Reconfigure re-applies pattern configuration (a Windows service's description,
// start type, recovery actions, arguments, and environment) to the CURRENTLY
// deployed release and restarts it so the new settings take effect. It exists
// because a configuration-only change — one that leaves artifact.version and
// artifact.checksum untouched — hits Deploy's idempotency short-circuit
// (DESIGN §10.1 step 2) and would otherwise never reach CONFIGURE, silently
// skipping the mutable settings while Terraform records the new desired state.
//
// It is transactional, concurrency-safe, and rollback-safe. It acquires the SAME
// per-app deploy lock BEFORE it reads the manifest (so a concurrent deploy/destroy
// cannot complete between the manifest read and our mutations and leave us
// configuring — and re-finalizing — a stale release), then runs STOP → CONFIGURE →
// START → HEALTH so the new configuration is actually active on the running
// process (not just written to disk), and only then re-finalizes the manifest.
// If any step after STOP fails, it restores the PRIOR configuration (the settings
// Terraform still records in state) and health-checks it, so the machine is never
// left stopped or running rejected settings; the original error is surfaced so the
// apply fails and state stays accurate. If restoration itself fails it persists a
// failed manifest (DESIGN §10.6) and returns ERR_ROLLBACK_FAILED. Returns a nil
// status with a nil error when nothing is deployed yet. Single-host.
func (e *Engine) Reconfigure(ctx context.Context, s, prior *spec.Deployment) (*Status, error) {
	host := strings.ToLower(s.Target.Hosts[0])
	sl := newStepLogger(s, host)
	if prior == nil {
		prior = s
	}
	t, err := e.NewTransport(&s.Target, host)
	if err != nil {
		return nil, coded("ERR_SPEC_INVALID", host, "VALIDATE", err)
	}
	pat, verr := pattern.For(s.Pattern.Type)
	if verr != nil {
		return nil, coded("ERR_UNSUPPORTED", host, "VALIDATE", verr)
	}
	if err := t.Connect(ctx); err != nil {
		return nil, wrapTransportErr(err, host, "CONNECT")
	}
	defer t.Close()
	// The lock and manifest live at the app root (version-independent), so we can
	// build the lock paths and acquire the lock BEFORE reading the manifest.
	p := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, s.Artifact.Version)
	lstart := time.Now()
	lk, lwarn, err := AcquireLock(ctx, t, p, lockOwner(), "reconfigure", s.Strategy.EffectiveLockTimeout())
	sl.emit(ctx, "LOCK", lstart)
	if err != nil {
		return nil, err
	}
	if lwarn != "" {
		e.warnf("%s", lwarn)
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
	// Now that we hold the lock, read the manifest under it — nothing can change
	// current_version/checksum out from under us for the rest of this operation.
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
	// Reconfigure the CURRENT deployed version's release (mirrors ReadStatus): the
	// pattern context version MUST be the manifest's current_version so we target
	// the live binary path, not a desired version that is not yet on disk.
	rp := layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, m.CurrentVersion)
	newRC := releaseCtxVersion(s, rp, m.CurrentVersion)
	newRC.EmitStep = sl.emit
	// Pattern preflight MUST run before any mutation (DESIGN §8.1): a same-artifact
	// change to node_exe, or a launcher=exe→dotnet_dll switch, must validate the new
	// tooling (node --version / dotnet --list-runtimes contains Microsoft.AspNetCore.App)
	// BEFORE we stop and reconfigure the running service — otherwise the service could
	// be torn down and re-registered against a launcher the target cannot run.
	if err := sl.timed(ctx, "PREFLIGHT", func() error { return e.preflight(ctx, t, s, rp, pat, newRC) }); err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	writeManifest := func(result string) *Manifest {
		return &Manifest{
			Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
			CurrentVersion: m.CurrentVersion, PreviousVersion: m.PreviousVersion,
			CurrentRelease: rp.Release, ArtifactChecksum: m.ArtifactChecksum,
			ProviderVersion: ProviderVersion,
			LastOperation: LastOp{Type: "reconfigure", Result: result,
				Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
		}
	}
	// activate follows DESIGN §10.2: STOP → CONFIGURE → START → HEALTH for a given
	// release context and health check. It is used both to apply the new settings
	// and to RESTORE the prior ones — the restore path MUST stop the service that is
	// running the rejected configuration before re-applying the prior settings, or
	// the prior environment/arguments would be written but never activated. The
	// health check is passed in so the restored configuration is validated against
	// the PRIOR health check, not the new one.
	//
	// preStart, when non-nil, runs at the pre-activation point (after CONFIGURE,
	// before START). Only the forward path supplies it, and only to run a CHANGED
	// post_install hook — the restore path passes nil so the PRIOR hook is never
	// re-executed while rolling back.
	activate := func(rc pattern.ReleaseCtx, hc *spec.HealthCheck, preStart func() error) error {
		// STOP and CONFIGURE return the pattern error UNWRAPPED — exactly as the
		// Deploy path (switchOn) and the START/HEALTH steps below already do.
		// pat.Stop / pat.Configure yield a correctly-coded *pattern.StepError:
		// ERR_SERVICE_STOP / ERR_SERVICE_INSTALL for a remote exit-code failure, but
		// ERR_CONNECT when the transport drops mid-step. Re-coding them here with a
		// blanket coded(...) would override that ERR_CONNECT — diverging from the
		// DESIGN §12 taxonomy (a stable contract for pipelines & tests) and from the
		// Deploy path, where the identical failure keeps its original code — and would
		// double the "[ERR_SERVICE_STOP] [ERR_SERVICE_STOP]" prefix on a genuine
		// service failure.
		if err := sl.timed(ctx, "STOP", func() error { return pat.Stop(ctx, t, rc) }); err != nil {
			return err
		}
		if err := sl.timed(ctx, "CONFIGURE", func() error { return pat.Configure(ctx, t, rc) }); err != nil {
			return err
		}
		if preStart != nil {
			if err := preStart(); err != nil {
				return err
			}
		}
		if err := sl.timed(ctx, "START", func() error { return pat.Start(ctx, t, rc) }); err != nil {
			return err
		}
		return sl.timed(ctx, "HEALTH", func() error {
			return RunHealthCheck(ctx, t, hc, rp.Current, rc.Env)
		})
	}
	// forward applies the NEW configuration AND finalizes it. FINALIZE and the
	// post-finalize Status read are part of the transaction: if either fails the
	// machine must not be left on the new configuration while Terraform retains the
	// prior state, so their failure triggers the same rollback as a health failure.
	var (
		okManifest *Manifest
		okStatus   string
	)
	// node is non-nil for node_web_app; depsBackedUp tracks whether we snapshotted
	// node_modules before `npm ci` so the rollback path can restore it.
	node, _ := pat.(*pattern.NodeWebApp)
	depsBackedUp := false
	forward := func() error {
		// Run post_install ONLY when the hook actually CHANGED for this same-artifact
		// reconfigure, at the pre-activation point (after CONFIGURE, before START) so
		// it runs before the new configuration is activated — matching its
		// after-extract/before-switchover semantics on deploy. An UNCHANGED hook is
		// not re-executed (its effect already landed when the artifact was staged),
		// and because only the forward path supplies preStart the PRIOR hook is never
		// re-run while rolling back.
		// preStart runs at the pre-activation point (after STOP+CONFIGURE, before
		// START) so the service is stopped (no locked node_modules) yet started with
		// the new dependencies/hook in place. Two things happen here, in deploy order:
		//   1. node_web_app InstallDeps (`npm ci --omit=dev`) — self-gates on
		//      install_deps, so a same-artifact install_deps=false→true toggle actually
		//      installs deps (and validates package-lock.json) instead of no-op'ing.
		//   2. a CHANGED post_install hook — an UNCHANGED hook is not re-executed (its
		//      effect already landed at stage time). Only the forward path supplies
		//      preStart, so neither re-runs while rolling back.
		_, isNode := pat.(*pattern.NodeWebApp)
		postInstallChanged := s.Pattern.PostInstall != prior.Pattern.PostInstall
		var preStart func() error
		if isNode || postInstallChanged {
			preStart = func() error {
				if node != nil {
					// Snapshot node_modules BEFORE npm ci so a partial install can be
					// rolled back to a runnable tree (self-gates on install_deps).
					if s.Pattern.InstallDeps {
						if berr := sl.timed(ctx, "STAGE", func() error { return node.BackupDeps(ctx, t, newRC) }); berr != nil {
							return berr
						}
						depsBackedUp = true
					}
					if derr := sl.timed(ctx, "STAGE", func() error { return node.InstallDeps(ctx, t, newRC) }); derr != nil {
						return derr
					}
				}
				if postInstallChanged {
					return e.runPostInstall(ctx, t, s, rp.Release, newRC, host)
				}
				return nil
			}
		}
		if err := activate(newRC, &s.HealthCheck, preStart); err != nil {
			return err
		}
		nm := writeManifest("success")
		if err := sl.timed(ctx, "FINALIZE", func() error { return WriteManifest(ctx, t, rp, nm) }); err != nil {
			return coded("ERR_CONNECT", host, "FINALIZE", err)
		}
		st, serr := pat.Status(ctx, t, newRC)
		if serr != nil {
			return wrapTransportErr(serr, host, "READ")
		}
		okManifest, okStatus = nm, st
		return nil
	}
	if ferr := forward(); ferr != nil {
		// Rollback (DESIGN §10.2 restore): restore the PRIOR configuration — the
		// settings Terraform still holds in state — validated against the PRIOR
		// health check, so the machine is not left stopped or running rejected
		// settings.
		priorRC := releaseCtxVersion(prior, rp, m.CurrentVersion)
		priorRC.EmitStep = sl.emit
		rbStart := time.Now()
		// A partial `npm ci` may have left the ACTIVE release without a runnable
		// dependency tree — restore the node_modules snapshot BEFORE restoring the
		// prior service configuration, so the prior app comes up against its
		// original dependencies. If the snapshot restore ITSELF fails, the prior
		// dependency tree is UNRECOVERABLE: do NOT attempt to start the prior
		// configuration (it would come up against a broken/absent node_modules and
		// could still report healthy under health_check=none) — escalate straight
		// to ERR_ROLLBACK_FAILED so the operator is forced to repair.
		var rerr error
		if depsBackedUp {
			rerr = node.RestoreDeps(ctx, t, newRC)
		}
		if rerr == nil {
			rerr = activate(priorRC, &prior.HealthCheck, nil)
		}
		if rerr == nil {
			// Prior configuration restored and healthy — re-finalize the manifest to
			// record the rolled-back reconfigure (version/checksum unchanged).
			rbm := writeManifest("rolled_back")
			rerr = sl.timed(ctx, "FINALIZE", func() error { return WriteManifest(ctx, t, rp, rbm) })
		}
		sl.emit(ctx, "ROLLBACK", rbStart)
		if rerr != nil {
			// DESIGN §10.6: restoration (or its manifest write) failed — persist the
			// required deploy/failed marker and SURFACE a persistence failure so the
			// operator is forced to repair, rather than only warning. finalizeFailed
			// writes last_operation.type=deploy, result=failed.
			detail := fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s: reconfigure failed and restore of prior configuration failed; reconfigure error: %v; restore error: %v",
				host, ferr, rerr)
			if fmErr := e.finalizeFailed(ctx, sl, t, s, rp, m.CurrentVersion, started, "failed"); fmErr != nil {
				detail = fmt.Errorf("%v; failed-marker persistence ALSO failed (manual repair required): %v", detail, fmErr)
			}
			return nil, coded("ERR_ROLLBACK_FAILED", host, "ROLLBACK", detail)
		}
		// Surface the original error so the apply fails and Terraform retains the
		// (now accurate) prior state.
		return nil, fmt.Errorf("%w; prior configuration restored (healthy)", ferr)
	}
	// Reconfigure succeeded end-to-end: discard the node_modules snapshot (the new
	// dependency tree is live and healthy). Cleanup is best-effort — a leftover
	// backup dir wastes disk but does not affect correctness.
	if depsBackedUp {
		if cerr := node.CommitDeps(ctx, t, newRC); cerr != nil {
			e.warnf("node_modules backup cleanup failed on %s: %v", host, cerr)
		}
	}
	out := statusFrom(okManifest, host, okStatus)
	out.DeployedVersion = deployedVer
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
	// Uninstall calls Stop internally, which may escalate to FORCE_KILL; wire the
	// step sink so that escalation emits a structured FORCE_KILL record on the
	// destroy path too (DESIGN §9.2 S2 / WSV-07), not only on deploy/rollback.
	rc.EmitStep = newStepLogger(s, host).emit
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
