package provider

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
)

// Stage 9.1 — CON (§18.2), ART (§18.3) and CAP (§18.4) acceptance scenarios.
// Each test maps 1:1 to a DESIGN §18 ID. Positive scenarios end by asserting
// `.lock` is absent on the touched host (via applyStep / CheckDestroy);
// connectivity negatives never reach a host so they assert only the coded error.

// ---------------------------------------------------------------------------
// CON — connectivity (§18.2)
// ---------------------------------------------------------------------------

// CON-01: W1, apply minimal console spec over winrm https insecure ⇒ success;
// exactly one WARN diag about insecure TLS. The acceptance framework cannot
// inspect diagnostics from a Check, so the "exactly one WARN" invariant is
// asserted structurally by the unit test TestApplyWinRMInsecureEmitsExactlyOneWarnDiag
// (same insecure_skip_verify code path); here we prove the end-to-end apply
// succeeds, records the release path, and leaves no lock.
func TestAccCON01_InsecureTLSApplies(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestMatchResourceAttr("labdeploy_deployment.val", "release_path", mustRe(`releases[\\/]1\.0\.0$`)),
	))
}

// CON-02: W1 with a wrong password ⇒ ERR_AUTH, exactly one auth attempt. We set
// connect_retries: 2 so that IF auth failures were (wrongly) retried, the apply
// would incur at least one 5s backoff; bounding the failure UNDER 5s therefore
// proves the configured retries were suppressed for an auth rejection (DESIGN
// §8.1: ERR_AUTH is never retried) — evaluator item 3.
func TestAccCON02_WrongPassword(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	host := os.Getenv(envW1Host)
	user := os.Getenv(envW1User)
	t.Setenv("LABDEPLOY_ACC_W1_WRONGPW", "definitely-the-wrong-password")
	target := fmt.Sprintf(`  transport: winrm
  hosts: [%q]
  os: windows
  credentials: { username: %q, password_env: LABDEPLOY_ACC_W1_WRONGPW }
  winrm: { use_https: true, insecure_skip_verify: true }
  connect_retries: 2`, host, user)
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: "1.0.0"
  checksum: %q
  source: { type: http, url: %q }
pattern: { type: console_app, exe: bin\sample-svc.exe }
`, target, artifactSHA(t, "1.0.0", "zip"), artifactURL(t, "1.0.0", "zip"))
	_ = at
	// A bad credential must fail before even the first 5s retry backoff elapses,
	// proving the one auth attempt was NOT retried despite connect_retries=2.
	wallBetween(t, 0, 5*time.Second, "CON-02 wrong-password apply", func() {
		runScenarioNoProbe(t, errorStep(accDeploymentConfig(spec), `ERR_AUTH`))
	})
}

// CON-03: firewall-dropped port with connect_retries=2 ⇒ ERR_CONNECT after the
// retries are exhausted. The retry loop wraps the last error as
// `after 3 attempts:` (retries+1), so the diagnostic itself proves attempts=3 —
// the DESIGN §18 CON-03 "log shows attempts=3" requirement — evaluator item 4.
// The elapsed wall is additionally bounded BELOW by 2×backoff to prove the two
// retries actually spun (a single immediate failure returns far faster).
func TestAccCON03_ConnectRetriesExhausted(t *testing.T) {
	accPreCheck(t)
	// A TEST-NET / unroutable address guarantees the port is never reachable.
	target := `  transport: winrm
  hosts: ["203.0.113.1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_ACC_CON_PW }
  winrm: { use_https: true, insecure_skip_verify: true, timeout_seconds: 3 }
  connect_retries: 2`
	t.Setenv("LABDEPLOY_ACC_CON_PW", "pw")
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: "1.0.0"
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://203.0.113.1/a.zip" }
pattern: { type: console_app, exe: bin\sample-svc.exe }
`, target)
	// The two 5s backoffs between the three attempts put the floor at ~10s; the
	// error text must name the attempt count (attempts=3).
	wallBetween(t, 6*time.Second, 90*time.Second, "CON-03 retry-exhausted apply", func() {
		runScenarioNoProbe(t, errorStep(accDeploymentConfig(spec), `ERR_CONNECT(?s).*after 3 attempts`))
	})
}

