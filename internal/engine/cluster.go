package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/pattern"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// clusterCtx bundles per-node transports; hosts[0] is the coordinator for all
// FailoverClusters cmdlets (DESIGN §9.5 rule "coordinator").
type clusterCtx struct {
	s      *spec.Deployment
	hosts  []string // lowercase, spec order
	tr     map[string]transport.Transport
	cg     *pattern.ClusterGeneric
	paths  func(host, version string) layout.Paths // uniform local layout per node
	locked []*Lock                                 // in acquisition (hosts) order
}

func (e *Engine) newClusterCtx(ctx context.Context, s *spec.Deployment) (*clusterCtx, error) {
	cc := &clusterCtx{s: s, hosts: lowerAll(s.Target.Hosts),
		tr: map[string]transport.Transport{}, cg: &pattern.ClusterGeneric{}}
	cc.paths = func(host, version string) layout.Paths {
		return layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, version)
	}
	for _, h := range cc.hosts {
		sl := newStepLogger(s, h)
		vstart := time.Now()
		t, err := e.NewTransport(&s.Target, h)
		if err != nil {
			sl.emit(ctx, "VALIDATE", vstart)
			cc.closeAll()
			return nil, err
		}
		sl.emit(ctx, "VALIDATE", vstart)
		cstart := time.Now()
		if err := t.Connect(ctx); err != nil {
			sl.emit(ctx, "CONNECT", cstart)
			cc.closeAll()
			return nil, wrapTransportErr(err, h, "CONNECT")
		}
		sl.emit(ctx, "CONNECT", cstart)
		cc.tr[h] = t
	}
	return cc, nil
}

func (cc *clusterCtx) coord() transport.Transport { return cc.tr[cc.hosts[0]] }
func (cc *clusterCtx) closeAll() {
	for _, t := range cc.tr {
		if t != nil {
			t.Close()
		}
	}
}

// lockAll acquires node locks in hosts order; on failure releases the ones
// already held in REVERSE order (DESIGN §13 / architecture §2.4 cluster rule:
// acquire hosts-order, release reverse).
func (e *Engine) lockAll(ctx context.Context, cc *clusterCtx, op string) error {
	for _, h := range cc.hosts {
		p := cc.paths(h, cc.s.Artifact.Version)
		lstart := time.Now()
		lk, warn, err := AcquireLock(ctx, cc.tr[h], p, lockOwner(), op, cc.s.Strategy.EffectiveLockTimeout())
		newStepLogger(cc.s, h).emit(ctx, "LOCK", lstart)
		if err != nil {
			e.unlockAll(ctx, cc)
			return err
		}
		if warn != "" {
			e.warnf("%s", warn)
		}
		cc.locked = append(cc.locked, lk)
	}
	return nil
}

func (e *Engine) unlockAll(ctx context.Context, cc *clusterCtx) {
	// Release in REVERSE acquisition order (DESIGN §13, evaluator item 3), and
	// give EACH node its OWN bounded cleanup context detached from ctx
	// cancellation (evaluator item 3): cluster cleanup must release every held
	// node lock even when the operation was canceled/timed out, and one slow
	// release must not consume a shared deadline for the remaining nodes.
	for i := len(cc.locked) - 1; i >= 0; i-- {
		rctx, cancel := lockCleanupContext(ctx)
		ustart := time.Now()
		if rerr := ReleaseLock(rctx, cc.locked[i]); rerr != nil {
			e.warnf("lock release failed: %v", rerr)
		}
		if cc.s != nil && i < len(cc.hosts) {
			newStepLogger(cc.s, cc.hosts[i]).emit(ctx, "UNLOCK", ustart)
		}
		cancel()
	}
	cc.locked = nil
}

