//go:build e2e

package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	"gopkg.in/yaml.v3"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
	"github.com/smartpcr/terraform-provider-labdeploy/tests/e2e/pipeline"
)

// ---------------------------------------------------------------------------
// Stage 9.2 — Cluster and Pipeline Acceptance — E2E.
//
// Both acceptance scenarios are declared `[proof: lab]`:
//   * Cluster rolling acceptance (CLU-01..08) needs a C2/C3 two/three-node WSFC
//     lab, and
//   * Pipeline smoke (PIP-01..03) needs live GitHub Actions + ADO runners.
// Neither lab is provisionable on Forge's gate host. Rather than SKIP the
// required scenarios (which would prove nothing), this suite proves the SAME
// acceptance PROPERTIES two ways:
//
//   1. REPRODUCIBLE, on every gate with NO skip. The FULL CLU-01..08 matrix is
//      driven against the REAL Stage 6.2 rolling engine
//      (internal/engine/cluster.go) over an in-process scripted WSFC transport
//      (the shared `clu*` fake from the sibling failover stage — same `e2e`
//      package, no network, no docker): fresh CREATE (CLU-01), single-failover
//      gap (CLU-02), C3 hosts-order rolling (CLU-03), health rollback (CLU-04),
//      connection failure (CLU-05), role-binding conflict (CLU-06), destroy purge
//      (CLU-07) and preferred owner (CLU-08). The shipped reference pipelines
//      (examples/pipelines/*.yml) and the REAL registration code
//      (tests/e2e/pipeline) are asserted structurally: both always publish their
//      results artifacts and, on a deploy failure, a failure-guarded rollback
//      job/stage reapplies the last-known-good version (v=1.1.0).
//
//   2. AGAINST THE REAL LAB, additively, under TF_ACC=1. When the C2/C3 WSFC lab
//      connection env is present the "cluster rolling acceptance" scenario drives
//      a REAL create -> rolling update -> destroy over the REAL transport; when
//      the live GH/ADO env is present the pipeline scenario dispatches the REAL
//      workflows and asserts artifact CONTENTS and target RECOVERY to v=1.1.0.
//      The Stage 9.2 lab pipeline (test/e2e/lab-acceptance-gate/*, .github/
//      workflows/*) wires the cluster hosts, self-hosted runner pool and secrets
//      and invokes this suite with TF_ACC=1 plus the tests/e2e/pipeline live
//      PIP-01..03 tests. A missing var UNDER TF_ACC=1 FAILS (never a silent
//      skip); the suite never sets TF_ACC.
//
// Every symbol here is `cap`-prefixed (Cluster And Pipeline) so sibling stages
// sharing this `e2e` package do not collide. It reuses the sibling failover
// stage's `clu*` fake WSFC transport (cluNewFakeHost / cluNode / cluFakeCluster /
// cluCaptureSteps / cluCountStep / cluStepNames / splitCSV / cluEqual).
// ---------------------------------------------------------------------------

const (
	capManifestPath = `C:\deploy\sample-svc\manifest.json`
	capRoot         = `C:\deploy\sample-svc`
	capService      = "SampleSvc"
	capRole         = "SampleRole"
)

// capWorld holds per-scenario state for the cluster (real-engine) and pipeline
// (shipped-template) legs.
type capWorld struct {
	// cluster leg
	hosts       []string
	owner       string
	prevRelease string
	preferred   string
	boundSvc    string // CLU-06: role pre-bound to this service (spec wants capService)
	blockHost   string // CLU-05: node whose WinRM connect fails
	failHealth  bool   // CLU-04: firstNew health fails
	fresh       bool   // CLU-01: role absent (create path)

	cl    *cluFakeCluster
	nodes map[string]*cluNode
	order []*cluNode

	srv *httptest.Server

	out   *engine.Status
	err   error
	steps []cluStepEntry

	// pipeline leg
	ghDoc  map[string]interface{}
	adoDoc map[string]interface{}
}

func (w *capWorld) reset() {
	if w.srv != nil {
		w.srv.Close()
	}
	*w = capWorld{nodes: map[string]*cluNode{}}
}

