package engine

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ----------------------------------------------------------------------------
// Multi-host cluster step-logging test (implementation-plan.md:225 / Scenario
// "Conditional and per-host steps logged"). A genuine 2-node WSFC rolling update
// where ONE node has the new release cached (skips FETCH/CHECKSUM) and a
// preferred-owner health failure drives a rollback that REPEATS the SWITCH step
// on both nodes. Every emitted record — per-node and repeated — still carries
// the full app/host/step/version/duration_ms field set.
// ----------------------------------------------------------------------------

// fakeCluster is the WSFC state shared across all nodes (role existence, bound
// service, current owner, group state). The coordinator cmdlets mutate it.
type fakeCluster struct {
	nodes []string
	role  bool
	svc   string
	owner string
	state string
}

var reNodeArg = regexp.MustCompile(`-Node '([^']+)'`)

// clusterNode wraps a per-node fakeHost and additionally answers the coordinator
// FailoverClusters cmdlets against the shared cluster state; all non-cluster
// scripts (staging, service, junction, health) delegate to the embedded fake.
type clusterNode struct {
	*fakeHost
	cl *fakeCluster
}

func (n *clusterNode) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "Add-ClusterGenericServiceRole"): // CreateRole
		n.cl.role = true
		n.cl.state = "Online"
		return ok(""), nil
	case strings.Contains(s, "Remove-ClusterGroup"): // RemoveRole
		n.cl.role = false
		return ok(""), nil
	case strings.Contains(s, "Move-ClusterGroup"): // MoveGroup
		if m := reNodeArg.FindStringSubmatch(s); m != nil {
			n.cl.owner = strings.ToLower(m[1])
		}
		n.cl.state = "Online"
		return ok(""), nil
	case strings.Contains(s, "Start-ClusterGroup"): // StartGroup
		n.cl.state = "Online"
		return ok(""), nil
	case strings.Contains(s, "Set-ClusterOwnerNode"): // SetPreferredOwners
		return ok(""), nil
	case strings.Contains(s, "Stop-ClusterGroup"): // StopGroup
		n.cl.state = "Offline"
		return ok(""), nil
	case strings.Contains(s, "Get-ClusterResource"): // RoleBinding
		if !n.cl.role {
			return ok("ABSENT"), nil
		}
		return ok("PRESENT|" + n.cl.svc + "|" + n.cl.owner + "|" + n.cl.state), nil
	case strings.Contains(s, "Get-ClusterNode"): // NodesUp
		return ok(strings.Join(n.cl.nodes, "\n")), nil
	case strings.Contains(s, "FailoverClusters module missing"): // pattern Preflight
		return ok(""), nil
	case strings.Contains(s, "Get-ClusterGroup") && strings.Contains(s, "not_installed"): // Status
		if !n.cl.role {
			return ok("not_installed"), nil
		}
		return ok(strings.ToLower(n.cl.state)), nil
	}
	return n.fakeHost.Exec(ctx, c)
}

func TestStepLogClusterMultiHostUpdateRollback(t *testing.T) {
	payload := []byte("cluster v2 zip")
	url, sum, done := testArtifactServer(t, payload)
	defer done()

	cl := &fakeCluster{
		nodes: []string{"lab-01", "lab-02"},
		role:  true, svc: "SampleSvc", owner: "lab-01", state: "Online",
	}
	newNode := func(host string) *clusterNode {
		f := newFakeHost(host)
		f.svc = "Running"
		// Both nodes have a previous 1.0.0 deployment recorded.
		seedManifest(t, f, `C:\deploy\sample-svc\manifest.json`, &Manifest{
			Schema: 1, App: "sample-svc", Pattern: "cluster_generic_service",
			CurrentVersion: "1.0.0", ArtifactChecksum: "sha256:old",
			CurrentRelease:  `C:\deploy\sample-svc\releases\1.0.0`,
			ProviderVersion: ProviderVersion,
			LastOperation:   LastOp{Type: "deploy", Result: "success"},
		})
		return &clusterNode{fakeHost: f, cl: cl}
	}
	n1 := newNode("lab-01")
	n2 := newNode("lab-02")
	// The passive node (lab-02) already has the new 2.0.0 release fully extracted
	// ⇒ its stage skips FETCH/CHECKSUM. The owner (lab-01) is NOT cached ⇒ its U6
	// stage DOES fetch, so exactly one host skips FETCH/CHECKSUM.
	seedReleaseMarker(t, n2.fakeHost, `C:\deploy\sample-svc\releases\2.0.0`, "2.0.0", sum)

	// lab-01 is the preferred owner: after the role is updated on lab-02 (U4/U5
	// pass) and the old owner lab-01 is updated (U6), U7 moves the role back to
	// lab-01 and its health at 2.0.0 FAILS, driving the rollback. lab-01 health
	// passes once its junction is restored to 1.0.0; lab-02 health always passes.
	n1.fakeHost.healthGate = func() bool { return strings.Contains(n1.fakeHost.current, "2.0.0") }

	nodes := map[string]*clusterNode{"lab-01": n1, "lab-02": n2}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
		return nodes[strings.ToLower(host)], nil
	}

	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	_, err := eng.Deploy(ctx, clusterUpdateSpec(t, url, sum))
	if err == nil {
		t.Fatalf("expected rolled-back cluster deploy to return the original error")
	}
	if !strings.Contains(err.Error(), "rolled back to 1.0.0") {
		t.Fatalf("expected cluster rollback-to-previous error, got: %v", err)
	}

	steps := captureSteps(t, &buf)
	names := stepNames(steps)

	// Per-host projection.
	byHost := map[string][]stepEntry{}
	for _, s := range steps {
		byHost[s.host] = append(byHost[s.host], s)
	}
	if len(byHost["lab-01"]) == 0 || len(byHost["lab-02"]) == 0 {
		t.Fatalf("expected step records for BOTH nodes; got hosts=%v", func() []string {
			var hs []string
			for h := range byHost {
				hs = append(hs, h)
			}
			return hs
		}())
	}

	// Conditional-skip: the cached passive (lab-02) never logs FETCH/CHECKSUM,
	// while the non-cached owner (lab-01) does — exactly one host skips them.
	if countStep(byHost["lab-02"], "FETCH") != 0 || countStep(byHost["lab-02"], "CHECKSUM") != 0 {
		t.Fatalf("cached passive lab-02 must NOT log FETCH/CHECKSUM: %v", stepNames(byHost["lab-02"]))
	}
	if countStep(byHost["lab-01"], "FETCH") == 0 || countStep(byHost["lab-01"], "CHECKSUM") == 0 {
		t.Fatalf("non-cached owner lab-01 must log FETCH and CHECKSUM: %v", stepNames(byHost["lab-01"]))
	}

	// Rollback repeats SWITCH: each node is switched forward and then again
	// during rollback restore.
	if got := countStep(byHost["lab-02"], "SWITCH"); got < 2 {
		t.Fatalf("expected passive lab-02 SWITCH repeated (forward + rollback), got %d: %v", got, stepNames(byHost["lab-02"]))
	}
	if got := countStep(byHost["lab-01"], "SWITCH"); got < 2 {
		t.Fatalf("expected owner lab-01 SWITCH repeated (update + rollback), got %d: %v", got, stepNames(byHost["lab-01"]))
	}
	if countStep(steps, "ROLLBACK") == 0 {
		t.Fatalf("expected a ROLLBACK step record: %v", names)
	}

	// Every emitted record — per-node and repeated — carries the full field set
	// (captureSteps already enforces presence + numeric duration_ms). Assert the
	// contextual fields are populated and correct per node.
	for _, s := range steps {
		if s.app != "sample-svc" {
			t.Fatalf("step %q has wrong app=%q", s.step, s.app)
		}
		if s.host != "lab-01" && s.host != "lab-02" {
			t.Fatalf("step %q emitted for unexpected host %q", s.step, s.host)
		}
		if s.version == "" {
			t.Fatalf("step %q on %q missing version", s.step, s.host)
		}
	}
}