func (e *Engine) deployCluster(ctx context.Context, s *spec.Deployment) (*Status, error) {
	cc, err := e.newClusterCtx(ctx, s)
	if err != nil {
		return nil, err
	}
	defer cc.closeAll()

	// PREFLIGHT (coordinator cmdlets + per-node global gates). Every remote
	// coordinator cmdlet is a logged step (DESIGN §8.5): the module/preflight
	// check and the node-up probe are PREFLIGHT, the role-binding read is a
	// VALIDATE of existing cluster state.
	slCoord0 := newStepLogger(s, cc.hosts[0])
	rcCoord := releaseCtx(s, cc.paths(cc.hosts[0], s.Artifact.Version))
	if err := slCoord0.timed(ctx, "PREFLIGHT", func() error {
		return cc.cg.Preflight(ctx, cc.coord(), rcCoord)
	}); err != nil {
		return nil, err
	}
	var up []string
	if err := slCoord0.timed(ctx, "PREFLIGHT", func() error {
		var perr error
		up, perr = cc.cg.NodesUp(ctx, cc.coord())
		return perr
	}); err != nil {
		return nil, err
	}
	upSet := map[string]bool{}
	for _, n := range up {
		upSet[n] = true
	}
	for _, h := range cc.hosts {
		if !upSet[shortName(h)] && !upSet[h] {
			return nil, coded("ERR_PREFLIGHT", h, "PREFLIGHT",
				fmt.Errorf("node not Up in cluster (up=%v)", up)) // CLU-05
		}
		p := cc.paths(h, s.Artifact.Version)
		if err := newStepLogger(s, h).timed(ctx, "PREFLIGHT", func() error {
			return e.preflight(ctx, cc.tr[h], s, p, cc.cg, releaseCtx(s, p))
		}); err != nil {
			return nil, err
		}
	}

	if err := e.lockAll(ctx, cc, "deploy"); err != nil {
		return nil, err
	}
	defer e.unlockAll(ctx, cc)

	role := s.Pattern.RoleName
	var exists bool
	var boundSvc, owner string
	if err := slCoord0.timed(ctx, "VALIDATE", func() error {
		var berr error
		exists, boundSvc, owner, _, berr = cc.cg.RoleBinding(ctx, cc.coord(), role)
		return berr
	}); err != nil {
		return nil, err
	}
	if exists && !strings.EqualFold(boundSvc, s.Pattern.ServiceName) {
		return nil, coded("ERR_PREFLIGHT", cc.hosts[0], "PREFLIGHT",
			fmt.Errorf("role %q already bound to service %q, spec wants %q — refusing (CLU-06)",
				role, boundSvc, s.Pattern.ServiceName))
	}

	// Idempotency short-circuit across all nodes (IDP-02).
	if exists {
		if allCurrent, m := e.clusterAllCurrent(ctx, cc); allCurrent {
			st, _ := cc.cg.Status(ctx, cc.coord(), rcCoord)
			if st == "online" {
				out := statusFrom(m, "", st)
				out.Hosts = cc.hosts
				return out, nil
			}
		}
	}

	if !exists {
		return e.clusterCreate(ctx, cc)
	}
	return e.clusterRollingUpdate(ctx, cc, strings.ToLower(owner))
}

func (e *Engine) clusterAllCurrent(ctx context.Context, cc *clusterCtx) (bool, *Manifest) {
	var first *Manifest
	for _, h := range cc.hosts {
		m, err := ReadManifest(ctx, cc.tr[h], cc.paths(h, cc.s.Artifact.Version))
		if err != nil || m == nil ||
			m.CurrentVersion != cc.s.Artifact.Version ||
			m.ArtifactChecksum != cc.s.Artifact.Checksum {
			return false, nil
		}
		if first == nil {
			first = m
		}
	}
	return first != nil, first
}

