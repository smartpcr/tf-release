package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// nodeWebSpec builds a single-host node_web_app deployment (winsw-wrapped).
// install_deps toggles the STAGE-phase `npm ci --omit=dev`.
func nodeWebSpec(t *testing.T, url, checksum string, installDeps bool) *spec.Deployment {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "%s"
  source: { type: http, url: "%s" }
pattern:
  type: node_web_app
  service_name: SampleWeb
  entry: server.js
  port: 8087
  winsw_exe: tools\WinSW.exe
  install_deps: %t
health_check:
  type: http
  http: { url: "http://localhost:8087/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy: { keep_releases: 2, rollback_on_failure: true }
`, checksum, url, installDeps)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}

// dotnetAPISpec builds a single-host dotnet_api deployment (native SCM hosting).
func dotnetAPISpec(t *testing.T, url, checksum string) *spec.Deployment {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "%s"
  source: { type: http, url: "%s" }
pattern:
  type: dotnet_api
  service_name: SampleApi
  launcher: exe
  exe: SampleApi.exe
  urls: "http://+:8088"
health_check:
  type: http
  http: { url: "http://localhost:8088/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy: { keep_releases: 2, rollback_on_failure: true }
`, checksum, url)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}

// Evaluator iter2 item 1: a same-artifact configuration change to node_web_app
// or dotnet_api must be reconfigurable (routed through Reconfigure so CONFIGURE
// actually applies PORT/entry/args/hosting/ASPNETCORE_URLS), not silently
// dropped by Deploy's idempotency short-circuit.
func TestReconfigurableCoversNodeAndDotnet(t *testing.T) {
	u := "http://x/pkg.zip"
	sum := "sha256:" + strings.Repeat("a", 64)

	node := nodeWebSpec(t, u, sum, false)
	if !reconfigurable(node, node) {
		t.Error("node_web_app same-artifact change must be reconfigurable")
	}
	dotnet := dotnetAPISpec(t, u, sum)
	if !reconfigurable(dotnet, dotnet) {
		t.Error("dotnet_api same-artifact change must be reconfigurable")
	}

	// A changed artifact (version bump) must NOT be reconfigurable — it deploys.
	bumped := dotnetAPISpec(t, u, sum)
	bumped.Artifact.Version = "2.0.0"
	if reconfigurable(bumped, dotnet) {
		t.Error("version bump must fall through to Deploy, not Reconfigure")
	}

	// Multi-host is never reconfigurable (Reconfigure is single-host).
	multi := dotnetAPISpec(t, u, sum)
	multi.Target.Hosts = []string{"lab-01", "lab-02"}
	if reconfigurable(multi, dotnet) {
		t.Error("multi-host must not be reconfigurable")
	}

	// Toggling install_deps is now reconfigurable: Reconfigure runs the pattern
	// preflight AND re-runs InstallDeps (npm ci) at the pre-activation point, so the
	// toggle actually installs deps instead of being swallowed by Deploy's
	// unchanged-artifact idempotency short-circuit (iter3 item 1).
	prior := nodeWebSpec(t, u, sum, false)
	next := nodeWebSpec(t, u, sum, true)
	if !reconfigurable(next, prior) {
		t.Error("install_deps toggle must be reconfigurable so Reconfigure runs npm ci")
	}
	// install_deps unchanged (both true) is still reconfigurable.
	if !reconfigurable(nodeWebSpec(t, u, sum, true), nodeWebSpec(t, u, sum, true)) {
		t.Error("install_deps unchanged must remain reconfigurable")
	}
}