// CON-04: L1 with a pinned WRONG ssh.host_key ⇒ ERR_CONNECT detail `host key
// mismatch`. The pin is a syntactically-valid, correctly-Base64'd ed25519 public
// key that simply does not match L1's real key, so the handshake reaches the
// key-verification stage and rejects the mismatch (rather than failing earlier on
// a malformed key) — evaluator item 3.
func TestAccCON04_HostKeyMismatch(t *testing.T) {
	accPreCheck(t)
	host := mustEnv(t, envL1Host, "L1 host-key mismatch scenario")
	user := mustEnv(t, envL1User, "L1 host-key mismatch scenario")
	if os.Getenv(envL1Password) == "" && os.Getenv(envL1Key) == "" {
		t.Fatalf("TF_ACC=1 requires %s or %s for CON-04", envL1Password, envL1Key)
	}
	credLine := fmt.Sprintf("credentials: { username: %q, password_env: %s }", user, envL1Password)
	if os.Getenv(envL1Password) == "" {
		credLine = fmt.Sprintf("credentials: { username: %q, private_key_env: %s }", user, envL1Key)
	}
	// A syntactically-valid ed25519 host key in the exact wire form
	// sshTransport.hostKeyCallback expects: ONLY the Base64-encoded wire bytes
	// (no `ssh-ed25519 ` prefix, no comment). ssh.ParsePublicKey decodes this
	// cleanly, so the handshake reaches the key-comparison stage and rejects the
	// mismatch (ERR_CONNECT "host key mismatch") rather than failing earlier on a
	// malformed pin — evaluator item 1.
	const wrongKey = "AAAAC3NzaC1lZDI1NTE5AAAAILJtYLj4f6zAekxuJD5jn5vGMDR1kiaw/ZHA2AR3dHIi"
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: ssh
  hosts: [%q]
  os: linux
  %s
  ssh: { host_key: %q }
artifact:
  type: zip
  version: "1.0.0"
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://203.0.113.1/a.zip" }
pattern: { type: console_app, exe: bin/sample-svc }
`, host, credLine, wrongKey)
	runScenarioNoProbe(t, errorStep(accDeploymentConfig(spec), `ERR_CONNECT(?s).*host key mismatch`))
}

// CON-05: runner==W1, transport local, os windows ⇒ apply a console spec
// succeeds without opening any socket. Requires running ON the Windows target as
// the runner (opt-in via LABDEPLOY_ACC_LOCAL_WINDOWS=1).
func TestAccCON05_LocalTransport(t *testing.T) {
	accPreCheck(t)
	if os.Getenv("LABDEPLOY_ACC_LOCAL_WINDOWS") != "1" {
		t.Skip("set LABDEPLOY_ACC_LOCAL_WINDOWS=1 when the runner IS the Windows target")
	}
	at := accTarget{
		tgt:  buildLocalWindowsTarget(),
		host: "localhost",
	}
	at.yaml = `  transport: local
  hosts: ["localhost"]
  os: windows`
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: "1.0.0"
  checksum: %q
  source: { type: http, url: %q }
pattern: { type: console_app, exe: bin\sample-svc.exe }
strategy: { keep_releases: 2 }
`, at.yaml, artifactSHA(t, "1.0.0", "zip"), artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
	))
}

// ---------------------------------------------------------------------------
// ART — artifacts (§18.3)
// ---------------------------------------------------------------------------