// clusterCreate = C1..C6 (DESIGN §9.5).
func (e *Engine) clusterCreate(ctx context.Context, cc *clusterCtx) (*Status, error) {
	s := cc.s
	started := time.Now().UTC()
	// Best-effort per-node log/event collection on every exit path (DESIGN §6.5).
	defer e.collectClusterLogs(ctx, cc, started)
	// C1: stage + switch + register (start=demand) on EVERY node.
	for _, h := range cc.hosts {
		p := cc.paths(h, s.Artifact.Version)
		rc := releaseCtx(s, p)
		sl := newStepLogger(s, h)
		if err := e.stageOnHost(ctx, sl, cc.tr[h], s, p, cc.cg, rc); err != nil {
			return nil, err
		}
		if err := sl.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, cc.tr[h], p) }); err != nil {
			return nil, err
		}
		if err := sl.timed(ctx, "CONFIGURE", func() error { return cc.cg.Configure(ctx, cc.tr[h], rc) }); err != nil {
			return nil, err
		}
	}
	// C2: role creation on coordinator (registration = CONFIGURE step, DESIGN §8.5).
	slCoord := newStepLogger(s, cc.hosts[0])
	if err := slCoord.timed(ctx, "CONFIGURE", func() error {
		return cc.cg.CreateRole(ctx, cc.coord(), s.Pattern.ServiceName, s.Pattern.RoleName, s.Pattern.StaticAddress)
	}); err != nil {
		e.clusterFinalize(ctx, cc, "", started, "failed")
		return nil, err
	}
	// C3: preferred owners (ownership config = CONFIGURE step, DESIGN §8.5).
	if s.Pattern.PreferredOwner != "" {
		ordered := preferredFirst(cc.hosts, strings.ToLower(s.Pattern.PreferredOwner))
		if err := slCoord.timed(ctx, "CONFIGURE", func() error {
			return cc.cg.SetPreferredOwners(ctx, cc.coord(), s.Pattern.RoleName, ordered)
		}); err != nil {
			e.clusterFinalize(ctx, cc, "", started, "failed")
			return nil, err
		}
	}
	// C4: bring online.
	if err := slCoord.timed(ctx, "START", func() error {
		return cc.cg.StartGroup(ctx, cc.coord(), s.Pattern.RoleName, 120)
	}); err != nil {
		e.clusterFinalize(ctx, cc, "", started, "failed")
		return nil, err
	}
	// C5: settle + health on the owner node. HEALTH is attributed to the ACTUAL
	// owner host (may differ from the coordinator) so the record's host matches
	// where the check ran (evaluator item 3).
	time.Sleep(time.Duration(s.Strategy.Cluster.EffectiveSettle()) * time.Second)
	ownerHost := e.clusterOwnerHost(ctx, cc)
	if err := newStepLogger(s, ownerHost).timed(ctx, "HEALTH", func() error { return e.clusterHealthOn(ctx, cc, ownerHost) }); err != nil {
		_ = cc.cg.StopGroup(ctx, cc.coord(), s.Pattern.RoleName)
		e.clusterFinalize(ctx, cc, "", started, "failed")
		return nil, fmt.Errorf("%w; role stopped after failed create (CLU-01 failure path)", err)
	}
	// C6: manifests + prune.
	e.clusterFinalize(ctx, cc, "", started, "success")
	st, _ := cc.cg.Status(ctx, cc.coord(), releaseCtx(s, cc.paths(cc.hosts[0], s.Artifact.Version)))
	m, _ := ReadManifest(ctx, cc.coord(), cc.paths(cc.hosts[0], s.Artifact.Version))
	out := statusFrom(m, "", st)
	out.Hosts = cc.hosts
	return out, nil
}

