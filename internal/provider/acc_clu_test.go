package provider

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// Stage 9.2 — CLU (DESIGN §18.7) failover-cluster acceptance against real WSFC
// labs: C2 (two-node cn1/cn2) and C3 (three-node) Windows Server 2022 clusters,
// WinRM https. Each test maps 1:1 to a DESIGN §18.7 ID (scenario IDs MUST appear
// in the test name — DESIGN §18 line 948). Positive scenarios end by asserting
// `.lock` is absent on EVERY node (the DESIGN §18 cross-cutting invariant, probed
// across all cluster hosts); failure scenarios assert the coded error plus the
// "nothing partially mutated" post-state via CheckDestroy.
//
// Env-gating mirrors the single-target harness: TF_ACC unset ⇒ self-SKIP; TF_ACC=1
// with a missing cluster env ⇒ FAIL (never a silent skip), so a mis-configured lab
// run cannot masquerade as a pass.

// Cluster env var names.
const (
	// Comma/space separated WSFC node lists. cn1 is the coordinator (hosts[0]).
	envC2Nodes = "LABDEPLOY_ACC_C2_NODES"
	envC3Nodes = "LABDEPLOY_ACC_C3_NODES"

	// Shared cluster admin credentials (WinRM https to every node).
	envClusterUser     = "LABDEPLOY_ACC_CLUSTER_USER"
	envClusterPassword = "LABDEPLOY_ACC_CLUSTER_PASSWORD"
	envClusterPort     = "LABDEPLOY_ACC_CLUSTER_PORT"

	// The role's Client Access Point (CAP) health URL — the cluster VIP/network
	// name, NOT localhost. CLU-02's availability probe MUST hit the CAP so it
	// observes service continuity THROUGH the access point as ownership moves.
	envClusterCAPHealthURL = "LABDEPLOY_ACC_CLUSTER_HEALTH_URL"

	// Opt-in fault-injection flags (the operator provisions the fault before the
	// run; under TF_ACC=1 the scenario FAILS if the flag is absent rather than
	// passing on an unproven path).
	envCN2Blocked      = "LABDEPLOY_ACC_C2_CN2_BLOCKED"
	envRolePreConflict = "LABDEPLOY_ACC_CLU_ROLE_PRECONFLICT"
	// CLU-05 baseline: the operator pre-seeds a healthy v1.0.0 deployment on C2
	// (both nodes) BEFORE firewalling cn2, so the failed 1.1.0 update can be shown
	// to leave the prior junction + role owner UNCHANGED (no partial switch).
	envC2Baseline = "LABDEPLOY_ACC_C2_BASELINE"

	clusterRole = "sample-role"
	clusterSvc  = "SampleSvc"
)

// clusterTarget bundles the whole-cluster target (all hosts — used for the spec
// `target:` block and the all-node `.lock` invariant) plus one single-host
// accTarget per node so the existing per-host probes (junction, service, manifest,
// health) can be reused node-by-node.
type clusterTarget struct {
	all   accTarget   // Hosts = every node; used for the spec + all-node lock probe
	nodes []accTarget // one single-host target per node, in hosts order
	names []string    // node names in hosts order
}

// nodeByName resolves the single-host target whose node matches name (comparing
// the short hostname, since Get-ClusterGroup reports the bare node name while the
// spec may carry an FQDN).
func (ct clusterTarget) nodeByName(name string) (accTarget, bool) {
	want := shortHost(name)
	for i, n := range ct.names {
		if shortHost(n) == want {
			return ct.nodes[i], true
		}
	}
	return accTarget{}, false
}

func shortHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if i := strings.IndexByte(h, '.'); i >= 0 {
		return h[:i]
	}
	return h
}

