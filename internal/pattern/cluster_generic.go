package pattern

import (
	"context"
	"fmt"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ClusterGeneric handles per-node service registration for a WSFC Generic
// Service role. The rolling orchestration (U-steps) lives in engine/cluster.go;
// this type supplies node-level verbs plus cluster cmdlet helpers executed on
// the coordinator (hosts[0]) — DESIGN §9.5.
type ClusterGeneric struct{ ws WindowsService }

func (c *ClusterGeneric) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	script := `Import-Module FailoverClusters -ErrorAction SilentlyContinue
if(-not (Get-Module FailoverClusters)){ Write-Error 'FailoverClusters module missing'; exit 1 }
exit 0`
	r, err := runPS(ctx, t, t.Host(), "PREFLIGHT", script, nil, 60)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return stepErr("ERR_PREFLIGHT", t.Host(), "PREFLIGHT", fmt.Errorf("%s", truncOut(r)))
	}
	return nil
}

// Configure registers/updates the node-local service. Cluster owns start:
// start_type forced to `manual` → sc `demand` (DESIGN §9.5 C1).
func (c *ClusterGeneric) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return c.ws.configure(ctx, t, rc, "manual")
}

// Stop stops the LOCAL service instance only (used during node update; the
// cluster keeps the role on another node).
func (c *ClusterGeneric) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return c.ws.Stop(ctx, t, rc)
}

// Start is intentionally a no-op at node scope: the ROLE is started via
// Start-ClusterGroup by the engine, never the local SCM.
func (c *ClusterGeneric) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil
}

func (c *ClusterGeneric) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	// Role state queried on whichever node this is (cmdlets are cluster-wide).
	role := rc.Spec.Pattern.RoleName
	script := fmt.Sprintf(`Import-Module FailoverClusters -ErrorAction SilentlyContinue
$g = Get-ClusterGroup -Name %s -ErrorAction SilentlyContinue
if($null -eq $g){ Write-Output 'not_installed'; exit 0 }
Write-Output $g.State.ToString().ToLower()
exit 0`, psq(role))
	r, err := runPS(ctx, t, t.Host(), "STATUS", script, nil, 60)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.ToLower(r.Stdout)), nil
}

func (c *ClusterGeneric) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	// Node-scope: delete the local service registration. Role removal is a
	// coordinator action (RemoveRole) driven by engine before node loop.
	return c.ws.Uninstall(ctx, t, rc, purge)
}

// ------------------- coordinator (cluster-wide) helpers ---------------------

// RoleBinding returns (exists, boundServiceName, ownerNode, state).
func (c *ClusterGeneric) RoleBinding(ctx context.Context, t transport.Transport, role string) (bool, string, string, string, error) {
	script := fmt.Sprintf(`Import-Module FailoverClusters
$g = Get-ClusterGroup -Name %s -ErrorAction SilentlyContinue
if($null -eq $g){ Write-Output 'ABSENT'; exit 0 }
$res = Get-ClusterResource | Where-Object { $_.OwnerGroup.Name -eq $g.Name -and $_.ResourceType -eq 'Generic Service' } | Select-Object -First 1
$svc = ''
if($res){ $svc = ($res | Get-ClusterParameter -Name ServiceName -ErrorAction SilentlyContinue).Value }
Write-Output ('PRESENT|' + $svc + '|' + $g.OwnerNode.Name + '|' + $g.State.ToString())
exit 0`, psq(role))
	r, err := runPS(ctx, t, t.Host(), "PREFLIGHT", script, nil, 120)
	if err != nil {
		return false, "", "", "", err
	}
	if r.ExitCode != 0 {
		return false, "", "", "", stepErr("ERR_PREFLIGHT", t.Host(), "PREFLIGHT", fmt.Errorf("%s", truncOut(r)))
	}
	out := strings.TrimSpace(r.Stdout)
	if out == "ABSENT" {
		return false, "", "", "", nil
	}
	parts := strings.SplitN(strings.TrimPrefix(out, "PRESENT|"), "|", 3)
	if len(parts) != 3 {
		return false, "", "", "", fmt.Errorf("unexpected role probe output: %q", out)
	}
	return true, parts[0], parts[1], parts[2], nil
}

// PreflightRole performs the coordinator role-binding conflict check
// (DESIGN §9.5 preflight): if the WSFC Generic Service role `role` already
// exists bound to a service OTHER than `wantSvc`, it returns an
// ERR_SERVICE_INSTALL-coded error that names BOTH the already-bound service and
// the service the spec wants, and performs NO mutation (the only script it runs
// is the read-only RoleBinding probe). On no conflict it returns
// (exists, ownerNode, nil). This is the in-process seam the "role binding
// conflict" scenario exercises with a scripted fake Transport.
func (c *ClusterGeneric) PreflightRole(ctx context.Context, t transport.Transport, role, wantSvc string) (bool, string, error) {
	exists, boundSvc, owner, _, err := c.RoleBinding(ctx, t, role)
	if err != nil {
		return false, "", err
	}
	if exists && !strings.EqualFold(boundSvc, wantSvc) {
		return false, "", stepErr("ERR_SERVICE_INSTALL", t.Host(), "PREFLIGHT",
			fmt.Errorf("role %q already bound to service %q, spec wants service %q — refusing (CLU-06)",
				role, boundSvc, wantSvc))
	}
	return exists, owner, nil
}