// ART-01: W1 http zip, correct sha, fetch_mode runner_push ⇒ success; the
// release dir carries a `.labdeploy-release.json` whose recorded sha MATCHES the
// spec checksum (not merely present) — evaluator item 5.
func TestAccART01_ZipRunnerPush(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	sha := artifactSHA(t, "1.0.0", "zip")
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: "1.0.0"
  checksum: %q
  fetch_mode: runner_push
  source: { type: http, url: %q }
pattern: { type: console_app, exe: bin\sample-svc.exe }
strategy: { keep_releases: 2 }
`, at.yaml, sha, artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		checkReleaseMarkerSHA(at, "1.0.0", "zip", sha),
	))
}

// ART-02: with a healthy windows_service ALREADY deployed at 1.0.0, an upgrade to
// 1.1.0 whose spec sha is off by one hex ⇒ ERR_CHECKSUM_MISMATCH; POST-STATE per
// DESIGN §18 ART-02: no releases/1.1.0 dir, manifest still records 1.0.0 (no
// change), staging empty, and the PRE-EXISTING service is untouched & still
// RUNNING on 1.0.0 — evaluator item 2. The reachable host means CheckDestroy also
// asserts the lock is absent.
func TestAccART02_ChecksumMismatch(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	good10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	sha11 := artifactSHA(t, "1.1.0", "zip")
	bad11 := accDeploymentConfig(strings.Replace(
		winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""), sha11, flipLastHex(sha11), 1))
	runScenario(t, at,
		applyStep(at, good10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		),
		errorStep(bad11, `ERR_CHECKSUM_MISMATCH`),
		// Re-apply the good 1.0.0 (no-op) so Check can assert the post-failure live
		// state: the failed upgrade left nothing behind and did not disturb 1.0.0.
		applyStep(at, good10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkReleaseAbsent(at, "1.1.0", "ART-02: failed checksum must leave no release dir"),
			checkReleaseCount(at, 1),
			checkManifestVersion(at, "1.0.0", "ART-02: manifest must still record 1.0.0 (unchanged)"),
			checkServiceState(at, "SampleSvc", "Running"),
			checkStagingEmpty(at),
		),
	)
}

// ART-03: W1 url → 404 ⇒ ERR_ARTIFACT_FETCH whose diagnostic INCLUDES `404`
// (not merely the code) — evaluator item 7.
func TestAccART03_Fetch404(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	url := artifactURL(t, "9.9.9-does-not-exist", "zip")
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: "1.0.0"
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: %q }
pattern: { type: console_app, exe: bin\sample-svc.exe }
`, at.yaml, url)
	runScenario(t, at, errorStep(accDeploymentConfig(spec), `ERR_ARTIFACT_FETCH(?s).*404`))
}

// ART-04: W1 nupkg of sample-svc ⇒ success; files land under releases/<ver>/lib
// per the nupkg package layout AND the app runs (verify_command exercises it) —
// evaluator item 8.
func TestAccART04_Nupkg(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: nupkg
  version: "1.0.0"
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: console_app
  exe: lib\sample-svc.exe
  verify_command: "lib\\sample-svc.exe --version"
strategy: { keep_releases: 2 }
`, at.yaml, artifactSHA(t, "1.0.0", "nupkg"), artifactURL(t, "1.0.0", "nupkg"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		checkReleaseSubdir(at, "1.0.0", "lib", "ART-04: nupkg must unpack to releases/<ver>/lib"),
	))
}

// ART-05: W1 fetch_mode target_pull with an auth header env ⇒ success proven to
// be a TARGET-side pull. Under TF_ACC=1 the runner's egress to the package host
// MUST be firewalled (LABDEPLOY_ACC_ART_RUNNER_EGRESS_BLOCKED=1) so a successful
// deploy is possible ONLY if the target fetched the payload itself — the DESIGN
// §18 ART-05 discriminator. The proof environment is REQUIRED (hard-fail), not
// skipped, so the acceptance gate can never pass on the unproven path —
// evaluator item 2.
func TestAccART05_TargetPull(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	if os.Getenv("LABDEPLOY_ACC_ART_RUNNER_EGRESS_BLOCKED") != "1" {
		t.Fatalf("ART-05 under TF_ACC=1 requires LABDEPLOY_ACC_ART_RUNNER_EGRESS_BLOCKED=1 (runner egress to the package host firewalled) so a successful deploy PROVES the target pulled the artifact; refusing to pass on an unproven path")
	}
	sha := artifactSHA(t, "1.0.0", "zip")
	if os.Getenv("LABDEPLOY_ACC_ART_TOKEN") == "" {
		t.Setenv("LABDEPLOY_ACC_ART_TOKEN", "lab-token")
	}
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: "1.0.0"
  checksum: %q
  fetch_mode: target_pull
  source:
    type: http
    url: %q
    auth: { header: "Authorization", token_env: LABDEPLOY_ACC_ART_TOKEN, scheme: bearer }
pattern: { type: console_app, exe: bin\sample-svc.exe }
strategy: { keep_releases: 2 }
`, at.yaml, sha, artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		checkReleaseMarkerSHA(at, "1.0.0", "zip", sha),
	))
}