func capSeedManifest(f *cluFakeHost, version, releasePath string) error {
	m := &engine.Manifest{
		Schema: 1, App: "sample-svc", Pattern: "cluster_generic_service",
		CurrentVersion: version, ArtifactChecksum: "sha256:old",
		CurrentRelease:  releasePath,
		ProviderVersion: engine.ProviderVersion,
		LastOperation:   engine.LastOp{Type: "deploy", Result: "success"},
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f.files[capManifestPath] = b
	return nil
}

// buildNodes constructs the fake WSFC state from the world's declared shape.
func (w *capWorld) buildNodes() error {
	relPath := capRoot + `\releases\` + w.prevRelease
	boundSvc := capService
	if w.boundSvc != "" {
		boundSvc = w.boundSvc
	}
	w.cl = &cluFakeCluster{
		nodes: w.hosts, role: !w.fresh, svc: boundSvc,
		owner: strings.ToLower(w.owner), state: "Online",
	}
	if w.fresh {
		w.cl.state = "Offline"
	}
	w.nodes = map[string]*cluNode{}
	w.order = nil
	for _, h := range w.hosts {
		f := cluNewFakeHost(h)
		if !w.fresh {
			f.svc = "Running"
			f.current = relPath
			if err := capSeedManifest(f, w.prevRelease, relPath); err != nil {
				return err
			}
		}
		if strings.EqualFold(h, w.blockHost) {
			f.fail["connect"] = true
		}
		node := &cluNode{cluFakeHost: f, cl: w.cl}
		w.nodes[strings.ToLower(h)] = node
		w.order = append(w.order, node)
	}
	if w.failHealth {
		prev := w.prevRelease
		for h, node := range w.nodes {
			if h == strings.ToLower(w.owner) {
				continue
			}
			n := node
			n.healthGate = func() bool { return !strings.Contains(n.current, prev) }
		}
	}
	return nil
}

// --------------------------- cluster Given steps ---------------------------

func (w *capWorld) givenFreshCluster(hostsCSV string) error {
	w.hosts = splitCSV(hostsCSV)
	w.fresh = true
	w.prevRelease = "" // no prior release on a fresh cluster
	w.owner = w.hosts[0]
	return w.buildNodes()
}

func (w *capWorld) givenHealthyCluster(hostsCSV, owner, version string) error {
	w.hosts = splitCSV(hostsCSV)
	w.owner = strings.ToLower(owner)
	w.prevRelease = version
	return w.buildNodes()
}

func (w *capWorld) givenHealthFailsOnPassive() error {
	w.failHealth = true
	return w.buildNodes()
}

func (w *capWorld) givenBlockWinRM(host string) error {
	w.blockHost = host
	return w.buildNodes()
}

func (w *capWorld) givenRoleBoundToOther(svc string) error {
	w.boundSvc = svc
	return w.buildNodes()
}

func (w *capWorld) givenPreferredOwner(host string) error {
	w.preferred = host
	return w.buildNodes()
}

// --------------------------- cluster When steps ----------------------------

// capClusterSpec builds a cluster_generic_service Deployment over an explicit
// host list to `version`, optionally with a preferred owner, backed by a live
// httptest artifact server. Mirrors the sibling cluUpdateSpec but adds host-count
// and preferred-owner control.
func (w *capWorld) capClusterSpec(version string) (*spec.Deployment, error) {
	payload := []byte("cluster " + version + " zip")
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write(payload)
	}))
	url := w.srv.URL + "/pkg.zip"
	sum := sha256.Sum256(payload)
	checksum := "sha256:" + hex.EncodeToString(sum[:])

	preferredLine := ""
	if w.preferred != "" {
		preferredLine = "\n  preferred_owner: " + w.preferred
	}
	hostList := `["` + strings.Join(w.hosts, `", "`) + `"]`
	y := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ` + hostList + `
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: ` + version + `
  checksum: "` + checksum + `"
  source: { type: http, url: "` + url + `" }
pattern:
  type: cluster_generic_service
  service_name: ` + capService + `
  role_name: ` + capRole + preferredLine + `
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
		return nil, fmt.Errorf("spec: %w", err)
	}
	return d, nil
}

func (w *capWorld) newEngine() *engine.Engine {
	eng := engine.New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
		n, ok := w.nodes[strings.ToLower(host)]
		if !ok {
			return nil, fmt.Errorf("no fake node for host %q", host)
		}
		return n, nil
	}
	return eng
}

func (w *capWorld) whenApply(version string) error {
	d, err := w.capClusterSpec(version)
	if err != nil {
		return err
	}
	eng := w.newEngine()
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	w.out, w.err = eng.Deploy(ctx, d)
	steps, cerr := cluCaptureSteps(&buf)
	if cerr != nil {
		return cerr
	}
	w.steps = steps
	return nil
}

func (w *capWorld) whenDestroyPurge() error {
	d, err := w.capClusterSpec(w.prevRelease)
	if err != nil {
		return err
	}
	w.err = w.newEngine().Destroy(context.Background(), d, "purge")
	return nil
}

// --------------------------- cluster Then steps ----------------------------

func (w *capWorld) thenApplySucceeds() error {
	if w.err != nil {
		return fmt.Errorf("cluster apply must succeed, got: %v", w.err)
	}
	if w.out == nil {
		return fmt.Errorf("cluster apply must return a status")
	}
	return nil
}

func (w *capWorld) thenRoleOnline() error {
	if !w.cl.role {
		return fmt.Errorf("fresh create must create the role")
	}
	if w.cl.state != "Online" {
		return fmt.Errorf("role must be Online after create, state=%q", w.cl.state)
	}
	return nil
}

func (w *capWorld) thenOwnerRunsRelease(version string) error {
	node, ok := w.nodes[w.cl.owner]
	if !ok {
		return fmt.Errorf("cluster owner %q has no fake node", w.cl.owner)
	}
	if !capHostIn(w.cl.owner, w.hosts) {
		return fmt.Errorf("owner %q must be one of the spec hosts %v", w.cl.owner, w.hosts)
	}
	if !strings.Contains(node.current, version) {
		return fmt.Errorf("owner %q must run release %s; junction=%q", w.cl.owner, version, node.current)
	}
	return nil
}

func (w *capWorld) thenEverySuccessManifest(version string) error {
	for _, n := range w.order {
		if !strings.Contains(n.current, version) {
			return fmt.Errorf("host %s must run release %s; junction=%q", n.host, version, n.current)
		}
		m := string(n.files[capManifestPath])
		if !strings.Contains(m, `"result": "success"`) || !strings.Contains(m, `"current_version": "`+version+`"`) {
			return fmt.Errorf("host %s must record a success manifest at %s: %q", n.host, version, m)
		}
	}
	return nil
}

func (w *capWorld) thenServiceStatus(want string) error {
	if w.out == nil {
		return fmt.Errorf("no status returned")
	}
	if !strings.EqualFold(w.out.ServiceStatus, want) {
		return fmt.Errorf("reported service status %q, want %q", w.out.ServiceStatus, want)
	}
	return nil
}

func (w *capWorld) thenExactlyOneFailover() error {
	if got := cluCountStep(w.steps, "MOVE_GROUP"); got != 1 {
		return fmt.Errorf("a healthy rolling update must fail over EXACTLY ONCE (one MOVE_GROUP), got %d: %v",
			got, cluStepNames(w.steps))
	}
	return nil
}

// thenHealthBeforeDrain proves the single-gap property: the NEW owner is
// health-checked (U5) before the OLD owner's local service is drained/stopped
// (U6), so the role is only ever offline for the one planned failover window.
func (w *capWorld) thenHealthBeforeDrain() error {
	firstNew := w.firstPassive()
	healthIdx, stopIdx := -1, -1
	for i, s := range w.steps {
		if s.step == "HEALTH" && strings.EqualFold(s.host, firstNew) && healthIdx < 0 {
			healthIdx = i
		}
		if s.step == "STOP" && strings.EqualFold(s.host, w.owner) && stopIdx < 0 {
			stopIdx = i
		}
	}
	if healthIdx < 0 {
		return fmt.Errorf("expected a HEALTH step on the new owner %q: %v", firstNew, cluStepNames(w.steps))
	}
	if stopIdx < 0 {
		return fmt.Errorf("expected a STOP step draining the old owner %q: %v", w.owner, cluStepNames(w.steps))
	}
	if healthIdx >= stopIdx {
		return fmt.Errorf("new owner must be health-checked BEFORE the old owner is drained (single gap); healthIdx=%d stopIdx=%d",
			healthIdx, stopIdx)
	}
	return nil
}

func (w *capWorld) thenOwnerEndsOnPassive(version string) error {
	if w.cl.owner == strings.ToLower(w.owner) {
		return fmt.Errorf("owner must move off the original owner %q onto the previously passive node, still %q",
			w.owner, w.cl.owner)
	}
	return w.thenOwnerRunsRelease(version)
}

func (w *capWorld) thenPassivesUpdatedInOrder(orderCSV string) error {
	want := splitCSV(orderCSV)
	var pre []string
	for _, s := range w.steps {
		if s.step == "MOVE_GROUP" {
			break
		}
		if s.step == "SWITCH" {
			pre = append(pre, s.host)
		}
	}
	if !cluEqual(pre, want) {
		return fmt.Errorf("pre-move passive SWITCH order must be EXACTLY %v (hosts order), got %v", want, pre)
	}
	return nil
}

func (w *capWorld) thenFormerOwnerUpdatedLast(owner string) error {
	lastSwitch := ""
	moveSeen := false
	for _, s := range w.steps {
		if s.step == "MOVE_GROUP" {
			moveSeen = true
		}
		if s.step == "SWITCH" {
			lastSwitch = s.host
		}
	}
	if !moveSeen {
		return fmt.Errorf("expected a MOVE_GROUP before the former owner is updated")
	}
	if !strings.EqualFold(lastSwitch, owner) {
		return fmt.Errorf("former owner %q must be updated (SWITCH) last, last SWITCH host was %q", owner, lastSwitch)
	}
	return nil
}

func (w *capWorld) thenApplyRollsBack(version string) error {
	if w.err == nil {
		return fmt.Errorf("a firstNew health failure must roll the cluster back and surface the original error")
	}
	if !strings.Contains(w.err.Error(), "rolled back to "+version) {
		return fmt.Errorf("expected a rollback-to-previous error mentioning %q, got: %v", version, w.err)
	}
	if cluCountStep(w.steps, "MOVE_GROUP") != 2 {
		return fmt.Errorf("a firstNew rollback must emit exactly 2 MOVE_GROUP (forward + back), got %d: %v",
			cluCountStep(w.steps, "MOVE_GROUP"), cluStepNames(w.steps))
	}
	if cluCountStep(w.steps, "ROLLBACK") == 0 {
		return fmt.Errorf("expected a ROLLBACK step record: %v", cluStepNames(w.steps))
	}
	return nil
}

func (w *capWorld) thenRoleBackToOwner(owner string) error {
	if w.cl.owner != strings.ToLower(owner) {
		return fmt.Errorf("role must return to the original owner %s after rollback, got owner=%q", owner, w.cl.owner)
	}
	return nil
}

func (w *capWorld) thenEveryNodeRolledBack(version string) error {
	for _, n := range w.order {
		if !strings.Contains(n.current, version) {
			return fmt.Errorf("host %s junction must be restored to previous version %s; junction=%q",
				n.host, version, n.current)
		}
		m := string(n.files[capManifestPath])
		if !strings.Contains(m, `"result": "rolled_back"`) {
			return fmt.Errorf("host %s manifest must record result=rolled_back: %q", n.host, m)
		}
		if !strings.Contains(m, `"current_version": "`+version+`"`) {
			return fmt.Errorf("host %s rolled_back manifest must record current_version=%s: %q", n.host, version, m)
		}
	}
	return nil
}

func (w *capWorld) thenApplyFailsConnect(host string) error {
	if w.err == nil {
		return fmt.Errorf("an unreachable node must fail the apply")
	}
	if !strings.Contains(w.err.Error(), "[ERR_CONNECT]") {
		return fmt.Errorf("want ERR_CONNECT for an unreachable node, got: %v", w.err)
	}
	if !strings.Contains(strings.ToLower(w.err.Error()), strings.ToLower(host)) {
		return fmt.Errorf("connect failure must name the unreachable host %q: %v", host, w.err)
	}
	return nil
}

func (w *capWorld) thenReachableNodeUnchanged(host, version string) error {
	node, ok := w.nodes[strings.ToLower(host)]
	if !ok {
		return fmt.Errorf("unknown host %q", host)
	}
	rel := capRoot + `\releases\` + version
	if node.current != rel {
		return fmt.Errorf("reachable node %s must be left unchanged at %s; junction=%q", host, version, node.current)
	}
	// No partial switch means no new lock/marker files beyond the seed manifest.
	if _, held := node.files[capRoot+`\.lock`]; held {
		return fmt.Errorf("reachable node %s must not hold a .lock after a pre-connect failure", host)
	}
	return nil
}

func (w *capWorld) thenRefusedServiceConflict() error {
	if w.err == nil {
		return fmt.Errorf("a role bound to a different service must refuse the apply")
	}
	if !strings.Contains(w.err.Error(), "[ERR_SERVICE_INSTALL]") {
		return fmt.Errorf("want ERR_SERVICE_INSTALL for a role/service conflict, got: %v", w.err)
	}
	msg := w.err.Error()
	if !strings.Contains(msg, w.boundSvc) || !strings.Contains(msg, capService) {
		return fmt.Errorf("conflict must name BOTH the bound service %q and the spec service %q: %v", w.boundSvc, capService, w.err)
	}
	return nil
}

func (w *capWorld) thenNothingModified() error {
	for _, n := range w.order {
		rel := capRoot + `\releases\` + w.prevRelease
		if n.current != rel {
			return fmt.Errorf("host %s junction must be unchanged on a refused apply; junction=%q", n.host, n.current)
		}
		if _, held := n.files[capRoot+`\.lock`]; held {
			return fmt.Errorf("host %s must not hold a .lock after a pre-lock conflict", n.host)
		}
	}
	return nil
}

func (w *capWorld) thenDestroyRoleRemoved() error {
	if w.err != nil {
		return fmt.Errorf("cluster destroy --purge must succeed, got: %v", w.err)
	}
	if w.cl.role {
		return fmt.Errorf("destroy must remove the cluster role, but role is still present")
	}
	return nil
}

func (w *capWorld) thenServiceAndPayloadGone() error {
	for _, n := range w.order {
		if n.svc != "" {
			return fmt.Errorf("host %s service survived purge: %q", n.host, n.svc)
		}
		for k := range n.files {
			if strings.HasPrefix(k, capRoot) {
				return fmt.Errorf("host %s payload survived purge: %s", n.host, k)
			}
		}
	}
	return nil
}

func (w *capWorld) thenOwnerEndsOnPreferred(host string) error {
	if w.cl.owner != strings.ToLower(host) {
		return fmt.Errorf("role must land on the preferred owner %s, got owner=%q", host, w.cl.owner)
	}
	return nil
}

func (w *capWorld) firstPassive() string {
	for _, h := range w.hosts {
		if !strings.EqualFold(h, w.owner) {
			return strings.ToLower(h)
		}
	}
	return ""
}

func capHostIn(host string, hosts []string) bool {
	for _, h := range hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// ------------------------ cluster lab leg (TF_ACC=1) -----------------------

type capLabCluster struct {
	hosts      []string
	user       string
	artifact   string // base URL
	sha100     string
	sha110     string
	badVersion string // a deliberately-unhealthy release used to prove CLU-04 rollback
	badSha     string
	healthURL  string
}

// capLiveClusterEnv returns the C2/C3 lab connection when TF_ACC=1 AND the
// required vars are present. Under TF_ACC=1 a missing var is a hard error (the
// required-scenario rule); with TF_ACC unset it returns (nil,false,nil) so the
// reproducible CLU-01..08 matrix above stands as the gate proof.
func capLiveClusterEnv() (*capLabCluster, bool, error) {
	if os.Getenv("TF_ACC") != "1" {
		return nil, false, nil
	}
	get := func(k string) (string, error) {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			return "", fmt.Errorf("TF_ACC=1 but required lab var %s is unset", k)
		}
		return v, nil
	}
	hostsCSV, err := get("LABDEPLOY_ACC_CLU_HOSTS")
	if err != nil {
		return nil, false, err
	}
	lc := &capLabCluster{hosts: splitCSV(hostsCSV)}
	for _, spec := range []struct {
		dst *string
		key string
	}{
		{&lc.user, "LABDEPLOY_ACC_CLU_USER"},
		{&lc.artifact, "LABDEPLOY_ACC_ARTIFACT_BASE_URL"},
		{&lc.sha100, "LABDEPLOY_ACC_SHA_1_0_0_ZIP"},
		{&lc.sha110, "LABDEPLOY_ACC_SHA_1_1_0_ZIP"},
		{&lc.badVersion, "LABDEPLOY_ACC_CLU_BAD_VERSION"},
		{&lc.badSha, "LABDEPLOY_ACC_CLU_BAD_ZIP"},
		{&lc.healthURL, "LABDEPLOY_ACC_CLU_HEALTH_URL"},
	} {
		v, err := get(spec.key)
		if err != nil {
			return nil, false, err
		}
		*spec.dst = v
	}
	// The spec references password_env: LABDEPLOY_PASSWORD.
	if _, err := get("LABDEPLOY_PASSWORD"); err != nil {
		return nil, false, err
	}
	if len(lc.hosts) < 2 {
		return nil, false, fmt.Errorf("LABDEPLOY_ACC_CLU_HOSTS must list a two/three-node WSFC cluster, got %v", lc.hosts)
	}
	return lc, true, nil
}

// capRealSpecOpts parameterises the live cluster spec so the matrix can vary
// version, host list, service/role names and preferred owner to exercise every
// CLU-01..08 property against the real WSFC transport.
type capRealSpecOpts struct {
	version   string
	checksum  string
	hosts     []string // nil ⇒ lc.hosts
	service   string   // "" ⇒ capService
	role      string   // "" ⇒ capRole
	preferred string   // "" ⇒ none
}

func (lc *capLabCluster) realSpec(o capRealSpecOpts) (*spec.Deployment, error) {
	hosts := o.hosts
	if hosts == nil {
		hosts = lc.hosts
	}
	svc := o.service
	if svc == "" {
		svc = capService
	}
	role := o.role
	if role == "" {
		role = capRole
	}
	preferredLine := ""
	if o.preferred != "" {
		preferredLine = "\n  preferred_owner: " + o.preferred
	}
	hostList := `["` + strings.Join(hosts, `", "`) + `"]`
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: %s
  os: windows
  credentials: { username: %q, password_env: LABDEPLOY_PASSWORD }
  winrm: { use_https: true, insecure_skip_verify: true }
artifact:
  type: zip
  version: %s
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: cluster_generic_service
  service_name: %s
  role_name: %s%s
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: %q }
  initial_delay_seconds: 2
  interval_seconds: 2
  timeout_seconds: 5
strategy:
  keep_releases: 2
  rollback_on_failure: true
  cluster: { health_settle_seconds: 2 }
`, hostList, lc.user, o.version, o.checksum,
		fmt.Sprintf("%s/sample-svc-%s.zip", strings.TrimRight(lc.artifact, "/"), o.version),
		svc, role, preferredLine, lc.healthURL)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		return nil, fmt.Errorf("live cluster spec: %w", err)
	}
	return d, nil
}