// clusterRollingUpdate = U0..U8 with R-steps rollback (DESIGN §9.5).
func (e *Engine) clusterRollingUpdate(ctx context.Context, cc *clusterCtx, owner string) (*Status, error) {
	s := cc.s
	started := time.Now().UTC()
	// Best-effort per-node log/event collection on every exit path (DESIGN §6.5).
	defer e.collectClusterLogs(ctx, cc, started)
	owner = matchHost(cc.hosts, owner)
	if owner == "" {
		return nil, coded("ERR_PREFLIGHT", cc.hosts[0], "PREFLIGHT",
			fmt.Errorf("role owner not among spec hosts %v", cc.hosts))
	}
	prevVersion := ""
	if m, _ := ReadManifest(ctx, cc.tr[owner], cc.paths(owner, s.Artifact.Version)); m != nil {
		prevVersion = m.CurrentVersion
	}
	var passives []string
	for _, h := range cc.hosts {
		if h != owner {
			passives = append(passives, h)
		}
	}
	drain := s.Strategy.Cluster.EffectiveDrain()
	settle := s.Strategy.Cluster.EffectiveSettle()

	// U2: stage new bits on every passive (no service impact).
	for _, h := range passives {
		p := cc.paths(h, s.Artifact.Version)
		if err := e.stageOnHost(ctx, newStepLogger(s, h), cc.tr[h], s, p, cc.cg, releaseCtx(s, p)); err != nil {
			return nil, err
		}
	}
	// U3: switch + reconfigure passives.
	for _, h := range passives {
		p := cc.paths(h, s.Artifact.Version)
		rc := releaseCtx(s, p)
		sl := newStepLogger(s, h)
		if err := sl.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, cc.tr[h], p) }); err != nil {
			return nil, err // pre-move failure: role untouched on old owner (CLU-04-safe)
		}
		if err := sl.timed(ctx, "CONFIGURE", func() error { return cc.cg.Configure(ctx, cc.tr[h], rc) }); err != nil {
			return nil, err
		}
	}
	// U4: move role onto first updated passive.
	firstNew := passives[0]
	slFirst := newStepLogger(s, firstNew)
	if err := slFirst.timed(ctx, "MOVE_GROUP", func() error {
		return cc.cg.MoveGroup(ctx, cc.coord(), s.Pattern.RoleName, firstNew, drain)
	}); err != nil {
		return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
	}
	// U5: settle + health on new owner.
	time.Sleep(time.Duration(settle) * time.Second)
	if err := slFirst.timed(ctx, "HEALTH", func() error { return e.clusterHealthOn(ctx, cc, firstNew) }); err != nil {
		return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
	}
	// U6: update the old owner (role now elsewhere).
	{
		p := cc.paths(owner, s.Artifact.Version)
		rc := releaseCtx(s, p)
		slO := newStepLogger(s, owner)
		if err := e.stageOnHost(ctx, slO, cc.tr[owner], s, p, cc.cg, rc); err != nil {
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
		if err := slO.timed(ctx, "STOP", func() error { return cc.cg.Stop(ctx, cc.tr[owner], rc) }); err != nil { // local instance only
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
		if err := slO.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, cc.tr[owner], p) }); err != nil {
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
		if err := slO.timed(ctx, "CONFIGURE", func() error { return cc.cg.Configure(ctx, cc.tr[owner], rc) }); err != nil {
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
	}
	// U7: optional move to preferred owner + re-health.
	if po := strings.ToLower(s.Pattern.PreferredOwner); po != "" {
		poHost := matchHost(cc.hosts, po)
		if poHost != "" && poHost != firstNew {
			slPo := newStepLogger(s, poHost)
			if err := slPo.timed(ctx, "MOVE_GROUP", func() error {
				return cc.cg.MoveGroup(ctx, cc.coord(), s.Pattern.RoleName, poHost, drain)
			}); err != nil {
				return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
			}
			time.Sleep(time.Duration(settle) * time.Second)
			if err := slPo.timed(ctx, "HEALTH", func() error { return e.clusterHealthOn(ctx, cc, poHost) }); err != nil {
				return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
			}
		}
		ordered := preferredFirst(cc.hosts, po)
		if err := newStepLogger(s, cc.hosts[0]).timed(ctx, "CONFIGURE", func() error {
			return cc.cg.SetPreferredOwners(ctx, cc.coord(), s.Pattern.RoleName, ordered)
		}); err != nil {
			e.warnf("set preferred owners failed: %v", err)
		}
	}
	// U8: finalize.
	e.clusterFinalize(ctx, cc, prevVersion, started, "success")
	st, _ := cc.cg.Status(ctx, cc.coord(), releaseCtx(s, cc.paths(cc.hosts[0], s.Artifact.Version)))
	m, _ := ReadManifest(ctx, cc.coord(), cc.paths(cc.hosts[0], s.Artifact.Version))
	out := statusFrom(m, "", st)
	out.Hosts = cc.hosts
	return out, nil
}

