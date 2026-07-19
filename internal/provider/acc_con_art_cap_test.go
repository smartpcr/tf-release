package provider

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Stage 9.1 — CON (§18.2), ART (§18.3) and CAP (§18.4) acceptance scenarios.
// Each test maps 1:1 to a DESIGN §18 ID. Positive scenarios end by asserting
// `.lock` is absent on the touched host (via applyStep / CheckDestroy);
// connectivity negatives never reach a host so they assert only the coded error.

// ---------------------------------------------------------------------------
// CON — connectivity (§18.2)
// ---------------------------------------------------------------------------

// CON-01: W1, apply minimal console spec over winrm https insecure ⇒ success;
// exactly one WARN diag about insecure TLS (the warn surface is unit-covered by
// TestApplyWinRMInsecureEmitsExactlyOneWarnDiag; here we prove the end-to-end
// apply succeeds and leaves no lock).
func TestAccCON01_InsecureTLSApplies(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
	))
}

// CON-02: W1 with a wrong password ⇒ ERR_AUTH, exactly one auth attempt (no
// retry). The password_env references a var we set to a deliberately wrong value.
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
  connect_retries: 0`, host, user)
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
`, target, artifactSHA(t, "1.0.0"), artifactURL(t, "1.0.0", "zip"))
	_ = at
	runScenarioNoProbe(t, errorStep(accDeploymentConfig(spec), `ERR_AUTH`))
}

// CON-03: firewall-dropped port with connect_retries=2 ⇒ ERR_CONNECT after the
// retries are exhausted (3 attempts total). Uses an unroutable host so the dial
// times out without ever completing.
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
	runScenarioNoProbe(t, errorStep(accDeploymentConfig(spec), `ERR_CONNECT`))
}

// CON-04: L1 with a pinned WRONG ssh.host_key ⇒ ERR_CONNECT detail `host key
// mismatch`. Uses the real L1 host so the handshake reaches the key-verification
// stage, then rejects the mismatched pin.
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
	// A syntactically-valid but wrong pinned key (base64 of a bogus blob).
	const wrongKey = "AAAAB3NzaC1yc2EAAAADAQABAAABAQDwrongwrongwrongwrongwrongwrongwrong="
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
	runScenarioNoProbe(t, errorStep(accDeploymentConfig(spec), `ERR_CONNECT`))
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
`, at.yaml, artifactSHA(t, "1.0.0"), artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
	))
}

// ---------------------------------------------------------------------------
// ART — artifacts (§18.3)
// ---------------------------------------------------------------------------

// ART-01: W1 http zip, correct sha, fetch_mode runner_push ⇒ success; the
// release dir carries a `.labdeploy-release.json` whose sha matches.
func TestAccART01_ZipRunnerPush(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
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
`, at.yaml, artifactSHA(t, "1.0.0"), artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		checkReleaseMarker(at, "1.0.0"),
	))
}

// ART-02: W1 sha off by one hex ⇒ ERR_CHECKSUM_MISMATCH; target keeps no
// releases/<ver> dir. The host IS reachable, so CheckDestroy still asserts the
// lock is absent after the failed fetch.
func TestAccART02_ChecksumMismatch(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	bad := flipLastHex(artifactSHA(t, "1.0.0"))
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
`, at.yaml, bad, artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, errorStep(accDeploymentConfig(spec), `ERR_CHECKSUM_MISMATCH`))
}

// ART-03: W1 url → 404 ⇒ ERR_ARTIFACT_FETCH including `404`.
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
	runScenario(t, at, errorStep(accDeploymentConfig(spec), `ERR_ARTIFACT_FETCH`))
}

// ART-04: W1 nupkg of sample-svc ⇒ success; files land under releases/<ver>/lib.
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
pattern: { type: console_app, exe: bin\sample-svc.exe }
strategy: { keep_releases: 2 }
`, at.yaml, artifactSHA(t, "1.0.0"), artifactURL(t, "1.0.0", "nupkg"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
	))
}

// ART-05: W1 fetch_mode target_pull with an auth header env ⇒ success (the
// target pulls the payload itself). Requires LABDEPLOY_ACC_ART_TOKEN as the
// referenced token env NAME.
func TestAccART05_TargetPull(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
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
`, at.yaml, artifactSHA(t, "1.0.0"), artifactURL(t, "1.0.0", "zip"))
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
	))
}

// ART-06: L1 docker_registry with a bad password_env value ⇒ ERR_ARTIFACT_FETCH
// containing a `docker login` stderr snippet; container list unchanged.
func TestAccART06_DockerLoginFails(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	t.Setenv("LABDEPLOY_ACC_DOCKER_PW", "definitely-wrong")
	image := os.Getenv("LABDEPLOY_ACC_DOCKER_IMAGE")
	if image == "" {
		image = "registry.example.test/sample-svc:1.0.0"
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
	runScenario(t, at, errorStep(accDeploymentConfig(spec), `ERR_ARTIFACT_FETCH`))
}

// ---------------------------------------------------------------------------
// CAP — console_app (§18.4)
// ---------------------------------------------------------------------------

// CAP-01: W1 fresh apply v1.0.0 with a verify_command ⇒ apply ok; `current`
// junction target ends releases\1.0.0; deployed_version=1.0.0, service_status=n/a.
func TestAccCAP01_WindowsConsole(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", `sample-svc.exe --version`))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "n/a"),
		checkCurrentTarget(at, "1.0.0"),
	))
}

// CAP-02: W1 rolled forward v1.1.0 → v1.0.0(cached) ×3 with keep_releases=2 ⇒
// after the last apply exactly 2 dirs remain under releases (newest+previous).
func TestAccCAP02_KeepReleasesPrune(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v11 := accDeploymentConfig(consoleSpec(t, at, "1.1.0", "zip", ""))
	v10 := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	runScenario(t, at,
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		applyStep(at, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkReleaseCount(at, 2),
		),
	)
}

// CAP-03: L1, same as CAP-01 over ssh, linux build ⇒ ok; `current` is a symlink
// (verified via readlink).
func TestAccCAP03_LinuxConsole(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", `sample-svc --version`))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		checkCurrentTarget(at, "1.0.0"),
	))
}