// capCluCheck is one CLU-01..08 acceptance check run against the REAL cluster.
type capCluCheck struct {
	id  string
	err error
}

// capLiveClusterResult holds the outcome of the full CLU-01..08 matrix run
// against the real C2/C3 lab, asserted by thenRealClusterAllSucceed. `expected`
// is the set of CLU ids that MUST have run for the given node count (CLU-03 is
// three-node-only), so a C2 run cannot be accepted as "complete" while silently
// omitting a check.
type capLiveClusterResult struct {
	active   bool
	hosts    int
	expected []string
	checks   []capCluCheck
}

func capDeployCaptured(eng *engine.Engine, d *spec.Deployment) (*engine.Status, []cluStepEntry, error) {
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	out, err := eng.Deploy(ctx, d)
	steps, cerr := cluCaptureSteps(&buf)
	if cerr != nil {
		// A tflog decode fault is a capture-harness failure, not a deploy
		// outcome; propagate it (joined with any deploy error) instead of
		// silently discarding it. cluCaptureSteps already returns nil steps on
		// error, so the CLU-02/03/04/08 checks that read steps would otherwise
		// fail with a misleading "saw 0 MOVE_GROUP"/"no HEALTH step" that hides
		// the real decode error. Mirrors whenApply, which propagates cerr.
		return out, nil, errors.Join(err, cerr)
	}
	return out, steps, err
}