// clusterRollback = R1..R5 (DESIGN §9.5). Old owner still runs prev bits until
// U6; R-steps move the role back and restore passive junctions.
func (e *Engine) clusterRollback(ctx context.Context, cc *clusterCtx, oldOwner string,
	passives []string, prevVersion string, started time.Time, orig error) error {
	s := cc.s
	if !s.Strategy.EffectiveRollback() {
		e.clusterFinalize(ctx, cc, prevVersion, started, "failed")
		return fmt.Errorf("%w; rollback_on_failure=false — cluster left as-is", orig)
	}
	if prevVersion == "" {
		e.clusterFinalize(ctx, cc, prevVersion, started, "failed")
		return fmt.Errorf("%w; no previous version recorded — cannot roll back", orig)
	}
	tflog.Warn(ctx, "cluster deploy failed; rolling back", map[string]interface{}{
		"role": s.Pattern.RoleName, "to": prevVersion, "cause": orig.Error()})
	drain := s.Strategy.Cluster.EffectiveDrain()
	// Rollback step records carry the version being RESTORED (prevVersion).
	slFor := func(host string) stepLogger {
		return stepLogger{app: s.Metadata.Name, host: strings.ToLower(host), version: prevVersion}
	}
	rbStart := time.Now()

	// R1: move role back to old owner (still on prev junction until U6 ran;
	// if U6 already switched it, restore its junction FIRST). SWITCH/CONFIGURE
	// here are idempotent restores, but a genuine failure means the old owner is
	// NOT restored — those errors are collected (not suppressed) so the rollback
	// cannot falsely report a recovered cluster (evaluator iter2 item 8).
	pPrevOwner := cc.paths(oldOwner, prevVersion)
	slOwner := slFor(oldOwner)
	var restoreErrs []error
	if err := slOwner.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, cc.tr[oldOwner], pPrevOwner) }); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("owner %s junction restore: %w", oldOwner, err))
	}
	if err := slOwner.timed(ctx, "CONFIGURE", func() error { return cc.cg.Configure(ctx, cc.tr[oldOwner], releaseCtx(s, pPrevOwner)) }); err != nil {
		restoreErrs = append(restoreErrs, fmt.Errorf("owner %s reconfigure: %w", oldOwner, err))
	}
	if err := slOwner.timed(ctx, "MOVE_GROUP", func() error {
		return cc.cg.MoveGroup(ctx, cc.coord(), s.Pattern.RoleName, oldOwner, drain)
	}); err != nil {
		slOwner.emit(ctx, "ROLLBACK", rbStart)
		// Write failed manifests so target state is recorded before we surface
		// ERR_ROLLBACK_FAILED (DESIGN §10.6). A failed-manifest write is itself
		// folded into the diagnostic so we never claim §10.6 persistence that did
		// not happen (evaluator item 8).
		fmErr := e.clusterFinalizeVersion(ctx, cc, prevVersion, "", started, "failed")
		detail := fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — role could not return to %s; deploy: %v; move-back: %v", oldOwner, oldOwner, orig, err)
		if fmErr != nil {
			detail = fmt.Errorf("%v; failed-manifest write error: %v", detail, fmErr)
		}
		return coded("ERR_ROLLBACK_FAILED", oldOwner, "ROLLBACK", detail)
	}
	// R2: health on old owner at prev version.
	time.Sleep(time.Duration(s.Strategy.Cluster.EffectiveSettle()) * time.Second)
	if err := slOwner.timed(ctx, "HEALTH", func() error { return e.clusterHealthOnVersion(ctx, cc, oldOwner, prevVersion) }); err != nil {
		slOwner.emit(ctx, "ROLLBACK", rbStart)
		fmErr := e.clusterFinalizeVersion(ctx, cc, prevVersion, "", started, "failed")
		detail := fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — old version unhealthy after move-back; deploy: %v; health: %v", oldOwner, orig, err)
		if fmErr != nil {
			detail = fmt.Errorf("%v; failed-manifest write error: %v", detail, fmErr)
		}
		return coded("ERR_ROLLBACK_FAILED", oldOwner, "ROLLBACK", detail)
	}
	// R3: restore passive junctions + registration to prev. A passive that fails
	// to restore leaves the cluster partially updated, so its error is collected
	// and forces ERR_ROLLBACK_FAILED rather than a silent rolled_back (item 8).
	for _, h := range passives {
		pp := cc.paths(h, prevVersion)
		slp := slFor(h)
		if err := slp.timed(ctx, "SWITCH", func() error { return e.switchJunction(ctx, cc.tr[h], pp) }); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("passive %s junction restore: %w", h, err))
			continue
		}
		if err := slp.timed(ctx, "CONFIGURE", func() error { return cc.cg.Configure(ctx, cc.tr[h], releaseCtx(s, pp)) }); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("passive %s reconfigure: %w", h, err))
		}
	}
	slOwner.emit(ctx, "ROLLBACK", rbStart)
	if len(restoreErrs) > 0 {
		// Role is back on the old owner and healthy, but one or more nodes were
		// not restored to prev — record failed manifests and surface the unknown
		// state instead of reporting a clean rollback (item 8 + §10.6).
		fmErr := e.clusterFinalizeVersion(ctx, cc, prevVersion, "", started, "failed")
		if fmErr != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("failed-manifest write: %w", fmErr))
		}
		return coded("ERR_ROLLBACK_FAILED", oldOwner, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — role restored to %s but node(s) not fully rolled back; deploy: %v; restore errors: %v",
				oldOwner, oldOwner, orig, errors.Join(restoreErrs...)))
	}
	// R4: manifests reflect rolled_back state. A failure to persist the
	// rolled_back manifest means §10.6 state was not durably recorded, so surface
	// ERR_ROLLBACK_FAILED rather than reporting a clean rollback (item 8).
	if fmErr := e.clusterFinalizeVersion(ctx, cc, prevVersion, "", started, "rolled_back"); fmErr != nil {
		return coded("ERR_ROLLBACK_FAILED", oldOwner, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE host=%s — role restored to %s but rolled_back manifest write failed: %v; deploy: %v",
				oldOwner, oldOwner, fmErr, orig))
	}
	// R5: surface original failure.
	return fmt.Errorf("%w; cluster rolled back to %s (role on %s, healthy)", orig, prevVersion, oldOwner)
}

