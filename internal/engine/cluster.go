package engine

import (
	"context"
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
	locked []string
}

func (e *Engine) newClusterCtx(ctx context.Context, s *spec.Deployment) (*clusterCtx, error) {
	cc := &clusterCtx{s: s, hosts: lowerAll(s.Target.Hosts),
		tr: map[string]transport.Transport{}, cg: &pattern.ClusterGeneric{}}
	cc.paths = func(host, version string) layout.Paths {
		return layout.NewPaths(s.Target.OS, s.Pattern.EffectiveInstallRoot(s.Target.OS), s.Metadata.Name, version)
	}
	for _, h := range cc.hosts {
		t, err := e.NewTransport(&s.Target, h)
		if err != nil {
			cc.closeAll()
			return nil, err
		}
		if err := t.Connect(ctx); err != nil {
			cc.closeAll()
			return nil, wrapTransportErr(err, h, "CONNECT")
		}
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
// already held (DESIGN §13 cluster rule).
func (e *Engine) lockAll(ctx context.Context, cc *clusterCtx, op string) error {
	for _, h := range cc.hosts {
		p := cc.paths(h, cc.s.Artifact.Version)
		warn, err := AcquireLock(ctx, cc.tr[h], p, lockOwner(), op, cc.s.Strategy.EffectiveLockTimeout())
		if err != nil {
			e.unlockAll(ctx, cc)
			return err
		}
		if warn != "" {
			e.warnf("%s", warn)
		}
		cc.locked = append(cc.locked, h)
	}
	return nil
}

func (e *Engine) unlockAll(ctx context.Context, cc *clusterCtx) {
	// Detach from ctx cancellation so cluster cleanup releases every held node
	// lock even when the operation was canceled/timed out (evaluator item 3).
	rctx, cancel := lockCleanupContext(ctx)
	defer cancel()
	for _, h := range cc.locked {
		ReleaseLock(rctx, cc.tr[h], cc.paths(h, cc.s.Artifact.Version))
	}
	cc.locked = nil
}

func (e *Engine) deployCluster(ctx context.Context, s *spec.Deployment) (*Status, error) {
	cc, err := e.newClusterCtx(ctx, s)
	if err != nil {
		return nil, err
	}
	defer cc.closeAll()

	// PREFLIGHT (coordinator cmdlets + per-node global gates).
	rcCoord := releaseCtx(s, cc.paths(cc.hosts[0], s.Artifact.Version))
	if err := cc.cg.Preflight(ctx, cc.coord(), rcCoord); err != nil {
		return nil, err
	}
	up, err := cc.cg.NodesUp(ctx, cc.coord())
	if err != nil {
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
		if err := e.preflight(ctx, cc.tr[h], s, p, cc.cg, releaseCtx(s, p)); err != nil {
			return nil, err
		}
	}

	if err := e.lockAll(ctx, cc, "deploy"); err != nil {
		return nil, err
	}
	defer e.unlockAll(ctx, cc)

	role := s.Pattern.RoleName
	exists, boundSvc, owner, _, err := cc.cg.RoleBinding(ctx, cc.coord(), role)
	if err != nil {
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
	// C1: stage + switch + register (start=demand) on EVERY node.
	for _, h := range cc.hosts {
		p := cc.paths(h, s.Artifact.Version)
		rc := releaseCtx(s, p)
		if err := e.stageOnHost(ctx, cc.tr[h], s, p, cc.cg, rc); err != nil {
			return nil, err
		}
		if err := e.switchJunction(ctx, cc.tr[h], p); err != nil {
			return nil, err
		}
		if err := cc.cg.Configure(ctx, cc.tr[h], rc); err != nil {
			return nil, err
		}
	}
	// C2: role creation on coordinator.
	if err := cc.cg.CreateRole(ctx, cc.coord(), s.Pattern.ServiceName, s.Pattern.RoleName, s.Pattern.StaticAddress); err != nil {
		return nil, err
	}
	// C3: preferred owners.
	if s.Pattern.PreferredOwner != "" {
		ordered := preferredFirst(cc.hosts, strings.ToLower(s.Pattern.PreferredOwner))
		if err := cc.cg.SetPreferredOwners(ctx, cc.coord(), s.Pattern.RoleName, ordered); err != nil {
			return nil, err
		}
	}
	// C4: bring online.
	if err := cc.cg.StartGroup(ctx, cc.coord(), s.Pattern.RoleName, 120); err != nil {
		e.clusterFinalize(ctx, cc, "", started, "failed")
		return nil, err
	}
	// C5: settle + health on the owner node.
	time.Sleep(time.Duration(s.Strategy.Cluster.EffectiveSettle()) * time.Second)
	if err := e.clusterHealthOnOwner(ctx, cc); err != nil {
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
		if err := e.stageOnHost(ctx, cc.tr[h], s, p, cc.cg, releaseCtx(s, p)); err != nil {
			return nil, err
		}
	}
	// U3: switch + reconfigure passives.
	for _, h := range passives {
		p := cc.paths(h, s.Artifact.Version)
		rc := releaseCtx(s, p)
		if err := e.switchJunction(ctx, cc.tr[h], p); err != nil {
			return nil, err // pre-move failure: role untouched on old owner (CLU-04-safe)
		}
		if err := cc.cg.Configure(ctx, cc.tr[h], rc); err != nil {
			return nil, err
		}
	}
	// U4: move role onto first updated passive.
	firstNew := passives[0]
	if err := cc.cg.MoveGroup(ctx, cc.coord(), s.Pattern.RoleName, firstNew, drain); err != nil {
		return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
	}
	// U5: settle + health on new owner.
	time.Sleep(time.Duration(settle) * time.Second)
	if err := e.clusterHealthOn(ctx, cc, firstNew); err != nil {
		return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
	}
	// U6: update the old owner (role now elsewhere).
	{
		p := cc.paths(owner, s.Artifact.Version)
		rc := releaseCtx(s, p)
		if err := e.stageOnHost(ctx, cc.tr[owner], s, p, cc.cg, rc); err != nil {
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
		if err := cc.cg.Stop(ctx, cc.tr[owner], rc); err != nil { // local instance only
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
		if err := e.switchJunction(ctx, cc.tr[owner], p); err != nil {
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
		if err := cc.cg.Configure(ctx, cc.tr[owner], rc); err != nil {
			return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
		}
	}
	// U7: optional move to preferred owner + re-health.
	if po := strings.ToLower(s.Pattern.PreferredOwner); po != "" {
		poHost := matchHost(cc.hosts, po)
		if poHost != "" && poHost != firstNew {
			if err := cc.cg.MoveGroup(ctx, cc.coord(), s.Pattern.RoleName, poHost, drain); err != nil {
				return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
			}
			time.Sleep(time.Duration(settle) * time.Second)
			if err := e.clusterHealthOn(ctx, cc, poHost); err != nil {
				return nil, e.clusterRollback(ctx, cc, owner, passives, prevVersion, started, err)
			}
		}
		ordered := preferredFirst(cc.hosts, po)
		if err := cc.cg.SetPreferredOwners(ctx, cc.coord(), s.Pattern.RoleName, ordered); err != nil {
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

	// R1: move role back to old owner (still on prev junction until U6 ran;
	// if U6 already switched it, restore its junction FIRST).
	pPrevOwner := cc.paths(oldOwner, prevVersion)
	_ = e.switchJunction(ctx, cc.tr[oldOwner], pPrevOwner) // idempotent restore
	_ = cc.cg.Configure(ctx, cc.tr[oldOwner], releaseCtx(s, pPrevOwner))
	if err := cc.cg.MoveGroup(ctx, cc.coord(), s.Pattern.RoleName, oldOwner, drain); err != nil {
		return coded("ERR_ROLLBACK_FAILED", oldOwner, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE — role could not return to %s; deploy: %v; move-back: %v", oldOwner, orig, err))
	}
	// R2: health on old owner at prev version.
	time.Sleep(time.Duration(s.Strategy.Cluster.EffectiveSettle()) * time.Second)
	if err := e.clusterHealthOnVersion(ctx, cc, oldOwner, prevVersion); err != nil {
		return coded("ERR_ROLLBACK_FAILED", oldOwner, "ROLLBACK",
			fmt.Errorf("MACHINE IN UNKNOWN STATE — old version unhealthy after move-back; deploy: %v; health: %v", orig, err))
	}
	// R3: restore passive junctions + registration to prev.
	for _, h := range passives {
		pp := cc.paths(h, prevVersion)
		if err := e.switchJunction(ctx, cc.tr[h], pp); err != nil {
			e.warnf("rollback: junction restore failed on %s: %v", h, err)
			continue
		}
		_ = cc.cg.Configure(ctx, cc.tr[h], releaseCtx(s, pp))
	}
	// R4: manifests reflect rolled_back state.
	e.clusterFinalizeVersion(ctx, cc, prevVersion, "", started, "rolled_back")
	// R5: surface original failure.
	return fmt.Errorf("%w; cluster rolled back to %s (role on %s, healthy)", orig, prevVersion, oldOwner)
}

func (e *Engine) clusterHealthOnOwner(ctx context.Context, cc *clusterCtx) error {
	_, _, owner, _, err := cc.cg.RoleBinding(ctx, cc.coord(), cc.s.Pattern.RoleName)
	if err != nil {
		return err
	}
	h := matchHost(cc.hosts, strings.ToLower(owner))
	if h == "" {
		h = cc.hosts[0]
	}
	return e.clusterHealthOn(ctx, cc, h)
}

func (e *Engine) clusterHealthOn(ctx context.Context, cc *clusterCtx, host string) error {
	return e.clusterHealthOnVersion(ctx, cc, host, cc.s.Artifact.Version)
}

func (e *Engine) clusterHealthOnVersion(ctx context.Context, cc *clusterCtx, host, version string) error {
	p := cc.paths(host, version)
	rc := releaseCtx(cc.s, p)
	return RunHealthCheck(ctx, cc.tr[host], &cc.s.HealthCheck, p.Current, rc.Env)
}

func (e *Engine) clusterFinalize(ctx context.Context, cc *clusterCtx, prev string, started time.Time, result string) {
	e.clusterFinalizeVersion(ctx, cc, cc.s.Artifact.Version, prev, started, result)
}

func (e *Engine) clusterFinalizeVersion(ctx context.Context, cc *clusterCtx, current, prev string, started time.Time, result string) {
	s := cc.s
	for _, h := range cc.hosts {
		p := cc.paths(h, current)
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
		if err := WriteManifest(ctx, cc.tr[h], p, m); err != nil {
			e.warnf("manifest write failed on %s: %v", h, err)
			continue
		}
		if result == "success" {
			if err := e.pruneReleases(ctx, cc.tr[h], s, p, m); err != nil {
				e.warnf("prune failed on %s: %v", h, err)
			}
		}
	}
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