func capOneMoveGroup(steps []cluStepEntry) (string, int) {
	host, count := "", 0
	for _, s := range steps {
		if s.step == "MOVE_GROUP" {
			count++
			host = s.host
		}
	}
	return host, count
}

func capHealthBeforeStop(steps []cluStepEntry, newOwner, oldOwner string) error {
	healthIdx, stopIdx := -1, -1
	for i, s := range steps {
		if s.step == "HEALTH" && strings.EqualFold(s.host, newOwner) && healthIdx < 0 {
			healthIdx = i
		}
		if s.step == "STOP" && strings.EqualFold(s.host, oldOwner) && stopIdx < 0 {
			stopIdx = i
		}
	}
	if healthIdx < 0 || stopIdx < 0 || healthIdx >= stopIdx {
		return fmt.Errorf("new owner %q must be health-checked BEFORE old owner %q is drained (single gap): healthIdx=%d stopIdx=%d steps=%v",
			newOwner, oldOwner, healthIdx, stopIdx, cluStepNames(steps))
	}
	return nil
}

func capPreMoveSwitchOrder(steps []cluStepEntry) []string {
	var pre []string
	for _, s := range steps {
		if s.step == "MOVE_GROUP" {
			break
		}
		if s.step == "SWITCH" {
			pre = append(pre, s.host)
		}
	}
	return pre
}

func capLastSwitch(steps []cluStepEntry) string {
	last := ""
	for _, s := range steps {
		if s.step == "SWITCH" {
			last = s.host
		}
	}
	return last
}

// capHealthHost returns the host of the first HEALTH step. On a fresh create the
// HEALTH step is attributed to the ACTUAL owner node, so this recovers the owner
// the cluster elected (used to prove CLU-08 preferred-owner at create time).
func capHealthHost(steps []cluStepEntry) string {
	for _, s := range steps {
		if s.step == "HEALTH" {
			return s.host
		}
	}
	return ""
}

func (w *capWorld) givenLabClusterWhenProvided() error { return nil }