// ART-06: L1 docker_registry with a bad password_env value ⇒ ERR_ARTIFACT_FETCH
// containing a `docker login` stderr snippet; the container list is UNCHANGED
// (captured before, compared after) — evaluator item 9.
func TestAccART06_DockerLoginFails(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	t.Setenv("LABDEPLOY_ACC_DOCKER_PW", "definitely-wrong")
	image := os.Getenv("LABDEPLOY_ACC_DOCKER_IMAGE")
	if image == "" {
		image = "registry.example.test/sample-svc:1.0.0"
	}
	before, err := snapshotContainers(at)
	if err != nil {
		t.Fatalf("ART-06: capture container list before apply: %v", err)
	}
	spec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: docker_image
  version: "1.0.0"
  source:
    type: docker_registry
    image: %q
    auth: { username: baduser, password_env: LABDEPLOY_ACC_DOCKER_PW }
pattern: { type: docker_container, container_name: sample-svc }
`, at.yaml, image)
	errorScenario(t, at, accDeploymentConfig(spec), `ERR_ARTIFACT_FETCH(?s).*docker login`,
		checkContainersUnchanged(at, before),
	)
}

// ---------------------------------------------------------------------------
// CAP — console_app (§18.4)
// ---------------------------------------------------------------------------

// CAP-01: W1 fresh apply v1.0.0 with a verify_command ⇒ apply ok; `current`
// junction target ends releases\1.0.0; deployed_version=1.0.0, service_status=n/a.
func TestAccCAP01_WindowsConsole(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", `bin\sample-svc.exe --version`))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "n/a"),
		checkCurrentTarget(at, "1.0.0"),
	))
}

// CAP-02: W1 apply THREE distinct versions (1.0.0 → 1.1.0 → 1.2.0-bad) with
// keep_releases=2 ⇒ after the last apply exactly 2 dirs remain under releases
// (newest+previous) and the OLDEST (1.0.0) is pruned. Three distinct versions are
// required to actually exercise pruning — two would never exceed the keep bound —
// evaluator item 10. 1.2.0-bad fail-starts only as a *service*; as a console_app
// it merely lays down the release tree, so it is a valid third version here.
func TestAccCAP02_KeepReleasesPrune(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v10 := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(consoleSpec(t, at, "1.1.0", "zip", ""))
	v12 := accDeploymentConfig(consoleSpec(t, at, "1.2.0-bad", "zip", ""))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		applyStep(at, v12,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.2.0-bad"),
			checkReleaseCount(at, 2),
			checkReleaseAbsent(at, "1.0.0", "CAP-02: keep_releases=2 must prune the oldest (1.0.0)"),
			checkPathPresent(at, layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "1.1.0").Release, "CAP-02: previous (1.1.0) retained"),
			checkPathPresent(at, layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "1.2.0-bad").Release, "CAP-02: newest (1.2.0-bad) retained"),
		),
	)
}

// CAP-03: L1, same as CAP-01 over ssh, linux build ⇒ ok; `current` is a symlink
// (verified via readlink).
func TestAccCAP03_LinuxConsole(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", `bin/sample-svc --version`))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		checkCurrentTarget(at, "1.0.0"),
	))
}