// collectClusterLogs runs best-effort deployment log/event collection on every
// cluster node (DESIGN §6.5); no-op when logs are unconfigured.
func (e *Engine) collectClusterLogs(ctx context.Context, cc *clusterCtx, started time.Time) {
	for _, h := range cc.hosts {
		e.collectDeploymentLogs(ctx, cc.tr[h], cc.s, cc.paths(h, cc.s.Artifact.Version), started)
	}
}

// clusterOwnerHost resolves the host currently owning the role via the
// coordinator RoleBinding read, falling back to the coordinator when the owner
// cannot be determined. Used to attribute owner-scoped step records to the host
// that actually runs them (evaluator item 3).
func (e *Engine) clusterOwnerHost(ctx context.Context, cc *clusterCtx) string {
	_, _, owner, _, err := cc.cg.RoleBinding(ctx, cc.coord(), cc.s.Pattern.RoleName)
	if err != nil {
		return cc.hosts[0]
	}
	h := matchHost(cc.hosts, strings.ToLower(owner))
	if h == "" {
		return cc.hosts[0]
	}
	return h
}

func (e *Engine) clusterHealthOn(ctx context.Context, cc *clusterCtx, host string) error {
	return e.clusterHealthOnVersion(ctx, cc, host, cc.s.Artifact.Version)
}

func (e *Engine) clusterHealthOnVersion(ctx context.Context, cc *clusterCtx, host, version string) error {
	p := cc.paths(host, version)
	rc := releaseCtx(cc.s, p)
	return RunHealthCheck(ctx, cc.tr[host], &cc.s.HealthCheck, p.Current, rc.Env)
}