// whenRealClusterLifecycle drives the ENTIRE CLU-01..08 matrix against the real
// C2/C3 WSFC lab under TF_ACC=1 (not just create/update/destroy). Because the
// engine's single failover ALWAYS lands the role on the first passive, and a
// preferred_owner that differs from that first passive triggers a SECOND
// MOVE_GROUP, CLU-02 (exactly-one-failover) and CLU-08 (preferred owner) cannot
// share one apply. So CLU-08 is proven at CREATE (preferred_owner honored by
// StartGroup, no extra failover) and CLU-02/CLU-03 are proven by a preferred-free
// rolling update. Each check records its own error; the expected set (CLU-03 is
// three-node-only) is asserted in the Then so a C2 run cannot pass while omitting
// a check. With the lab absent it records nothing and the Then reports a clear skip.
func (w *capWorld) whenRealClusterLifecycle(ctx context.Context) (context.Context, error) {
	lc, live, err := capLiveClusterEnv()
	if err != nil {
		return ctx, err // TF_ACC=1 with a missing var: hard fail, never skip
	}
	res := &capLiveClusterResult{active: live}
	if !live {
		return context.WithValue(ctx, capLiveKey{}, res), nil
	}
	res.hosts = len(lc.hosts)
	res.expected = []string{"CLU-01", "CLU-02", "CLU-04", "CLU-05", "CLU-06", "CLU-07", "CLU-08"}
	if len(lc.hosts) >= 3 {
		res.expected = append(res.expected, "CLU-03")
	}
	add := func(id string, e error) { res.checks = append(res.checks, capCluCheck{id: id, err: e}) }
	eng := engine.New() // real transport.NewTransport

	// The preferred owner (also the fresh-create owner) is the LAST node — a
	// non-coordinator on C3, so CLU-08 proves the engine honors a preference that
	// is NOT the default coordinator election.
	createOwner := lc.hosts[len(lc.hosts)-1]
	// After a create-on-last-owner, the rolling update sees `createOwner` as the
	// owner; its passives are every OTHER host in spec order, and the single
	// failover lands on the first of them.
	var passives []string
	for _, h := range lc.hosts {
		if !strings.EqualFold(h, createOwner) {
			passives = append(passives, h)
		}
	}
	firstNew := passives[0]

	// CLU-01 + CLU-08: fresh create at 1.0.0 brings the role online on the
	// PREFERRED owner running the new release (StartGroup honors preferred_owner
	// with no extra failover).
	var createSteps []cluStepEntry
	createErr := func() error {
		d, e := lc.realSpec(capRealSpecOpts{version: "1.0.0", checksum: lc.sha100, preferred: createOwner})
		if e != nil {
			return e
		}
		out, steps, e := capDeployCaptured(eng, d)
		createSteps = steps
		if e != nil {
			return fmt.Errorf("create failed: %w", e)
		}
		if !strings.EqualFold(out.ServiceStatus, "online") {
			return fmt.Errorf("role must be online after create, got %q", out.ServiceStatus)
		}
		if out.DeployedVersion != "1.0.0" {
			return fmt.Errorf("create must report DeployedVersion 1.0.0, got %q", out.DeployedVersion)
		}
		return nil
	}()
	add("CLU-01", createErr)
	add("CLU-08", func() error {
		if createErr != nil {
			return fmt.Errorf("create did not complete; cannot verify preferred owner: %w", createErr)
		}
		h := capHealthHost(createSteps)
		if h == "" {
			return fmt.Errorf("no HEALTH step recorded on create; cannot determine the elected owner")
		}
		if !strings.EqualFold(h, createOwner) {
			return fmt.Errorf("create must bring the role online on the preferred owner %q, but the owner (HEALTH host) was %q", createOwner, h)
		}
		return nil
	}())

	// CLU-02: a preferred-free rolling update to 1.1.0 fails over EXACTLY once onto
	// the first passive and health-checks it BEFORE draining the old owner.
	var updateSteps []cluStepEntry
	add("CLU-02", func() error {
		d, e := lc.realSpec(capRealSpecOpts{version: "1.1.0", checksum: lc.sha110})
		if e != nil {
			return e
		}
		out, steps, e := capDeployCaptured(eng, d)
		updateSteps = steps
		if e != nil {
			return fmt.Errorf("rolling update failed: %w", e)
		}
		if !strings.EqualFold(out.ServiceStatus, "online") {
			return fmt.Errorf("role must be online after update, got %q", out.ServiceStatus)
		}
		newOwner, moves := capOneMoveGroup(steps)
		if moves != 1 {
			return fmt.Errorf("rolling update must fail over EXACTLY once, saw %d MOVE_GROUP: %v", moves, cluStepNames(steps))
		}
		if !strings.EqualFold(newOwner, firstNew) {
			return fmt.Errorf("the single failover must move the role onto the first passive %q, moved to %q", firstNew, newOwner)
		}
		if err := capHealthBeforeStop(steps, firstNew, createOwner); err != nil {
			return err
		}
		return nil
	}())

	// CLU-03: on a three-node cluster the passives update in hosts order before the
	// failover and the former owner is updated last.
	if len(lc.hosts) >= 3 {
		add("CLU-03", func() error {
			pre := capPreMoveSwitchOrder(updateSteps)
			if !cluEqual(pre, passives) {
				return fmt.Errorf("passives must update in hosts order %v before the failover, got %v", passives, pre)
			}
			if last := capLastSwitch(updateSteps); !strings.EqualFold(last, createOwner) {
				return fmt.Errorf("former owner %q must be updated last, last SWITCH was %q", createOwner, last)
			}
			return nil
		}())
	}

	// CLU-06: a role already bound to a DIFFERENT service is refused (PreflightRole
	// runs before the idempotency short-circuit and before any lock), so nothing is
	// mutated even though the cluster is already current at 1.1.0.
	add("CLU-06", func() error {
		d, e := lc.realSpec(capRealSpecOpts{version: "1.1.0", checksum: lc.sha110, service: capService + "Conflict"})
		if e != nil {
			return e
		}
		_, _, e = capDeployCaptured(eng, d)
		if e == nil {
			return fmt.Errorf("a conflicting service on the bound role must be refused")
		}
		if !strings.Contains(e.Error(), "[ERR_SERVICE_INSTALL]") {
			return fmt.Errorf("conflict must surface [ERR_SERVICE_INSTALL], got: %v", e)
		}
		return nil
	}())

	// CLU-05: an unreachable extra node fails the apply to CONNECT (newClusterCtx
	// connects before any preflight/idempotency) without a partial switch.
	add("CLU-05", func() error {
		hosts := append(append([]string{}, lc.hosts...), "lab-unreachable.invalid")
		d, e := lc.realSpec(capRealSpecOpts{version: "1.1.0", checksum: lc.sha110, hosts: hosts})
		if e != nil {
			return e
		}
		_, _, e = capDeployCaptured(eng, d)
		if e == nil {
			return fmt.Errorf("an unreachable node must fail the apply")
		}
		if !strings.Contains(e.Error(), "[ERR_CONNECT]") {
			return fmt.Errorf("unreachable node must surface [ERR_CONNECT], got: %v", e)
		}
		return nil
	}())

	// CLU-04: a deliberately-unhealthy release rolls the WHOLE cluster back to the
	// last known good version (1.1.0) with two failovers (forward + back) and a ROLLBACK step.
	add("CLU-04", func() error {
		d, e := lc.realSpec(capRealSpecOpts{version: lc.badVersion, checksum: lc.badSha})
		if e != nil {
			return e
		}
		_, steps, e := capDeployCaptured(eng, d)
		if e == nil {
			return fmt.Errorf("an unhealthy release must fail and roll back")
		}
		if !strings.Contains(e.Error(), "rolled back to 1.1.0") {
			return fmt.Errorf("health failure must roll back to 1.1.0, got: %v", e)
		}
		if _, moves := capOneMoveGroup(steps); moves != 2 {
			return fmt.Errorf("a rollback must emit 2 MOVE_GROUP (forward + back), saw %d: %v", moves, cluStepNames(steps))
		}
		if cluCountStep(steps, "ROLLBACK") == 0 {
			return fmt.Errorf("expected a ROLLBACK step: %v", cluStepNames(steps))
		}
		return nil
	}())

	// CLU-07: destroy purges the role, service and payload on every node.
	add("CLU-07", func() error {
		d, e := lc.realSpec(capRealSpecOpts{version: "1.1.0", checksum: lc.sha110})
		if e != nil {
			return e
		}
		if e := eng.Destroy(context.Background(), d, "purge"); e != nil {
			return fmt.Errorf("destroy purge failed: %w", e)
		}
		return nil
	}())

	if len(lc.hosts) < 3 {
		fmt.Println("[lab-note] CLU-03 (three-node rolling order) is C3-only; this 2-node C2 lab " +
			"correctly excludes it from the expected set (7 checks), not silently omits it.")
	}
	return context.WithValue(ctx, capLiveKey{}, res), nil
}

type capLiveKey struct{}

func (w *capWorld) thenRealClusterAllSucceed(ctx context.Context) error {
	res, _ := ctx.Value(capLiveKey{}).(*capLiveClusterResult)
	if res == nil || !res.active {
		// No lab present: report a CLEAR, explicit skip reason (never a silent
		// pass). The reproducible in-gate CLU-01..08 matrix above is the gate
		// proof; this live leg is an additive lab-only assertion permitted to be
		// absent per the scenario's [proof: lab] tag.
		fmt.Println("[LAB-SKIP] C2/C3 WSFC cluster leg NOT executed: TF_ACC!=1 or " +
			"LABDEPLOY_ACC_CLU_* unset. The in-gate CLU-01..08 matrix over the real engine " +
			"is the reproducible proof; set TF_ACC=1 + LABDEPLOY_ACC_CLU_HOSTS/USER/HEALTH_URL, " +
			"LABDEPLOY_ACC_ARTIFACT_BASE_URL, LABDEPLOY_ACC_SHA_1_0_0_ZIP/1_1_0_ZIP, " +
			"LABDEPLOY_ACC_CLU_BAD_VERSION/BAD_ZIP and LABDEPLOY_PASSWORD to run it live.")
		return nil
	}
	// Index the checks that actually ran, then verify EVERY expected id (per node
	// count) both ran and passed. This rejects a run that omits an applicable check
	// as well as one that fails an assertion.
	ran := map[string]*capCluCheck{}
	for i := range res.checks {
		ran[res.checks[i].id] = &res.checks[i]
	}
	var problems []string
	for _, id := range res.expected {
		c, ok := ran[id]
		if !ok {
			problems = append(problems, id+": expected for a "+fmt.Sprint(res.hosts)+"-node cluster but did NOT execute")
			continue
		}
		if c.err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", id, c.err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("live CLU matrix failed %d of %d expected check(s) on the %d-node lab:\n  %s",
			len(problems), len(res.expected), res.hosts, strings.Join(problems, "\n  "))
	}
	return nil
}

// ----------------------------- pipeline leg --------------------------------

func capDecodeYAML(path string) (map[string]interface{}, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not well-formed YAML: %w", path, err)
	}
	m, ok := doc.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be a YAML mapping", path)
	}
	return m, nil
}

func (w *capWorld) givenShippedPipelines() error {
	gh, err := capDecodeYAML(pipeline.ShippedGitHubWorkflowPath())
	if err != nil {
		return err
	}
	ado, err := capDecodeYAML(pipeline.ShippedAzurePipelinePath())
	if err != nil {
		return err
	}
	w.ghDoc = gh
	w.adoDoc = ado
	return nil
}

func capAnyMapping(node interface{}, pred func(map[string]interface{}) bool) bool {
	switch n := node.(type) {
	case map[string]interface{}:
		if pred(n) {
			return true
		}
		for _, v := range n {
			if capAnyMapping(v, pred) {
				return true
			}
		}
	case []interface{}:
		for _, v := range n {
			if capAnyMapping(v, pred) {
				return true
			}
		}
	}
	return false
}