// Evaluator iter2 item 1 (end-to-end): a same-artifact Update on dotnet_api must
// reach CONFIGURE without re-staging the artifact.
func TestUpdateReconfiguresDotnetAPI(t *testing.T) {
	payload := []byte("api bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	d := dotnetAPISpec(t, url, sum)
	if _, err := eng.Deploy(context.Background(), d); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	f.log = nil
	if _, err := eng.Update(context.Background(), d, d); err != nil {
		t.Fatalf("update: %v\nlog=%v", err, f.log)
	}
	order := strings.Join(f.log, ">")
	if !strings.Contains(order, "CONFIGURE") {
		t.Fatalf("same-artifact dotnet_api Update must reconfigure (run CONFIGURE), got %v", f.log)
	}
	if strings.Contains(order, "FETCH") {
		t.Fatalf("same-artifact dotnet_api Update must not re-stage, got %v", f.log)
	}
}

// Evaluator iter2 item 2: node dependency installation must run for CACHED
// releases too. A cached release previously deployed with install_deps=false
// must still run `npm ci` (and validate package-lock.json) when this apply
// flips install_deps=true — InstallDeps cannot live only on the fresh-extract
// path.
func TestCachedNodeReleaseStillInstallsDeps(t *testing.T) {
	payload := []byte("node zip")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	// Pre-seed a matching release marker so releaseCached() returns true: the
	// artifact is already on disk (a prior install_deps=false deploy), so this
	// apply hits the cached path and skips fetch/extract.
	marker := `C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`
	f.files[marker] = []byte(fmt.Sprintf("{\n  \"version\": \"1.0.0\",\n  \"sha256\": %q,\n  \"extracted_at\": \"x\"\n}\n", sum))

	if _, err := eng.Deploy(context.Background(), nodeWebSpec(t, url, sum, true)); err != nil {
		t.Fatalf("deploy: %v\nlog=%v", err, f.log)
	}
	joined := strings.Join(f.log, ">")
	if strings.Contains(joined, "EXTRACT") {
		t.Fatalf("cached node deploy must skip extraction; log=%v", f.log)
	}
	if !strings.Contains(joined, "NPMCI") {
		t.Fatalf("cached node deploy with install_deps=true must still run npm ci; log=%v", f.log)
	}
}

// Fresh (non-cached) node deploy with install_deps=false must NOT run npm ci.
func TestFreshNodeReleaseSkipsInstallDepsWhenDisabled(t *testing.T) {
	payload := []byte("node zip 2")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	if _, err := eng.Deploy(context.Background(), nodeWebSpec(t, url, sum, false)); err != nil {
		t.Fatalf("deploy: %v\nlog=%v", err, f.log)
	}
	joined := strings.Join(f.log, ">")
	if !strings.Contains(joined, "EXTRACT") {
		t.Fatalf("fresh node deploy must extract; log=%v", f.log)
	}
	if strings.Contains(joined, "NPMCI") {
		t.Fatalf("install_deps=false must not run npm ci; log=%v", f.log)
	}
}

// A cached-release npm ci failure must propagate as ERR_SERVICE_INSTALL, proving
// the cached path actually gates on the install result (not a silent skip).
func TestCachedNodeInstallDepsFailurePropagates(t *testing.T) {
	payload := []byte("node zip 3")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	f.fail["npmci"] = true
	eng := engineWith(f)
	marker := `C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`
	f.files[marker] = []byte(fmt.Sprintf("{\n  \"version\": \"1.0.0\",\n  \"sha256\": %q,\n  \"extracted_at\": \"x\"\n}\n", sum))

	_, err := eng.Deploy(context.Background(), nodeWebSpec(t, url, sum, true))
	if err == nil {
		t.Fatalf("cached npm ci failure must fail the deploy; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "ERR_SERVICE_INSTALL") {
		t.Fatalf("cached npm ci failure must map to ERR_SERVICE_INSTALL, got: %v", err)
	}
}

// Evaluator iter3 item 1: an install_deps false→true Update on an existing
// HEALTHY manifest must actually run `npm ci` — the toggle used to be swallowed
// by Deploy's unchanged-artifact idempotency short-circuit. It is now routed
// through Reconfigure, which re-runs InstallDeps at the pre-activation point.
func TestUpdateToggleInstallDepsRunsNpmCi(t *testing.T) {
	payload := []byte("node zip toggle")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	// First deploy with install_deps=false ⇒ healthy manifest, node_modules NOT
	// installed (no npm ci).
	prior := nodeWebSpec(t, url, sum, false)
	if _, err := eng.Deploy(context.Background(), prior); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	if strings.Contains(strings.Join(f.log, ">"), "NPMCI") {
		t.Fatalf("install_deps=false first deploy must not run npm ci; log=%v", f.log)
	}
	f.log = nil

	// Update flips install_deps=true on the SAME artifact. This must NOT be a
	// silent no-op: npm ci has to execute.
	next := nodeWebSpec(t, url, sum, true)
	if _, err := eng.Update(context.Background(), next, prior); err != nil {
		t.Fatalf("update: %v\nlog=%v", err, f.log)
	}
	order := strings.Join(f.log, ">")
	if strings.Contains(order, "FETCH") {
		t.Fatalf("same-artifact install_deps toggle must not re-stage the artifact; log=%v", f.log)
	}
	if !strings.Contains(order, "NPMCI") {
		t.Fatalf("install_deps false→true Update must run npm ci; log=%v", f.log)
	}
	// npm ci must run while the service is stopped (after STOP, before START).
	iStop := strings.Index(order, "STOP")
	iNpm := strings.Index(order, "NPMCI")
	iStart := strings.Index(order, "START")
	if !(iStop >= 0 && iStop < iNpm && iNpm < iStart) {
		t.Fatalf("npm ci must run after STOP and before START; order=%v", f.log)
	}
	// The active node_modules is snapshotted before npm ci and, on success, the
	// snapshot is discarded (rollback-safety, not a leak).
	if !strings.Contains(order, "DEPBACKUP") {
		t.Fatalf("npm ci must be preceded by a node_modules snapshot; log=%v", f.log)
	}
	if !strings.Contains(order, "DEPCOMMIT") {
		t.Fatalf("successful reconfigure must discard the node_modules snapshot; log=%v", f.log)
	}
	if strings.Contains(order, "DEPRESTORE") {
		t.Fatalf("successful reconfigure must NOT restore the snapshot; log=%v", f.log)
	}
}

// Evaluator iter4 item 1: a failed `npm ci` during Node Reconfigure must NOT
// corrupt the active release. The engine snapshots node_modules before npm ci
// and, on failure, restores it BEFORE restoring the prior service configuration
// so the prior app remains runnable.
func TestReconfigureNpmCiFailureRestoresDeps(t *testing.T) {
	payload := []byte("node zip npmfail")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	// Deploy with install_deps=true so a real dependency tree exists to protect.
	prior := nodeWebSpec(t, url, sum, true)
	if _, err := eng.Deploy(context.Background(), prior); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	// Break npm ci, then push a same-artifact config change so Reconfigure runs
	// and re-invokes npm ci (which now fails mid-install and leaves a "partial"
	// tree). Capture the ORIGINAL dependency-tree token so we can prove the exact
	// tree is restored (not merely that some restore command ran).
	origTree := f.nm
	if origTree == "" || origTree == "partial" {
		t.Fatalf("precondition: healthy deploy must leave a good node_modules tree, got %q", origTree)
	}
	f.fail["npmci"] = true
	next := nodeWebSpec(t, url, sum, true)
	next.Pattern.NodeExe = `C:\node20\node.exe`
	_, err := eng.Update(context.Background(), next, prior)
	if err == nil {
		t.Fatalf("Update must fail when npm ci fails; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "ERR_SERVICE_INSTALL") {
		t.Fatalf("npm ci failure must surface ERR_SERVICE_INSTALL, got: %v", err)
	}
	order := strings.Join(f.log, ">")
	if !strings.Contains(order, "DEPBACKUP") {
		t.Fatalf("npm ci must be preceded by a node_modules snapshot; log=%v", f.log)
	}
	if !strings.Contains(order, "DEPRESTORE") {
		t.Fatalf("failed npm ci must restore the node_modules snapshot; log=%v", f.log)
	}
	iBackup := strings.Index(order, "DEPBACKUP")
	iRestore := strings.Index(order, "DEPRESTORE")
	if !(iBackup >= 0 && iBackup < iRestore) {
		t.Fatalf("snapshot must be taken before it is restored; order=%v", f.log)
	}
	// The prior app must be restarted only AFTER node_modules is restored, so it
	// comes up against its original dependency tree.
	iLastStart := strings.LastIndex(order, "START")
	if !(iRestore < iLastStart) {
		t.Fatalf("node_modules must be restored before the prior app restarts; order=%v", f.log)
	}
	// PROVE the exact original tree is back on disk and the snapshot is consumed.
	if f.nm != origTree {
		t.Fatalf("original node_modules tree must be restored; want %q got %q", origTree, f.nm)
	}
	if f.nmBak != "" {
		t.Fatalf("snapshot must be consumed by restore, still present: %q", f.nmBak)
	}
	// The prior service must actually be Running (start succeeded because deps are
	// good), not merely reported restored.
	if f.svc != "Running" {
		t.Fatalf("prior app must be running after restore; svc=%q", f.svc)
	}
	if !strings.Contains(err.Error(), "prior configuration restored") {
		t.Fatalf("rollback must restore the prior configuration healthy; got: %v", err)
	}
}

// A failed node_modules BACKUP must ABORT before the destructive npm ci runs:
// the active tree is never touched, so the prior app stays runnable.
func TestReconfigureBackupFailureAbortsBeforeNpmCi(t *testing.T) {
	payload := []byte("node zip bakfail")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	prior := nodeWebSpec(t, url, sum, true)
	if _, err := eng.Deploy(context.Background(), prior); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	origTree := f.nm
	f.log = nil
	f.fail["depbackup"] = true
	next := nodeWebSpec(t, url, sum, true)
	next.Pattern.NodeExe = `C:\node20\node.exe`
	_, err := eng.Update(context.Background(), next, prior)
	if err == nil {
		t.Fatalf("Update must fail when node_modules backup fails; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "ERR_SERVICE_INSTALL") {
		t.Fatalf("backup failure must surface ERR_SERVICE_INSTALL, got: %v", err)
	}
	order := strings.Join(f.log, ">")
	// A failed backup must NOT be followed by the destructive npm ci.
	if strings.Contains(order, "NPMCI") {
		t.Fatalf("destructive npm ci must NOT run after a failed backup; log=%v", f.log)
	}
	// The active tree is untouched — the prior app can still run.
	if f.nm != origTree {
		t.Fatalf("active node_modules must be untouched on backup failure; want %q got %q", origTree, f.nm)
	}
}

// A failed node_modules RESTORE must escalate to ERR_ROLLBACK_FAILED — the
// engine must NOT report the prior configuration as healthy when the dependency
// tree is left broken (evaluator iter5 item 2).
func TestReconfigureRestoreFailureIsRollbackFailed(t *testing.T) {
	payload := []byte("node zip restfail")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	prior := nodeWebSpec(t, url, sum, true)
	if _, err := eng.Deploy(context.Background(), prior); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	f.log = nil
	// npm ci fails (tree becomes partial) AND the restore of the snapshot fails.
	f.fail["npmci"] = true
	f.fail["deprestore"] = true
	next := nodeWebSpec(t, url, sum, true)
	next.Pattern.NodeExe = `C:\node20\node.exe`
	_, err := eng.Update(context.Background(), next, prior)
	if err == nil {
		t.Fatalf("Update must fail when restore fails; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "ERR_ROLLBACK_FAILED") {
		t.Fatalf("a failed dependency restore must surface ERR_ROLLBACK_FAILED, got: %v", err)
	}
	// The engine must NOT falsely claim the prior configuration is healthy.
	if strings.Contains(err.Error(), "prior configuration restored") {
		t.Fatalf("must not report prior config restored when restore failed; got: %v", err)
	}
	order := strings.Join(f.log, ">")
	// The prior service must NOT be started against the broken tree: no START runs
	// after the failed restore (the last-restore-then-start coupling is severed).
	if strings.Contains(order, "DEPRESTORE") {
		t.Fatalf("a FAILED restore must not be marked as a successful DEPRESTORE; log=%v", f.log)
	}
	// The broken tree is left in place for manual repair.
	if f.nm != "partial" {
		t.Fatalf("unrecoverable tree must remain partial for repair; got %q", f.nm)
	}
}

// Evaluator iter3 item 2: Reconfigure MUST run the pattern preflight before any
// mutation. A same-artifact node_exe change whose new tooling fails preflight
// must abort BEFORE the service is stopped/reconfigured.
func TestReconfigureRunsNodePreflightBeforeMutation(t *testing.T) {
	payload := []byte("node zip preflight")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	prior := nodeWebSpec(t, url, sum, false)
	if _, err := eng.Deploy(context.Background(), prior); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	f.log = nil
	// Now the target's node tooling is broken: a same-artifact node_exe change must
	// fail at PREFLIGHT before STOP/CONFIGURE touch the running service.
	f.fail["nodepreflight"] = true
	next := nodeWebSpec(t, url, sum, false)
	next.Pattern.NodeExe = `C:\node20\node.exe`
	_, err := eng.Update(context.Background(), next, prior)
	if err == nil {
		t.Fatalf("Update must fail when node preflight fails; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "ERR_PREFLIGHT") {
		t.Fatalf("failed node preflight must surface ERR_PREFLIGHT, got: %v", err)
	}
	order := strings.Join(f.log, ">")
	if strings.Contains(order, "STOP") || strings.Contains(order, "CONFIGURE") {
		t.Fatalf("preflight must run BEFORE any mutation (no STOP/CONFIGURE); log=%v", f.log)
	}
}

// dotnet_api Reconfigure must likewise run its aspnet-runtime preflight before
// mutating: a launcher=exe→dotnet_dll change on a box missing the runtime aborts
// before the service is stopped.
func TestReconfigureRunsDotnetPreflightBeforeMutation(t *testing.T) {
	payload := []byte("api preflight")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	prior := dotnetAPISpec(t, url, sum) // launcher=exe (no runtime probe)
	if _, err := eng.Deploy(context.Background(), prior); err != nil {
		t.Fatalf("first deploy: %v\nlog=%v", err, f.log)
	}
	f.log = nil
	f.fail["dotnetpreflight"] = true
	// Switch to launcher=dotnet_dll — now the aspnet-runtime probe runs and fails.
	next := dotnetAPISpec(t, url, sum)
	next.Pattern.Launcher = "dotnet_dll"
	next.Pattern.Exe = ""
	next.Pattern.DLL = "SampleApi.dll"
	_, err := eng.Update(context.Background(), next, prior)
	if err == nil {
		t.Fatalf("Update must fail when dotnet preflight fails; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "ERR_PREFLIGHT") {
		t.Fatalf("failed dotnet preflight must surface ERR_PREFLIGHT, got: %v", err)
	}
	if strings.Contains(strings.Join(f.log, ">"), "STOP") {
		t.Fatalf("dotnet preflight must run BEFORE STOP; log=%v", f.log)
	}
}