// NodesUp returns cluster nodes in Up state (lower-cased).
func (c *ClusterGeneric) NodesUp(ctx context.Context, t transport.Transport) ([]string, error) {
	script := `Import-Module FailoverClusters
(Get-ClusterNode | Where-Object State -eq 'Up').Name | ForEach-Object { $_.ToLower() }
exit 0`
	r, err := runPS(ctx, t, t.Host(), "PREFLIGHT", script, nil, 120)
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, stepErr("ERR_PREFLIGHT", t.Host(), "PREFLIGHT", fmt.Errorf("%s", truncOut(r)))
	}
	var nodes []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			nodes = append(nodes, l)
		}
	}
	return nodes, nil
}

// CreateRole = DESIGN §9.5 C2.
func (c *ClusterGeneric) CreateRole(ctx context.Context, t transport.Transport, svc, role, staticAddress string) error {
	extra := ""
	if staticAddress != "" {
		extra = " -StaticAddress " + psq(staticAddress)
	}
	script := fmt.Sprintf(`Import-Module FailoverClusters
try { Add-ClusterGenericServiceRole -ServiceName %s -Name %s%s -ErrorAction Stop | Out-Null }
catch { Write-Error $_.Exception.Message; exit %d }
exit 0`, psq(svc), psq(role), extra, ExitSvcInstall)
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 300)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return nil
}

// MoveGroup = Move-ClusterGroup with drain wait + Online/owner verification
// (DESIGN §9.5 U4). Exit 47 ⇒ ERR_CLUSTER_MOVE.
func (c *ClusterGeneric) MoveGroup(ctx context.Context, t transport.Transport, role, node string, drainSec int) error {
	script := fmt.Sprintf(`Import-Module FailoverClusters
try { Move-ClusterGroup -Name %s -Node %s -Wait %d -ErrorAction Stop | Out-Null }
catch { Write-Error $_.Exception.Message; exit %d }
$g = Get-ClusterGroup -Name %s
if($g.OwnerNode.Name.ToLower() -ne %s.ToLower() -or $g.State -ne 'Online'){
  Write-Error ("post-move: owner=" + $g.OwnerNode.Name + " state=" + $g.State); exit %d }
exit 0`, psq(role), psq(node), drainSec, ExitClusterMove, psq(role), psq(node), ExitClusterMove)
	r, err := runPS(ctx, t, t.Host(), "MOVE_GROUP", script, nil, drainSec+120)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "MOVE_GROUP", r)
	}
	return nil
}

// StartGroup = C4 (idempotent when already Online).
func (c *ClusterGeneric) StartGroup(ctx context.Context, t transport.Transport, role string, waitSec int) error {
	script := fmt.Sprintf(`Import-Module FailoverClusters
try { Start-ClusterGroup -Name %s -Wait %d -ErrorAction Stop | Out-Null }
catch { Write-Error $_.Exception.Message; exit %d }
if((Get-ClusterGroup -Name %s).State -ne 'Online'){ exit %d }
exit 0`, psq(role), waitSec, ExitClusterMove, psq(role), ExitClusterMove)
	r, err := runPS(ctx, t, t.Host(), "MOVE_GROUP", script, nil, waitSec+120)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "MOVE_GROUP", r)
	}
	return nil
}

// SetPreferredOwners applies the preferred_owner ordering (C3/U7).
func (c *ClusterGeneric) SetPreferredOwners(ctx context.Context, t transport.Transport, role string, ordered []string) error {
	list := make([]string, len(ordered))
	for i, n := range ordered {
		list[i] = psq(n)
	}
	script := fmt.Sprintf(`Import-Module FailoverClusters
Set-ClusterOwnerNode -Group %s -Owners %s
exit 0`, psq(role), strings.Join(list, ","))
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 120)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return nil
}

// StopGroup takes the role Offline (create-failure path, DESIGN §9.5 C5-fail).
func (c *ClusterGeneric) StopGroup(ctx context.Context, t transport.Transport, role string) error {
	script := fmt.Sprintf(`Import-Module FailoverClusters
Stop-ClusterGroup -Name %s -ErrorAction SilentlyContinue | Out-Null
exit 0`, psq(role))
	_, err := runPS(ctx, t, t.Host(), "MOVE_GROUP", script, nil, 300)
	return err
}

// RemoveRole = destroy path (DESIGN §9.5 DESTROY).
func (c *ClusterGeneric) RemoveRole(ctx context.Context, t transport.Transport, role string) error {
	script := fmt.Sprintf(`Import-Module FailoverClusters
$g = Get-ClusterGroup -Name %s -ErrorAction SilentlyContinue
if($null -eq $g){ exit 0 }
Stop-ClusterGroup -Name %s -ErrorAction SilentlyContinue | Out-Null
try { Remove-ClusterGroup -Name %s -RemoveResources -Force -ErrorAction Stop }
catch { Write-Error $_.Exception.Message; exit %d }
exit 0`, psq(role), psq(role), psq(role), ExitSvcInstall)
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 300)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return nil
}