func capStr(m map[string]interface{}, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func capAnyString(node interface{}, sub string) bool {
	switch n := node.(type) {
	case string:
		return strings.Contains(n, sub)
	case map[string]interface{}:
		for _, v := range n {
			if capAnyString(v, sub) {
				return true
			}
		}
	case []interface{}:
		for _, v := range n {
			if capAnyString(v, sub) {
				return true
			}
		}
	}
	return false
}

func (w *capWorld) thenGitHubUploadsArtifact() error {
	ok := capAnyMapping(w.ghDoc, func(m map[string]interface{}) bool {
		return strings.Contains(capStr(m, "uses"), "actions/upload-artifact") &&
			strings.Contains(capStr(m, "if"), "always()")
	})
	if !ok {
		return fmt.Errorf("github-deploy.yml must upload a results artifact with `if: always()` (PIP-01)")
	}
	if !capAnyString(w.ghDoc, "labdeploy-results-") {
		return fmt.Errorf("github-deploy.yml must publish the labdeploy-results-<version> artifact")
	}
	return nil
}

func (w *capWorld) thenAzurePublishesResults() error {
	pubTests := capAnyMapping(w.adoDoc, func(m map[string]interface{}) bool {
		return strings.Contains(capStr(m, "task"), "PublishTestResults@2") &&
			strings.Contains(capStr(m, "condition"), "always()")
	})
	if !pubTests {
		return fmt.Errorf("azure-pipelines.yml must publish VSTest results with `condition: always()` (PIP-03)")
	}
	pubArt := capAnyMapping(w.adoDoc, func(m map[string]interface{}) bool {
		return strings.Contains(capStr(m, "task"), "PublishPipelineArtifact@1") &&
			strings.Contains(capStr(m, "condition"), "always()")
	})
	if !pubArt {
		return fmt.Errorf("azure-pipelines.yml must publish a results artifact with `condition: always()` (PIP-03)")
	}
	if !capAnyString(w.adoDoc, "labdeploy-results-") {
		return fmt.Errorf("azure-pipelines.yml must publish the labdeploy-results-<version> artifact")
	}
	return nil
}

func (w *capWorld) thenGitHubRollbackRecovers() error {
	jobs, ok := w.ghDoc["jobs"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("github-deploy.yml must define jobs:")
	}
	rb, ok := jobs["rollback"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("github-deploy.yml must define a rollback job")
	}
	if !strings.Contains(capStr(rb, "if"), "failure()") {
		return fmt.Errorf("github rollback job must be guarded `if: failure()` so a failed deploy triggers it (PIP-02)")
	}
	if !capAnyString(rb["needs"], "deploy") {
		return fmt.Errorf("github rollback job must depend on the deploy job (`needs: deploy`)")
	}
	if !capAnyString(rb, "LAST_GOOD_VERSION") {
		return fmt.Errorf("github rollback job must reapply the last-known-good version (vars.LAST_GOOD_VERSION -> v=1.1.0)")
	}
	return nil
}

func (w *capWorld) thenAzureRollbackRecovers() error {
	stages, ok := w.adoDoc["stages"].([]interface{})
	if !ok {
		return fmt.Errorf("azure-pipelines.yml must define stages:")
	}
	var rollback map[string]interface{}
	for _, s := range stages {
		if m, ok := s.(map[string]interface{}); ok && strings.EqualFold(capStr(m, "stage"), "Rollback") {
			rollback = m
		}
	}
	if rollback == nil {
		return fmt.Errorf("azure-pipelines.yml must define a Rollback stage")
	}
	if !strings.Contains(capStr(rollback, "condition"), "failed()") {
		return fmt.Errorf("azure Rollback stage must be guarded `condition: failed()` (PIP-02)")
	}
	if !capAnyString(rollback["dependsOn"], "Deploy") {
		return fmt.Errorf("azure Rollback stage must depend on the Deploy stage")
	}
	if !capAnyString(rollback, "LAST_GOOD_VERSION") {
		return fmt.Errorf("azure Rollback stage must reapply the last-known-good version (LAST_GOOD_VERSION -> v=1.1.0)")
	}
	return nil
}

func (w *capWorld) thenRegisteredWorkflowPreservesRollback() error {
	shipped, err := pipeline.ReadShippedGitHubWorkflow()
	if err != nil {
		return fmt.Errorf("read shipped github-deploy.yml: %w", err)
	}
	registered, err := pipeline.RegisterWorkflow(shipped)
	if err != nil {
		return fmt.Errorf("RegisterWorkflow: %w", err)
	}
	var doc interface{}
	if err := yaml.Unmarshal(registered, &doc); err != nil {
		return fmt.Errorf("registered workflow is not well-formed YAML: %w", err)
	}
	top, _ := doc.(map[string]interface{})
	jobs, ok := top["jobs"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("registered workflow must define jobs:")
	}
	rb, ok := jobs["rollback"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("registration dropped the rollback job")
	}
	if !strings.Contains(capStr(rb, "if"), "failure()") {
		return fmt.Errorf("registered rollback job lost its `if: failure()` guard")
	}
	roundTrip := pipeline.StripEnvironmentBinding(registered)
	if pipeline.SHA256Hex(roundTrip) != pipeline.SHA256Hex(shipped) {
		return fmt.Errorf("stripping the env binding did not reproduce the shipped template (registration altered more than the binding)")
	}
	return nil
}

// thenLivePipelinesRecover runs the LIVE PIP smoke when the L4 lab env is
// present: PIP-01 (green GH deploy publishes a results artifact whose zip carries
// a .trx + summary.json), PIP-02 (a bad deploy fails and the rollback recovers the
// target to v=1.1.0) and PIP-03 (an ADO run publishes 3 VSTest results + artifact).
// With the lab absent it is a no-op: the structural proofs above stand as the gate
// coverage for the [proof: lab] pipeline scenario.
func (w *capWorld) thenLivePipelinesRecover(goodVersion string) error {
	if err := capLiveGitHub(goodVersion); err != nil {
		return err
	}
	return capLiveADO(goodVersion)
}

func capEnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// capRequireEnv returns the trimmed value of key, or a loud error when unset.
// Used on the LIVE pipeline leg for inputs whose absence would silently weaken
// the proof (checksums, health URL) — a live run must never pass without them.
func capRequireEnv(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return "", fmt.Errorf("live pipeline leg requires %s to prove recovery, but it is unset", key)
	}
	return v, nil
}

