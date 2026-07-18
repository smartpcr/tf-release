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

	// Toggling install_deps requires the STAGE-phase npm ci, which Reconfigure
	// does not run, so it must fall through to Deploy.
	prior := nodeWebSpec(t, u, sum, false)
	next := nodeWebSpec(t, u, sum, true)
	if reconfigurable(next, prior) {
		t.Error("install_deps toggle must fall through to Deploy (needs npm ci)")
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