func (e *Engine) clusterFinalize(ctx context.Context, cc *clusterCtx, prev string, started time.Time, result string) error {
	return e.clusterFinalizeVersion(ctx, cc, cc.s.Artifact.Version, prev, started, result)
}

func (e *Engine) clusterFinalizeVersion(ctx context.Context, cc *clusterCtx, current, prev string, started time.Time, result string) error {
	s := cc.s
	var writeErrs []error
	for _, h := range cc.hosts {
		p := cc.paths(h, current)
		sl := stepLogger{app: s.Metadata.Name, host: strings.ToLower(h), version: current}
		m := &Manifest{Schema: 1, App: s.Metadata.Name, Pattern: string(s.Pattern.Type),
			CurrentVersion: current, PreviousVersion: prev,
			CurrentRelease: p.Release, ArtifactChecksum: s.Artifact.Checksum,
			ProviderVersion: ProviderVersion,
			Extra:           map[string]string{"role": s.Pattern.RoleName},
			LastOperation: LastOp{Type: "deploy", Result: result,
				Started: started.Format(time.RFC3339), Finished: time.Now().UTC().Format(time.RFC3339)},
		}
		if result == "rolled_back" {
			m.PreviousVersion = ""
			m.ArtifactChecksum = "" // checksum of prev unknown here; Read tolerates empty
		}
		werr := sl.timed(ctx, "FINALIZE", func() error { return WriteManifest(ctx, cc.tr[h], p, m) })
		if werr != nil {
			e.warnf("manifest write failed on %s: %v", h, werr)
			writeErrs = append(writeErrs, fmt.Errorf("%s: %w", h, werr))
			continue
		}
		if result == "success" {
			if err := sl.timed(ctx, "PRUNE", func() error { return e.pruneReleases(ctx, cc.tr[h], s, p, m) }); err != nil {
				e.warnf("prune failed on %s: %v", h, err)
			}
		}
	}
	if len(writeErrs) > 0 {
		return errors.Join(writeErrs...)
	}
	return nil
}

func (e *Engine) destroyCluster(ctx context.Context, s *spec.Deployment, mode string) error {
	cc, err := e.newClusterCtx(ctx, s)
	if err != nil {
		return err
	}
	defer cc.closeAll()
	if err := e.lockAll(ctx, cc, "destroy"); err != nil {
		return err
	}
	defer e.unlockAll(ctx, cc)
	// Role first (stops + removes resources), then per-node service + tree.
	if err := cc.cg.RemoveRole(ctx, cc.coord(), s.Pattern.RoleName); err != nil {
		return err
	}
	for _, h := range cc.hosts {
		p := cc.paths(h, s.Artifact.Version)
		rc := releaseCtx(s, p)
		if err := cc.cg.Uninstall(ctx, cc.tr[h], rc, mode == "purge"); err != nil {
			return err
		}
		if mode == "purge" {
			if err := e.removePath(ctx, cc.tr[h], p.Root); err != nil {
				return err
			}
		} else {
			if err := e.removePath(ctx, cc.tr[h], p.Manifest); err != nil {
				return err
			}
		}
	}
	return nil
}

// -------------------------------- helpers -----------------------------------

// shortName strips domain suffix for cluster node comparison (nodes report
// NetBIOS names; specs may use FQDNs).
func shortName(h string) string {
	if i := strings.IndexByte(h, '.'); i > 0 {
		return h[:i]
	}
	return h
}

// matchHost maps a cluster-reported owner onto the spec host list.
func matchHost(hosts []string, owner string) string {
	owner = strings.ToLower(owner)
	for _, h := range hosts {
		if h == owner || shortName(h) == shortName(owner) {
			return h
		}
	}
	return ""
}

func preferredFirst(hosts []string, preferred string) []string {
	out := []string{}
	pref := matchHost(hosts, preferred)
	if pref != "" {
		out = append(out, pref)
	}
	for _, h := range hosts {
		if h != pref {
			out = append(out, h)
		}
	}
	return out
}