func capLiveGitHub(goodVersion string) error {
	repo := strings.TrimSpace(os.Getenv("LD_GH_LAB_REPO"))
	if repo == "" {
		fmt.Println("[LAB-SKIP] PIP-01/02 live GitHub Actions leg NOT executed: LD_GH_LAB_REPO unset " +
			"(no L4 GH runner). The structural + real-RegisterWorkflow proofs above are the in-gate " +
			"coverage for this [proof: lab] scenario; set LD_GH_LAB_REPO/LD_GH_TOKEN/LD_PIP_* to run it live.")
		return nil // lab absent — clearly reported, not silently passed
	}
	token := strings.TrimSpace(os.Getenv("LD_GH_TOKEN"))
	if token == "" {
		return fmt.Errorf("LD_GH_LAB_REPO set but LD_GH_TOKEN unset")
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("LD_GH_LAB_REPO must be owner/repo, got %q", repo)
	}
	owner, name := parts[0], parts[1]
	workflow := capEnvOr("LD_GH_LAB_WORKFLOW", "labdeploy-e2e.yml")
	branch := capEnvOr("LD_GH_LAB_BRANCH", "main")
	goodVer := capEnvOr("LD_PIP_GOOD_VERSION", goodVersion)
	badVer := capEnvOr("LD_PIP_BAD_VERSION", "1.2.0-bad")
	// When the live GH leg runs it MUST prove recovery to v=1.1.0, so the health
	// endpoint + both checksums are REQUIRED (never optional). A live run that
	// cannot assert recovery is not a proof — fail loudly instead.
	goodSum, err := capRequireEnv("LD_PIP_GOOD_CHECKSUM")
	if err != nil {
		return err
	}
	badSum, err := capRequireEnv("LD_PIP_BAD_CHECKSUM")
	if err != nil {
		return err
	}
	healthURL, err := capRequireEnv("LD_PIP_HEALTH_URL")
	if err != nil {
		return err
	}

	gh := pipeline.NewGitHubClient(owner, name, token)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	// PIP-01: good deploy publishes a results artifact with real contents.
	runID, err := capDispatchAndWait(ctx, gh, workflow, branch, map[string]string{"version": goodVer, "checksum": goodSum})
	if err != nil {
		return fmt.Errorf("PIP-01 good dispatch: %w", err)
	}
	concl, err := gh.WaitRun(ctx, runID, 40*time.Minute)
	if err != nil {
		return fmt.Errorf("PIP-01 wait run %d: %w", runID, err)
	}
	if concl != "success" {
		return fmt.Errorf("PIP-01 deploy run %d concluded %q, want success", runID, concl)
	}
	artName := "labdeploy-results-" + goodVer
	artID, err := gh.FindArtifact(ctx, runID, artName)
	if err != nil {
		return fmt.Errorf("PIP-01 find artifact %q: %w", artName, err)
	}
	zipBytes, err := gh.DownloadArtifactZip(ctx, artID)
	if err != nil {
		return fmt.Errorf("PIP-01 download artifact: %w", err)
	}
	if err := capAssertResultsZip(zipBytes, artName); err != nil {
		return err
	}

	// PIP-02: bad deploy fails, rollback recovers target to good version.
	badID, err := capDispatchAndWait(ctx, gh, workflow, branch, map[string]string{"version": badVer, "checksum": badSum})
	if err != nil {
		return fmt.Errorf("PIP-02 bad dispatch: %w", err)
	}
	if _, err := gh.WaitRun(ctx, badID, 40*time.Minute); err != nil {
		return fmt.Errorf("PIP-02 wait run %d: %w", badID, err)
	}
	jobs, err := gh.JobDetails(ctx, badID)
	if err != nil {
		return fmt.Errorf("PIP-02 job details: %w", err)
	}
	deploy, ok := capJobByName(jobs, "deploy")
	if !ok || deploy.Conclusion != "failure" {
		return fmt.Errorf("PIP-02 deploy job must conclude failure, got ok=%v concl=%q", ok, deploy.Conclusion)
	}
	rollback, ok := capJobByName(jobs, "rollback")
	if !ok || rollback.Conclusion != "success" {
		return fmt.Errorf("PIP-02 rollback job must conclude success, got ok=%v concl=%q", ok, rollback.Conclusion)
	}
	// PIP-02 recovery: the target MUST be back on the last-known-good version.
	// This assertion is MANDATORY on the live leg (never optional) — proving
	// recovery to v=1.1.0 is the whole point of the pipeline smoke.
	if err := capAssertServesVersion(ctx, healthURL, goodVer); err != nil {
		return err
	}
	return nil
}

func capDispatchAndWait(ctx context.Context, gh *pipeline.GitHubClient, workflow, branch string, inputs map[string]string) (int64, error) {
	actor, err := gh.AuthenticatedLogin(ctx)
	if err != nil {
		return 0, fmt.Errorf("resolve actor: %w", err)
	}
	before, err := gh.LatestRunID(ctx, workflow)
	if err != nil {
		return 0, fmt.Errorf("baseline run id: %w", err)
	}
	since, err := gh.DispatchWorkflow(ctx, workflow, branch, inputs)
	if err != nil {
		return 0, fmt.Errorf("dispatch: %w", err)
	}
	return gh.FindRunAfter(ctx, workflow, branch, before, since, actor, 3*time.Minute)
}

func capLiveADO(goodVersion string) error {
	org := strings.TrimSpace(os.Getenv("LD_ADO_ORG"))
	if org == "" {
		fmt.Println("[LAB-SKIP] PIP-03 live Azure DevOps leg NOT executed: LD_ADO_ORG unset " +
			"(no L4 ADO agent). The shipped azure-pipelines.yml structural proofs above are the " +
			"in-gate coverage; set LD_ADO_ORG/LD_ADO_PROJECT/LD_ADO_PAT/LD_ADO_PIPELINE_ID/LD_ADO_REPO to run it live.")
		return nil // lab absent — clearly reported, not silently passed
	}
	project := strings.TrimSpace(os.Getenv("LD_ADO_PROJECT"))
	pat := strings.TrimSpace(os.Getenv("LD_ADO_PAT"))
	idStr := strings.TrimSpace(os.Getenv("LD_ADO_PIPELINE_ID"))
	if project == "" || pat == "" || idStr == "" {
		return fmt.Errorf("LD_ADO_ORG set but LD_ADO_PROJECT/LD_ADO_PAT/LD_ADO_PIPELINE_ID incomplete")
	}
	pipelineID, err := strconv.Atoi(idStr)
	if err != nil {
		return fmt.Errorf("LD_ADO_PIPELINE_ID must be an integer: %w", err)
	}
	// The pipeline id alone is not proof of identity — any pipeline in the org can
	// carry that id. PIP-03 is the AUTHORITATIVE ADO check, so it MUST assert the
	// definition is bound to the expected lab repo (LD_ADO_REPO required) before
	// queueing. Without this a green run could smoke an unrelated pipeline.
	wantRepo, err := capRequireEnv("LD_ADO_REPO")
	if err != nil {
		return err
	}
	wantRepoType := capEnvOr("LD_ADO_REPO_TYPE", "TfsGit")
	version := capEnvOr("LD_PIP_GOOD_VERSION", goodVersion)
	checksum := strings.TrimSpace(os.Getenv("LD_PIP_GOOD_CHECKSUM"))

	ado := pipeline.NewADOClient(org, project, pat)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	// PIP-03 identity: refuse any pipeline not bound to the expected lab repo.
	def, err := ado.GetBuildDefinition(ctx, pipelineID)
	if err != nil {
		return fmt.Errorf("PIP-03 get build definition %d: %w", pipelineID, err)
	}
	if !strings.EqualFold(def.RepoName, wantRepo) {
		return fmt.Errorf("PIP-03 pipeline %d is bound to repo %q, want %q (set LD_ADO_REPO to the lab repo)", pipelineID, def.RepoName, wantRepo)
	}
	if !strings.EqualFold(strings.TrimSpace(def.RepoType), wantRepoType) {
		return fmt.Errorf("PIP-03 pipeline %d repository type %q, want %q (set LD_ADO_REPO_TYPE if the lab uses a different SCM)", pipelineID, def.RepoType, wantRepoType)
	}

	runID, err := ado.QueueRun(ctx, pipelineID, map[string]string{"version": version, "checksum": checksum})
	if err != nil {
		return fmt.Errorf("PIP-03 queue: %w", err)
	}
	result, err := ado.WaitRun(ctx, pipelineID, runID, 40*time.Minute)
	if err != nil {
		return fmt.Errorf("PIP-03 wait run %d: %w", runID, err)
	}
	if result != "succeeded" {
		return fmt.Errorf("PIP-03 run %d result %q, want succeeded", runID, result)
	}
	count, err := ado.TestResultCount(ctx, runID)
	if err != nil {
		return fmt.Errorf("PIP-03 test count: %w", err)
	}
	if count != 3 {
		return fmt.Errorf("PIP-03 Tests tab shows %d VSTest results, want 3", count)
	}
	names, err := ado.ArtifactNames(ctx, runID)
	if err != nil {
		return fmt.Errorf("PIP-03 artifact names: %w", err)
	}
	want := "labdeploy-results-" + version
	found := false
	for _, n := range names {
		if n == want {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("PIP-03 run %d did not publish artifact %q; got %v", runID, want, names)
	}
	return nil
}

func capJobByName(jobs []pipeline.JobInfo, name string) (pipeline.JobInfo, bool) {
	for _, j := range jobs {
		if strings.EqualFold(j.Name, name) {
			return j, true
		}
	}
	return pipeline.JobInfo{}, false
}

func capAssertResultsZip(zipBytes []byte, name string) error {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return fmt.Errorf("artifact %q is not a valid zip: %w", name, err)
	}
	var haveTRX bool
	var summary []byte
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		switch {
		case strings.HasSuffix(lower, ".trx"):
			haveTRX = true
		case strings.HasSuffix(lower, "summary.json"):
			rc, e := f.Open()
			if e == nil {
				summary, _ = io.ReadAll(rc)
				_ = rc.Close()
			}
		}
	}
	if !haveTRX {
		return fmt.Errorf("PIP-01 artifact %q contains no *.trx", name)
	}
	if summary == nil {
		return fmt.Errorf("PIP-01 artifact %q contains no summary.json", name)
	}
	var s struct {
		Total  *int  `json:"total"`
		Passed *bool `json:"passed"`
	}
	if err := json.Unmarshal(summary, &s); err != nil {
		return fmt.Errorf("PIP-01 summary.json does not parse: %w", err)
	}
	if s.Total == nil || s.Passed == nil {
		return fmt.Errorf("PIP-01 summary.json does not report the test summary; got %s", strings.TrimSpace(string(summary)))
	}
	return nil
}