func splitNodes(raw string) []string {
	f := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(f))
	for _, n := range f {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func requireC2(t *testing.T) clusterTarget {
	return requireCluster(t, envC2Nodes, 2, "C2 two-node WSFC")
}

func requireC3(t *testing.T) clusterTarget {
	return requireCluster(t, envC3Nodes, 3, "C3 three-node WSFC")
}

func requireCluster(t *testing.T, nodesEnv string, want int, why string) clusterTarget {
	t.Helper()
	names := splitNodes(mustEnv(t, nodesEnv, why))
	if len(names) < want {
		t.Fatalf("TF_ACC=1 requires %s to list at least %d WSFC nodes for %s (got %d)", nodesEnv, want, why, len(names))
	}
	user := mustEnv(t, envClusterUser, why)
	if os.Getenv(envClusterPassword) == "" {
		t.Fatalf("TF_ACC=1 requires %s (the cluster admin password) for %s", envClusterPassword, why)
	}
	port := optPort(t, envClusterPort, 5986)
	https := true
	insecure := true

	mk := func(hosts []string) accTarget {
		tgt := spec.Target{Transport: spec.TransportWinRM, Hosts: hosts, OS: spec.OSWindows, Port: port}
		tgt.Credentials.Username = user
		tgt.Credentials.PasswordEnv = envClusterPassword
		tgt.WinRM.UseHTTPS = &https
		tgt.WinRM.InsecureSkipVerify = &insecure
		return accTarget{tgt: tgt, host: hosts[0], passName: envClusterPassword}
	}

	all := mk(names)
	all.yaml = clusterTargetYAML(names, port, user)
	nodes := make([]accTarget, len(names))
	for i, n := range names {
		nodes[i] = mk([]string{n})
	}
	return clusterTarget{all: all, nodes: nodes, names: names}
}

// clusterTargetYAML renders the multi-host `target:` block (two-space indented, no
// leading key) the harness embeds in the spec.
func clusterTargetYAML(names []string, port int, user string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return fmt.Sprintf(`  transport: winrm
  hosts: [%s]
  os: windows
  port: %d
  credentials: { username: %q, password_env: %s }
  winrm: { use_https: true, insecure_skip_verify: true }`,
		strings.Join(quoted, ", "), port, user, envClusterPassword)
}

// clusterSpec renders a cluster_generic_service Deployment for `version`. When
// preferredOwner is non-empty the pattern pins the role's preferred owner (CLU-08).
func clusterSpec(t *testing.T, ct clusterTarget, version, ext, preferredOwner string) string {
	t.Helper()
	po := ""
	if preferredOwner != "" {
		po = "\n  preferred_owner: " + preferredOwner
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: %s
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: cluster_generic_service
  service_name: %s
  role_name: %s
  exe: bin\sample-svc.exe%s
health_check:
  type: http
  http: { url: %q }
strategy:
  rollback_on_failure: true
  cluster: { drain_timeout_seconds: 300, health_settle_seconds: 10 }
`, ct.all.yaml, artifactType(ext), version, artifactSHA(t, version, ext), artifactURL(t, version, ext),
		clusterSvc, clusterRole, po, healthURL(8080))
}

// ---------------------------------------------------------------------------
// Cluster observation helpers (Get-ClusterGroup owner/state on the coordinator).
// ---------------------------------------------------------------------------

// clusterGroup probes the coordinator for the role's owner node + state.
func clusterGroup(coord accTarget, role string) (owner, state string, err error) {
	out, e := probeHost(coord,
		fmt.Sprintf("$g = Get-ClusterGroup -Name '%s' -ErrorAction Stop; \"$($g.OwnerNode)|$($g.State)\"", psq(role)),
		"echo unsupported")
	if e != nil {
		return "", "", e
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected Get-ClusterGroup output %q", strings.TrimSpace(out))
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func ownerOf(ct clusterTarget, role string) (string, error) {
	owner, _, err := clusterGroup(ct.nodes[0], role)
	return owner, err
}

// clusterCAPHealthURL returns the role's Client Access Point health URL (the
// cluster VIP/network name). Under TF_ACC=1 a missing value is a FAILURE: CLU-02's
// single-gap proof is meaningless if the probe hits localhost instead of the CAP,
// so we refuse to run it against the wrong endpoint.
func clusterCAPHealthURL(t *testing.T) string {
	t.Helper()
	u := strings.TrimSpace(os.Getenv(envClusterCAPHealthURL))
	if u == "" {
		t.Fatalf("CLU-02 under TF_ACC=1 requires %s (the role's Client Access Point health URL — the cluster VIP/network name, NOT localhost) so availability is observed THROUGH the access point across the failover", envClusterCAPHealthURL)
	}
	return u
}

// checkClusterOwnerUnchanged asserts the role owner still equals the value captured
// before a failed update (CLU-05: an unreachable-node update must not move the
// role). prev must have been captured in PreConfig against the reachable node.
func checkClusterOwnerUnchanged(ct clusterTarget, role string, prev *string, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if strings.TrimSpace(*prev) == "" {
			return fmt.Errorf("%s: baseline owner was never captured", why)
		}
		owner, err := ownerOf(ct, role)
		if err != nil {
			return fmt.Errorf("%s: owner probe: %w", why, err)
		}
		if shortHost(owner) != shortHost(*prev) {
			return fmt.Errorf("%s: role owner moved from %q to %q during the failed update (partial switch)", why, *prev, owner)
		}
		return nil
	}
}

// checkJunctionUnchanged asserts node `at`'s `current` junction still resolves to
// exactly the target captured before a failed update (CLU-05: the prior release
// binding must survive an unreachable-node update byte-for-byte).
func checkJunctionUnchanged(at accTarget, prev *string, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if strings.TrimSpace(*prev) == "" {
			return fmt.Errorf("%s: baseline junction target was never captured", why)
		}
		got, err := readCurrentTarget(at)
		if err != nil {
			return fmt.Errorf("%s: junction probe: %w", why, err)
		}
		if !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(*prev)) {
			return fmt.Errorf("%s: current junction changed from %q to %q during the failed update (partial switch)", why, strings.TrimSpace(*prev), strings.TrimSpace(got))
		}
		return nil
	}
}

// checkClusterOnline asserts the role is Online.
func checkClusterOnline(ct clusterTarget, role string) func(*terraform.State) error {
	return func(*terraform.State) error {
		_, state, err := clusterGroup(ct.nodes[0], role)
		if err != nil {
			return fmt.Errorf("cluster group probe: %w", err)
		}
		if !strings.EqualFold(state, "Online") {
			return fmt.Errorf("role %s state = %q, want Online", role, state)
		}
		return nil
	}
}

// checkClusterOwnerIn asserts the current owner is one of allowed (CLU-01).
func checkClusterOwnerIn(ct clusterTarget, role string, allowed ...string) func(*terraform.State) error {
	return func(*terraform.State) error {
		owner, err := ownerOf(ct, role)
		if err != nil {
			return fmt.Errorf("owner probe: %w", err)
		}
		for _, a := range allowed {
			if shortHost(a) == shortHost(owner) {
				return nil
			}
		}
		return fmt.Errorf("role %s owner = %q, want one of %v", role, owner, allowed)
	}
}

// checkClusterOwnerIs asserts the owner equals want (CLU-08 preferred owner).
func checkClusterOwnerIs(ct clusterTarget, role, want string) func(*terraform.State) error {
	return func(*terraform.State) error {
		owner, err := ownerOf(ct, role)
		if err != nil {
			return fmt.Errorf("owner probe: %w", err)
		}
		if shortHost(owner) != shortHost(want) {
			return fmt.Errorf("role %s OwnerNode = %q, want %s", role, owner, want)
		}
		return nil
	}
}

// checkClusterOwnerIsNot asserts the owner is NOT prev — used by CLU-02 where the
// rolling update must end with the role on the PREVIOUSLY-PASSIVE node.
func checkClusterOwnerIsNot(ct clusterTarget, role string, prev *string, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		owner, err := ownerOf(ct, role)
		if err != nil {
			return fmt.Errorf("owner probe: %w", err)
		}
		if *prev == "" {
			return fmt.Errorf("%s: pre-update owner was never captured", why)
		}
		if shortHost(owner) == shortHost(*prev) {
			return fmt.Errorf("%s: final owner %q == pre-update owner; a single-failover roll must land on the passive node", why, owner)
		}
		return nil
	}
}

// checkClusterHealthOnOwner resolves the current owner then asserts /health serves
// `want` on it (DESIGN §18.7: health is verified on the owner node).
func checkClusterHealthOnOwner(ct clusterTarget, role, url, want string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		owner, err := ownerOf(ct, role)
		if err != nil {
			return fmt.Errorf("owner probe: %w", err)
		}
		node, ok := ct.nodeByName(owner)
		if !ok {
			return fmt.Errorf("owner %q is not among the configured nodes %v", owner, ct.names)
		}
		return checkHealthBody(node, url, want)(s)
	}
}

// checkServiceDemandAllNodes asserts the generic service exists on EVERY node with
// StartMode DEMAND (Windows "Manual") — DESIGN §18.7 CLU-01 "service exists on
// cn1+cn2 (start_type DEMAND)".
func checkServiceDemandAllNodes(ct clusterTarget) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, n := range ct.nodes {
			if err := checkServiceStartMode(n, clusterSvc, "Manual")(s); err != nil {
				return fmt.Errorf("node %s: %w", n.host, err)
			}
		}
		return nil
	}
}

// checkJunctionAllNodes asserts `current` resolves to releases/<version> on EVERY
// node — DESIGN §18.7 "both nodes junction→<v>".
func checkJunctionAllNodes(ct clusterTarget, version string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, n := range ct.nodes {
			if err := checkCurrentTarget(n, version)(s); err != nil {
				return fmt.Errorf("node %s: %w", n.host, err)
			}
		}
		return nil
	}
}

// checkManifestAllNodes asserts every node's manifest records current=<version> —
// DESIGN §18.7 "both manifests current=<v>".
func checkManifestAllNodes(ct clusterTarget, version, why string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, n := range ct.nodes {
			if err := checkManifestVersion(n, version, why)(s); err != nil {
				return fmt.Errorf("node %s: %w", n.host, err)
			}
		}
		return nil
	}
}

// checkManifestAllNodesContains asserts every node's manifest carries token (e.g.
// `rolled_back`) — CLU-04.
func checkManifestAllNodesContains(ct clusterTarget, token, why string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, n := range ct.nodes {
			if err := checkManifestContains(n, token, why)(s); err != nil {
				return fmt.Errorf("node %s: %w", n.host, err)
			}
		}
		return nil
	}
}

// checkServiceAbsentAllNodes asserts the service is gone from every node (CLU-07).
func checkServiceAbsentAllNodes(ct clusterTarget) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, n := range ct.nodes {
			if err := checkServiceAbsent(n, clusterSvc)(s); err != nil {
				return fmt.Errorf("node %s: %w", n.host, err)
			}
		}
		return nil
	}
}

// checkRootGoneAllNodes asserts <installRoot>\sample-svc is gone from every node
// (CLU-07 destroy purge).
func checkRootGoneAllNodes(ct clusterTarget) func(*terraform.State) error {
	return func(s *terraform.State) error {
		for _, n := range ct.nodes {
			root := installRootFor(n) + `\sample-svc`
			if err := checkPathAbsent(n, root, "CLU-07: app root must be purged")(s); err != nil {
				return fmt.Errorf("node %s: %w", n.host, err)
			}
		}
		return nil
	}
}

// checkClusterRoleAbsent asserts Get-ClusterGroup no longer knows the role (CLU-07).
func checkClusterRoleAbsent(ct clusterTarget, role string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(ct.nodes[0],
			fmt.Sprintf("if (Get-ClusterGroup -Name '%s' -ErrorAction SilentlyContinue) { 'ROLE_PRESENT' } else { 'ROLE_ABSENT' }", psq(role)),
			"echo unsupported")
		if err != nil {
			return fmt.Errorf("role-absent probe: %w", err)
		}
		if !strings.Contains(out, "ROLE_ABSENT") {
			return fmt.Errorf("role %s still present after destroy (got %q)", role, strings.TrimSpace(out))
		}
		return nil
	}
}

// checkClusterMoveGroupOnce asserts the engine emitted exactly one MOVE_GROUP step
// for a plain rolling update (CLU-03 "exactly one MOVE_GROUP", tolerating the
// optional preferred-owner move which CLU-03 does not configure).
func checkClusterMoveGroupOnce(path, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		lines, err := readTFLogLines(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		if !haveAnyStepLog(lines) {
			return fmt.Errorf("%s: no deploy-step records captured; cannot count MOVE_GROUP", why)
		}
		re := stepLineRe("MOVE_GROUP")
		n := 0
		for _, ln := range lines {
			if re.MatchString(ln) {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%s: MOVE_GROUP emitted %d times, want exactly 1", why, n)
		}
		return nil
	}
}

// checkClusterUpdateOrder asserts the passive nodes were CONFIGUREd in hosts order
// and the former owner LAST (CLU-03 "passives updated in hosts order; former owner
// updated last"). former is the pre-update owner; passives is the hosts-ordered
// list of the remaining nodes.
func checkClusterUpdateOrder(path string, passives []string, former *string, why string) func(*terraform.State) error {
	// Match CONFIGURE records order-independently. The engine emits step fields from
	// a Go map (engine stepLogger.emit), so terraform-plugin-log renders "host=" and
	// "step=" in non-deterministic order — DESIGN §8.5 even documents them as
	// "step=<NAME> host=<h>". A regex that requires host= before step=CONFIGURE would
	// silently drop every record that happens to render "step=CONFIGURE … host=…",
	// dropping nodes from `order` and flaking the assertion. So identify the step with
	// the host-agnostic sibling helper, then extract the host with a separate search.
	stepRe := stepLineRe("CONFIGURE")
	hostRe := regexp.MustCompile(`(?i)\bhost=([^\s]+)`)
	return func(*terraform.State) error {
		lines, err := readTFLogLines(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		var order []string
		for _, ln := range lines {
			if stepRe.MatchString(ln) {
				if m := hostRe.FindStringSubmatch(ln); m != nil {
					order = append(order, shortHost(m[1]))
				}
			}
		}
		// Expected per-node CONFIGURE order: passives (hosts order) then former owner.
		want := make([]string, 0, len(passives)+1)
		for _, p := range passives {
			want = append(want, shortHost(p))
		}
		want = append(want, shortHost(*former))
		// Filter the observed order to the per-node CONFIGURE records we care about
		// (drop the coordinator role-registration CONFIGURE, which targets hosts[0]
		// but is not a per-node config step in this comparison).
		got := make([]string, 0, len(want))
		seen := map[string]bool{}
		for _, h := range order {
			for _, w := range want {
				if h == w && !seen[h] {
					got = append(got, h)
					seen[h] = true
				}
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			return fmt.Errorf("%s: per-node CONFIGURE order = %v, want %v (passives in hosts order, former owner last)", why, got, want)
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// Cluster scenario driver.
// ---------------------------------------------------------------------------

// clusterRunScenario drives a positive cluster scenario. Like runScenario it wires
// the reattach address and asserts `.lock` absent on the destroy — but across ALL
// cluster nodes (ct.all.tgt.Hosts).
func clusterRunScenario(t *testing.T, ct clusterTarget, steps ...resource.TestStep) {
	t.Helper()
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
		CheckDestroy:             checkLockAbsent(ct.all.tgt, installRootFor(ct.all), "sample-svc"),
	})
}

// clusterApplyStep mirrors applyStep but appends the all-node lock invariant.
func clusterApplyStep(ct clusterTarget, config string, checks ...resource.TestCheckFunc) resource.TestStep {
	all := append([]resource.TestCheckFunc{}, checks...)
	all = append(all, checkLockAbsent(ct.all.tgt, installRootFor(ct.all), "sample-svc"))
	return resource.TestStep{Config: config, Check: resource.ComposeAggregateTestCheckFunc(all...)}
}

// checkClusterSingleGap reads the 1 Hz downtime log the probe wrote on a node and
// asserts (1) enough samples were captured, (2) the role was healthy before and
// recovered after the outage, (3) there was EXACTLY ONE contiguous FAIL run, and
// (4) that run is ≤ maxSec (DESIGN §18.7 CLU-02 "single gap ≤30s and only ONE
// gap"). A single failover during a rolling drain must produce one bounded gap,
// never a flapping series.
func checkClusterSingleGap(node accTarget, maxSec int, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := hostReadFile(node, ldDowntimeLog)
		if err != nil {
			return fmt.Errorf("%s: read downtime log: %w", why, err)
		}
		samples := strings.Fields(out)
		if len(samples) < 5 {
			return fmt.Errorf("%s: only %d downtime samples captured; the 1 Hz probe did not run across the update", why, len(samples))
		}
		isFail := func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "FAIL") }
		gaps, maxRun, cur := 0, 0, 0
		firstFail, lastFail, okBefore := -1, -1, false
		for i, s := range samples {
			if isFail(s) {
				if cur == 0 {
					gaps++
				}
				if firstFail < 0 {
					firstFail = i
				}
				lastFail = i
				cur++
				if cur > maxRun {
					maxRun = cur
				}
			} else {
				cur = 0
				if firstFail < 0 {
					okBefore = true
				}
			}
		}
		if firstFail >= 0 {
			if !okBefore {
				return fmt.Errorf("%s: no successful health sample BEFORE the outage — capture is partial", why)
			}
			okAfter := false
			for i := lastFail + 1; i < len(samples); i++ {
				if !isFail(samples[i]) {
					okAfter = true
				}
			}
			if !okAfter {
				return fmt.Errorf("%s: role never returned healthy after the outage", why)
			}
		}
		if gaps != 1 {
			return fmt.Errorf("%s: observed %d distinct outage gaps, want EXACTLY 1 (a single bounded failover — zero gaps means the drain/failover was never observed through the access point)", why, gaps)
		}
		if maxRun > maxSec {
			return fmt.Errorf("%s: outage %ds exceeds bound %ds", why, maxRun, maxSec)
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// CLU-01..08
// ---------------------------------------------------------------------------

// CLU-01: fresh apply v1.0.0 role sample-role ⇒ service exists on both nodes
// (start_type DEMAND); role Online; owner ∈ nodes; health on owner ok; both
// manifests current=1.0.0; TF service_status=online.
func TestAccCLU01_FreshCluster(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	cfg := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", ""))
	clusterRunScenario(t, ct, clusterApplyStep(ct, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "online"),
		checkClusterOnline(ct, clusterRole),
		checkClusterOwnerIn(ct, clusterRole, ct.names...),
		checkServiceDemandAllNodes(ct),
		checkJunctionAllNodes(ct, "1.0.0"),
		checkManifestAllNodes(ct, "1.0.0", "CLU-01: both manifests must record current=1.0.0"),
		checkClusterHealthOnOwner(ct, clusterRole, healthURL(8080), "v=1.0.0"),
	))
}

// CLU-02: apply v1.1.0 while a 1 Hz health probe runs across the drain ⇒ apply ok;
// outage is a single gap ≤30s; final owner = pre-update PASSIVE node; both nodes
// junction→1.1.0; role Online.
func TestAccCLU02_RollingSingleFailover(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	v10 := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(clusterSpec(t, ct, "1.1.0", "zip", ""))
	capURL := clusterCAPHealthURL(t)
	var preOwner string
	clusterRunScenario(t, ct,
		clusterApplyStep(ct, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkClusterOnline(ct, clusterRole)),
		resource.TestStep{
			// Capture the pre-update owner, then launch the 1 Hz probe on the
			// coordinator across the rolling update (both must succeed or the
			// scenario setup FAILS — a discarded error would invalidate the bound).
			PreConfig: preConfig(t, func() error {
				o, err := ownerOf(ct, clusterRole)
				if err != nil {
					return err
				}
				preOwner = o
				// Probe the role's Client Access Point (cluster VIP), NOT
				// localhost — availability must be observed THROUGH the access
				// point so the single failover gap is genuinely measured.
				return startDowntimeProbe(ct.nodes[0], capURL, 150)
			}),
			Config: v11,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
				checkClusterOnline(ct, clusterRole),
				checkJunctionAllNodes(ct, "1.1.0"),
				checkClusterOwnerIsNot(ct, clusterRole, &preOwner, "CLU-02: final owner must be the pre-update passive node"),
				checkClusterSingleGap(ct.nodes[0], 30, "CLU-02: rolling update outage must be a single drain gap ≤30s"),
				checkLockAbsent(ct.all.tgt, installRootFor(ct.all), "sample-svc"),
			),
		},
	)
}

// CLU-03 (C3): apply v1.1.0 on 3 nodes ⇒ passives updated in hosts order (former
// owner last), exactly one MOVE_GROUP.
func TestAccCLU03_ThreeNodeOrder(t *testing.T) {
	accPreCheck(t)
	ct := requireC3(t)
	logPath := tfLogCapture(t)
	v10 := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(clusterSpec(t, ct, "1.1.0", "zip", ""))
	var former string
	clusterRunScenario(t, ct,
		clusterApplyStep(ct, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkClusterOnline(ct, clusterRole)),
		resource.TestStep{
			PreConfig: func() {
				truncateTFLog(t, logPath)()
				o, err := ownerOf(ct, clusterRole)
				if err != nil {
					t.Fatalf("CLU-03: capture pre-update owner: %v", err)
				}
				former = o
			},
			Config: v11,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
				checkClusterOnline(ct, clusterRole),
				checkJunctionAllNodes(ct, "1.1.0"),
				checkClusterMoveGroupOnce(logPath, "CLU-03: a plain rolling update must move the group exactly once"),
				checkClusterUpdateOrderC3(ct, logPath, &former),
				checkLockAbsent(ct.all.tgt, installRootFor(ct.all), "sample-svc"),
			),
		},
	)
}

// checkClusterUpdateOrderC3 computes the expected passive order (all nodes in hosts
// order EXCEPT the former owner) and asserts the former owner was updated last.
func checkClusterUpdateOrderC3(ct clusterTarget, logPath string, former *string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		passives := make([]string, 0, len(ct.names))
		for _, n := range ct.names {
			if shortHost(n) != shortHost(*former) {
				passives = append(passives, n)
			}
		}
		return checkClusterUpdateOrder(logPath, passives, former, "CLU-03: passives update in hosts order, former owner last")(s)
	}
}

// CLU-04: apply v1.1.0 built `--health 500` ⇒ ERR_HEALTH_CHECK; role Online back on
// the ORIGINAL owner; /health serves v=1.0.0; both nodes junction→1.0.0; manifests
// last_operation=rolled_back; the next plan is non-empty (desired 1.1.0 ≠ actual).
func TestAccCLU04_HealthFailRollsBack(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	v10 := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", ""))
	bad := accDeploymentConfig(clusterSpec(t, ct, "1.1.0-health500", "zip", ""))
	var origOwner string
	clusterRunScenario(t, ct,
		clusterApplyStep(ct, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkClusterOnline(ct, clusterRole),
			func(*terraform.State) error {
				o, err := ownerOf(ct, clusterRole)
				origOwner = o
				return err
			}),
		resource.TestStep{
			Config:      bad,
			ExpectError: mustRe(`ERR_HEALTH_CHECK`),
		},
		// Post-rollback: re-applying 1.0.0 is a no-op that lets Check observe the
		// rolled-back live cluster (role back on the original owner, health & both
		// junctions on 1.0.0, manifests rolled_back).
		clusterApplyStep(ct, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkClusterOnline(ct, clusterRole),
			func(s *terraform.State) error {
				return checkClusterOwnerIs(ct, clusterRole, origOwner)(s)
			},
			checkJunctionAllNodes(ct, "1.0.0"),
			checkClusterHealthOnOwner(ct, clusterRole, healthURL(8080), "v=1.0.0"),
			checkManifestAllNodesContains(ct, "rolled_back", "CLU-04: both manifests must record last_operation rolled_back"),
		),
		// The bad target 1.1.0-health500 still differs from the live 1.0.0, so a
		// plan of the bad config is non-empty (desired ≠ actual).
		resource.TestStep{Config: bad, PlanOnly: true, ExpectNonEmptyPlan: true},
	)
}

// CLU-05: with a healthy v1.0.0 baseline pre-seeded on C2 and WinRM to cn2
// firewalled, applying an UPDATE to v1.1.0 fails ERR_CONNECT host=cn2 at
// CONNECT/preflight, and cn1 is left UNCHANGED: the prior junction still resolves
// to the baseline release AND the role owner has not moved (no partial switch).
// Requires LABDEPLOY_ACC_C2_BASELINE=1 (operator pre-seeded 1.0.0) and
// LABDEPLOY_ACC_C2_CN2_BLOCKED=1 (cn2 WinRM firewalled). Probes ONLY cn1 (cn2 is
// intentionally unreachable; the WSFC nodes stay UP so cn1 still reports the owner).
func TestAccCLU05_NodeUnreachable(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	if os.Getenv(envC2Baseline) != "1" {
		t.Fatalf("CLU-05 under TF_ACC=1 requires %s=1 (a healthy v1.0.0 deployment pre-seeded on C2 before cn2 is firewalled) so the UPDATE can be shown to leave the prior junction + owner unchanged", envC2Baseline)
	}
	if os.Getenv(envCN2Blocked) != "1" {
		t.Fatalf("CLU-05 under TF_ACC=1 requires %s=1 (cn2 WinRM firewalled before the run) so the ERR_CONNECT-on-cn2 path is genuinely exercised", envCN2Blocked)
	}
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	cn2 := ct.names[len(ct.names)-1]
	// The update target: 1.1.0 (the baseline on the cluster is 1.0.0).
	cfg := accDeploymentConfig(clusterSpec(t, ct, "1.1.0", "zip", ""))
	var preOwner, preJunction string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			// Verify the pre-seeded baseline is genuinely a HEALTHY v1.0.0 on the
			// reachable node BEFORE attempting the update — otherwise "unchanged
			// after the failed update" would be vacuous. Assert: cn1 owns the role
			// (so the reachable node serves it), the role is Online, cn1's junction
			// resolves to release 1.0.0, and the role's health serves v=1.0.0. Then
			// capture the owner + junction so CheckDestroy can prove neither moved.
			PreConfig: preConfig(t, func() error {
				owner, state, err := clusterGroup(ct.nodes[0], clusterRole)
				if err != nil {
					return fmt.Errorf("probe baseline role: %w", err)
				}
				if !strings.EqualFold(state, "Online") {
					return fmt.Errorf("baseline role %s state = %q, want Online (pre-seed a healthy v1.0.0 before the run)", clusterRole, state)
				}
				if shortHost(owner) != shortHost(ct.names[0]) {
					return fmt.Errorf("baseline role owner = %q, want the reachable node %s (pre-seed with cn1 as owner so its health + junction can be verified while cn2 is firewalled)", owner, ct.names[0])
				}
				j, err := readCurrentTarget(ct.nodes[0])
				if err != nil {
					return fmt.Errorf("capture baseline junction on cn1: %w", err)
				}
				if !strings.Contains(strings.ToLower(j), "1.0.0") {
					return fmt.Errorf("baseline cn1 junction = %q, want it to resolve to release 1.0.0 (baseline is not v1.0.0)", strings.TrimSpace(j))
				}
				health, err := probeHost(ct.nodes[0],
					fmt.Sprintf("(Invoke-WebRequest -UseBasicParsing -Uri '%s').Content", psq(healthURL(8080))),
					fmt.Sprintf("curl -fsS '%s'", shq(healthURL(8080))))
				if err != nil {
					return fmt.Errorf("baseline health probe on cn1: %w", err)
				}
				if !strings.Contains(health, "v=1.0.0") {
					return fmt.Errorf("baseline health on cn1 = %q, want it to serve v=1.0.0", strings.TrimSpace(health))
				}
				preOwner = owner
				preJunction = j
				return nil
			}),
			Config:      cfg,
			ExpectError: mustRe(`ERR_CONNECT(?s).*` + regexp.QuoteMeta(shortHost(cn2))),
		}},
		// cn2 is unreachable; assert cn1's post-state proves NO partial switch: the
		// 1.1.0 release never landed, and the prior junction + role owner are
		// unchanged from the pre-update baseline.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkReleaseAbsent(ct.nodes[0], "1.1.0", "CLU-05: cn1 must be untouched — the attempted 1.1.0 release must not land when cn2 is unreachable"),
			checkJunctionUnchanged(ct.nodes[0], &preJunction, "CLU-05: cn1 junction must still resolve to the pre-update baseline release"),
			checkClusterOwnerUnchanged(ct, clusterRole, &preOwner, "CLU-05: role owner must not move during the failed update"),
			checkPathAbsent(ct.nodes[0], installRootFor(ct.nodes[0])+`\sample-svc\.lock`, "CLU-05: cn1 lock must be absent"),
		),
	})
}

// CLU-06: pre-create role sample-role bound to a different service (other-svc) ⇒
// ERR_SERVICE_INSTALL whose message includes both service names; nothing modified.
// Requires LABDEPLOY_ACC_CLU_ROLE_PRECONFLICT=1 (the operator pre-creates the
// conflicting role before the run).
func TestAccCLU06_RoleBindingConflict(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	if os.Getenv(envRolePreConflict) != "1" {
		t.Fatalf("CLU-06 under TF_ACC=1 requires %s=1 (role %q pre-created bound to other-svc) so the conflict path is genuinely exercised", envRolePreConflict, clusterRole)
	}
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	cfg := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", ""))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: mustRe(`ERR_SERVICE_INSTALL(?s).*(SampleSvc|sample-role)(?s).*other-svc`),
		}},
		// Nothing modified: the attempted release never landed on either node.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkReleaseAbsent(ct.nodes[0], "1.0.0", "CLU-06: conflict must modify nothing on cn1"),
			checkReleaseAbsent(ct.nodes[len(ct.nodes)-1], "1.0.0", "CLU-06: conflict must modify nothing on cn2"),
		),
	})
}

// CLU-07: destroy (purge) ⇒ role absent, services deleted on both nodes,
// <root>\sample-svc gone on both nodes.
func TestAccCLU07_DestroyPurge(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	cfg := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", ""))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			clusterApplyStep(ct, cfg,
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				checkClusterOnline(ct, clusterRole)),
		},
		// Terraform destroys the resource after the last step; the purge must remove
		// the role, both services and both app roots, and leave `.lock` absent.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkClusterRoleAbsent(ct, clusterRole),
			checkServiceAbsentAllNodes(ct),
			checkRootGoneAllNodes(ct),
			checkLockAbsent(ct.all.tgt, installRootFor(ct.all), "sample-svc"),
		),
	})
}

// CLU-08: preferred_owner=cn2, fresh + update ⇒ after EACH apply OwnerNode==cn2.
func TestAccCLU08_PreferredOwner(t *testing.T) {
	accPreCheck(t)
	ct := requireC2(t)
	cn2 := ct.names[len(ct.names)-1]
	v10 := accDeploymentConfig(clusterSpec(t, ct, "1.0.0", "zip", cn2))
	v11 := accDeploymentConfig(clusterSpec(t, ct, "1.1.0", "zip", cn2))
	clusterRunScenario(t, ct,
		clusterApplyStep(ct, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkClusterOnline(ct, clusterRole),
			checkClusterOwnerIs(ct, clusterRole, cn2)),
		clusterApplyStep(ct, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkClusterOnline(ct, clusterRole),
			checkClusterOwnerIs(ct, clusterRole, cn2)),
	)
}