// TestClusterCreatePartialCleanup covers evaluator iter5 item 4 / DESIGN §10.3
// row 1: when a FRESH cluster create fails at C1 after an earlier node was
// already switched+configured, the partially-deployed nodes must be cleaned
// (junction removed, service uninstalled) and a failed manifest persisted, not
// left as a partial deployment.
func TestClusterCreatePartialCleanup(t *testing.T) {
	payload := []byte("cluster create zip")
	url, sum, done := testArtifactServer(t, payload)
	defer done()

	// Fresh cluster: no role yet ⇒ deployCluster takes the clusterCreate (C1..C6)
	// path.
	cl := &fakeCluster{
		nodes: []string{"lab-01", "lab-02"},
		role:  false, svc: "SampleSvc", owner: "", state: "Offline",
	}
	n1 := &clusterNode{fakeHost: newFakeHost("lab-01"), cl: cl}
	n2 := &clusterNode{fakeHost: newFakeHost("lab-02"), cl: cl}
	// C1 processes lab-01 first (stage→switch→configure OK), then lab-02 whose
	// SWITCH fails — lab-01 is now partially deployed and must be cleaned.
	n2.fakeHost.fail["switch"] = true

	nodes := map[string]*clusterNode{"lab-01": n1, "lab-02": n2}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
		return nodes[strings.ToLower(host)], nil
	}

	_, err := eng.Deploy(context.Background(), clusterUpdateSpec(t, url, sum))
	if err == nil {
		t.Fatalf("expected fresh cluster create to fail at lab-02 SWITCH")
	}
	// lab-01 was switched+configured, so it MUST be cleaned during C1 rollback.
	if n1.fakeHost.current != "" {
		t.Fatalf("partially-deployed lab-01 junction must be removed, got %q", n1.fakeHost.current)
	}
	if n1.fakeHost.svc != "" {
		t.Fatalf("partially-deployed lab-01 service must be uninstalled, got %q", n1.fakeHost.svc)
	}
	// A failed manifest must be persisted (§10.6) so Read reports the drift.
	m := string(n1.fakeHost.files[`C:\deploy\sample-svc\manifest.json`])
	if m == "" || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("fresh cluster create cleanup must persist a failed manifest on lab-01: %q", m)
	}
	if !strings.Contains(err.Error(), "nodes cleaned") {
		t.Fatalf("want cleaned-nodes outcome surfaced, got: %v", err)
	}
}

// clusterUpdateSpec is a 2-node cluster_generic_service Deployment updating to
// 2.0.0 with lab-01 as preferred owner and a short settle to keep the test fast.
func clusterUpdateSpec(t *testing.T, url, checksum string) *spec.Deployment {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01", "lab-02"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 2.0.0
  checksum: "` + checksum + `"
  source: { type: http, url: "` + url + `" }
pattern:
  type: cluster_generic_service
  service_name: SampleSvc
  role_name: SampleRole
  preferred_owner: lab-01
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy:
  keep_releases: 2
  rollback_on_failure: true
  cluster: { health_settle_seconds: 1 }
`
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}