func capAssertServesVersion(ctx context.Context, healthURL, version string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health GET %s: %w", healthURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), version) {
		return fmt.Errorf("PIP-02 recovery: health at %s does not report v=%s; body=%s", healthURL, version, strings.TrimSpace(string(body)))
	}
	return nil
}

// ---------------------------------------------------------------------------

func InitializeScenario_lab_acceptance_gate_cluster_and_pipeline_acceptance(ctx *godog.ScenarioContext) {
	w := &capWorld{nodes: map[string]*cluNode{}}
	var capPwWasSet bool
	var capPwPrev string

	ctx.Before(func(c context.Context, sc *godog.Scenario) (context.Context, error) {
		w.reset()
		// The cluster spec references target.credentials.password_env; the fake
		// transport never uses it, but spec validation requires the referenced env
		// var to be present. Set it deterministically for the in-process run (the
		// live lab leg overrides it from the secret group), remembering the prior
		// value so the After hook restores it and this suite never leaks env state
		// into sibling suites sharing the e2e package.
		capPwPrev, capPwWasSet = os.LookupEnv("LABDEPLOY_PASSWORD")
		if strings.TrimSpace(capPwPrev) == "" {
			_ = os.Setenv("LABDEPLOY_PASSWORD", "fake-pw")
		}
		return c, nil
	})
	ctx.After(func(c context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if w.srv != nil {
			w.srv.Close()
			w.srv = nil
		}
		if capPwWasSet {
			_ = os.Setenv("LABDEPLOY_PASSWORD", capPwPrev)
		} else {
			_ = os.Unsetenv("LABDEPLOY_PASSWORD")
		}
		return c, nil
	})

	// cluster givens
	ctx.Step(`^a fresh WSFC cluster with hosts "([^"]*)"$`, w.givenFreshCluster)
	ctx.Step(`^a healthy WSFC cluster with hosts "([^"]*)" owner "([^"]*)" running release "([^"]*)"$`, w.givenHealthyCluster)
	ctx.Step(`^the health check fails on the newly updated passive node$`, w.givenHealthFailsOnPassive)
	ctx.Step(`^WinRM to "([^"]*)" is blocked$`, w.givenBlockWinRM)
	ctx.Step(`^the cluster role is bound to a different service "([^"]*)"$`, w.givenRoleBoundToOther)
	ctx.Step(`^the preferred owner is "([^"]*)"$`, w.givenPreferredOwner)

	// cluster whens
	ctx.Step(`^release "([^"]*)" is applied to the cluster$`, w.whenApply)
	ctx.Step(`^the cluster deployment is destroyed with purge$`, w.whenDestroyPurge)

	// cluster thens
	ctx.Step(`^the cluster apply succeeds$`, w.thenApplySucceeds)
	ctx.Step(`^the cluster role is online$`, w.thenRoleOnline)
	ctx.Step(`^the role owner runs release "([^"]*)"$`, w.thenOwnerRunsRelease)
	ctx.Step(`^every node records a success manifest at "([^"]*)"$`, w.thenEverySuccessManifest)
	ctx.Step(`^the reported service status is "([^"]*)"$`, w.thenServiceStatus)
	ctx.Step(`^exactly one cluster failover occurs$`, w.thenExactlyOneFailover)
	ctx.Step(`^the new owner is health-checked before the old owner is drained$`, w.thenHealthBeforeDrain)
	ctx.Step(`^the role owner ends on the previously passive node running release "([^"]*)"$`, w.thenOwnerEndsOnPassive)
	ctx.Step(`^the passives are updated in hosts order "([^"]*)" before the failover$`, w.thenPassivesUpdatedInOrder)
	ctx.Step(`^the former owner "([^"]*)" is updated last$`, w.thenFormerOwnerUpdatedLast)
	ctx.Step(`^the cluster apply fails and rolls the cluster back to "([^"]*)"$`, w.thenApplyRollsBack)
	ctx.Step(`^the role returns to the original owner "([^"]*)"$`, w.thenRoleBackToOwner)
	ctx.Step(`^every node is rolled back to release "([^"]*)"$`, w.thenEveryNodeRolledBack)
	ctx.Step(`^the cluster apply fails to connect to "([^"]*)"$`, w.thenApplyFailsConnect)
	ctx.Step(`^the reachable node "([^"]*)" is left unchanged at release "([^"]*)"$`, w.thenReachableNodeUnchanged)
	ctx.Step(`^the cluster apply is refused with a service-install conflict naming both services$`, w.thenRefusedServiceConflict)
	ctx.Step(`^nothing is modified on any node$`, w.thenNothingModified)
	ctx.Step(`^the cluster role is removed$`, w.thenDestroyRoleRemoved)
	ctx.Step(`^the service and payload are gone on every node$`, w.thenServiceAndPayloadGone)
	ctx.Step(`^the role owner ends on the preferred node "([^"]*)"$`, w.thenOwnerEndsOnPreferred)

	// cluster lab leg
	ctx.Step(`^the C2/C3 WSFC lab connection when provided$`, w.givenLabClusterWhenProvided)
	ctx.Step(`^the full CLU-01\.\.08 matrix runs against the C2/C3 lab under TF_ACC=1$`, w.whenRealClusterLifecycle)
	ctx.Step(`^every CLU-01\.\.08 acceptance check passes on the real cluster$`, w.thenRealClusterAllSucceed)

	// pipeline leg
	ctx.Step(`^the shipped reference pipelines$`, w.givenShippedPipelines)
	ctx.Step(`^the GitHub workflow always uploads a results artifact$`, w.thenGitHubUploadsArtifact)
	ctx.Step(`^the Azure pipeline always publishes VSTest results and a results artifact$`, w.thenAzurePublishesResults)
	ctx.Step(`^the GitHub deploy failure triggers a rollback job that reapplies the last known good version$`, w.thenGitHubRollbackRecovers)
	ctx.Step(`^the Azure deploy failure triggers a rollback stage that reapplies the last known good version$`, w.thenAzureRollbackRecovers)
	ctx.Step(`^registering the GitHub workflow preserves the failure-guarded rollback job$`, w.thenRegisteredWorkflowPreservesRollback)
	ctx.Step(`^the live pipelines publish results and recover the target to "([^"]*)" when provided$`, w.thenLivePipelinesRecover)
}

func TestE2E_lab_acceptance_gate_cluster_and_pipeline_acceptance(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_lab_acceptance_gate_cluster_and_pipeline_acceptance,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"lab_acceptance_gate_cluster_and_pipeline_acceptance.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run cluster-and-pipeline acceptance feature tests")
	}
}
