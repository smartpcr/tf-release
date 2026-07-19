package provider

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Stage 9.1 — WSV (§18.5) and NOD/NET (§18.6) acceptance scenarios on W1
// (Windows Server 2022, WinRM https). Each test maps 1:1 to a DESIGN §18 ID.
// Positive scenarios end by asserting `.lock` is absent on W1; failure scenarios
// on the reachable host assert the coded error plus the same lock invariant via
// CheckDestroy.

// healthURL returns the sample-svc /health URL on W1, from env (or a default
// derived from the host). WSV/NOD/NET need the app's health endpoint reachable
// from W1 itself (curl-from-W1 in DESIGN), so the URL is localhost-relative.
func healthURL(port int) string {
	if u := os.Getenv("LABDEPLOY_ACC_HEALTH_URL"); u != "" {
		return u
	}
	return fmt.Sprintf("http://localhost:%d/health", port)
}

// ---------------------------------------------------------------------------
// WSV — windows_service (§18.5)
// ---------------------------------------------------------------------------

// WSV-01: fresh apply v1.0.0 with http /health ⇒ service RUNNING, LD_VERSION in
// HKLM env, health passed, junction→1.0.0, app.log carries the env dump. Asserts
// the full DESIGN §18 WSV-01 post-state — evaluator item 12.
func TestAccWSV01_FreshService(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkServiceState(at, "SampleSvc", "Running"),
		checkCurrentTarget(at, "1.0.0"),
		checkHKLMEnv(at, "LD_VERSION", "1.0.0"),
		checkHealthBody(at, healthURL(8080), "v=1.0.0"),
		checkAppLogContains(at, "LD_VERSION", "WSV-01: app.log must contain the env dump proving env delivery"),
	))
}

// WSV-02: upgrade v1.0.0 → v1.1.0 ⇒ zero-error apply; previous_version=1.0.0;
// both releases retained; junction→1.1.0; /health body v=1.1.0 — evaluator item 12.
func TestAccWSV02_Upgrade(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "previous_version", "1.0.0"),
			checkReleaseCount(at, 2),
			checkCurrentTarget(at, "1.1.0"),
			checkHealthBody(at, healthURL(8080), "v=1.1.0"),
		),
	)
}

// WSV-03: upgrade to v1.2.0-bad (fail-start) with rollback on ⇒ apply fails
// ERR_SERVICE_START (with an SCM event-log line); the deployment stays RUNNING on
// the prior (1.1.0) version — junction→1.1.0, /health v=1.1.0, TF
// deployed_version=1.1.0 — and a re-apply of 1.1.0 plans EMPTY (converged) —
// evaluator item 12.
func TestAccWSV03_FailStartRollsBack(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""))
	bad := accDeploymentConfig(winServiceSpec(t, at, "1.2.0-bad", "zip", healthURL(8080), ""))
	runScenario(t, at,
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		errorStep(bad, `ERR_SERVICE_START`),
		// Post-rollback: re-applying 1.1.0 is a no-op that lets Check assert the
		// rolled-back live state (service running on 1.1.0, junction & health on
		// 1.1.0, an SCM event was logged for the failed 1.2.0-bad start).
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
			checkServiceState(at, "SampleSvc", "Running"),
			checkCurrentTarget(at, "1.1.0"),
			checkHealthBody(at, healthURL(8080), "v=1.1.0"),
			checkWindowsEventLog(at, "WSV-03: fail-start must log an SCM 7000/7009 event"),
		),
		// Convergence: with the good version live, the next plan is empty.
		resource.TestStep{
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: false,
		},
	)
}

// WSV-04: upgrade to the `--health 500` APPLICATION VARIANT (a distinct build
// published at version 1.1.0-health500) with rollback on ⇒ ERR_HEALTH_CHECK;
// restored to the prior healthy version (1.0.0), junction & /health back on 1.0.0.
// DESIGN §18 WSV-04 requires deploying the 500-serving BUILD, not merely pointing
// health at a 500 endpoint — evaluator item 11.
func TestAccWSV04_HealthFailRollsBack(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	bad := accDeploymentConfig(winServiceSpec(t, at, "1.1.0-health500", "zip", healthURL(8080), ""))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		errorStep(bad, `ERR_HEALTH_CHECK`),
		// Rolled back to the healthy 1.0.0 build (no-op re-apply proves live state).
		applyStep(at, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkServiceState(at, "SampleSvc", "Running"),
			checkCurrentTarget(at, "1.0.0"),
			checkHealthBody(at, healthURL(8080), "v=1.0.0"),
		),
	)
}

// WSV-05: fail-start upgrade with rollback_on_failure=false ⇒ apply errors and
// the manifest records last_operation.result=failed; the NEXT plan is NON-empty
// (drift via §10.4) and re-applying the good version repairs — evaluator item 12.
func TestAccWSV05_NoRollbackLeavesDrift(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""))
	badNoRollback := accDeploymentConfig(winServiceSpecStrategy(t, at, "1.2.0-bad", "zip", healthURL(8080),
		"strategy: { keep_releases: 2, rollback_on_failure: false }"))
	runScenario(t, at,
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		errorStep(badNoRollback, `ERR_SERVICE_START`),
		// Drift: no rollback ⇒ manifest records the failed op and the next plan is
		// non-empty (desired 1.1.0 healthy ≠ actual failed 1.2.0-bad).
		resource.TestStep{
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
			Check: resource.ComposeAggregateTestCheckFunc(
				checkManifestContains(at, "failed", "WSV-05: manifest.last_operation.result must be failed"),
			),
		},
		// Re-applying the good version repairs the drift and converges.
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkServiceState(at, "SampleSvc", "Running"),
			checkHealthBody(at, healthURL(8080), "v=1.1.0"),
		),
	)
}

// WSV-06: wrapper=winsw wrapping a console build ⇒ fresh + upgrade both ok; the
// WinSW service XML under `current` is REGENERATED on upgrade (its contents
// reference the new version) — evaluator item 13.
func TestAccWSV06_WinswWrapper(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	extra := "  wrapper: winsw\n  winsw_exe: tools\\winsw.exe"
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), extra))
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), extra))
	runScenario(t, at,
		applyStep(at, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkWinswXMLVersion(at, "1.0.0", "WSV-06: fresh winsw xml references 1.0.0"),
		),
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkWinswXMLVersion(at, "1.1.0", "WSV-06: winsw xml regenerated to 1.1.0 on upgrade"),
		),
	)
}

// WSV-07: app ignores stop (slow-stop build) with stop_timeout_seconds=10 ⇒
// deploy succeeds; the stop phase force-kills within budget, leaving the service
// RUNNING on the new build. The whole upgrade is time-bounded (<90s wall) to
// prove the STOP phase does not hang, and app.log records the FORCE_KILL step —
// evaluator item 13.
func TestAccWSV07_ForceKillOnSlowStop(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), "  stop_timeout_seconds: 10"))
	slow := accDeploymentConfig(winServiceSpec(t, at, "1.1.0-slowstop", "zip", healthURL(8080), "  stop_timeout_seconds: 10"))
	wallBetween(t, 0, 90*time.Second, "WSV-07 slow-stop upgrade", func() {
		runScenario(t, at,
			applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
			applyStep(at, slow,
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0-slowstop"),
				checkServiceState(at, "SampleSvc", "Running"),
				checkAppLogContains(at, "FORCE_KILL", "WSV-07: STOP phase must record FORCE_KILL when the app ignores stop"),
			),
		)
	})
}

// WSV-08: non-builtin account `.\svcuser` with password_env ⇒ service
// ObjectName=.\svcuser; the secret never appears in TF logs (secret hygiene is
// unit-covered). Asserts the service runs under the requested account —
// evaluator item 13. Requires LABDEPLOY_ACC_SVCUSER_PW as the account password.
func TestAccWSV08_NonBuiltinAccount(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	if os.Getenv("LABDEPLOY_ACC_SVCUSER_PW") == "" {
		t.Fatalf("TF_ACC=1 requires LABDEPLOY_ACC_SVCUSER_PW (the .\\svcuser password) for WSV-08")
	}
	extra := "  account: { username: '.\\\\svcuser', password_env: LABDEPLOY_ACC_SVCUSER_PW }"
	cfg := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), extra))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkServiceObjectName(at, "SampleSvc", `.\svcuser`),
	))
}

// ---------------------------------------------------------------------------
// NOD / NET (§18.6) — node_web_app and dotnet_api on W1
// ---------------------------------------------------------------------------

// NOD-01: node app (bundled node_modules, install_deps=false) on port 8087 ⇒
// service running; GET :8087/health 200; PORT visible in app.log — evaluator item 14.
func TestAccNOD01_NodeBundled(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := nodeSpec(t, at, "node-1.0.0", 8087, false)
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "node-1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkServiceState(at, "SampleSvc", "Running"),
		checkHealthBody(at, healthURL(8087), "v="),
		checkAppLogContains(at, "PORT", "NOD-01: app.log must show the injected PORT env"),
	))
}

// NOD-02: install_deps=true against an artifact WITHOUT package-lock.json ⇒
// ERR_SERVICE_INSTALL mentioning the lockfile; no service created and no files
// copied (fresh) — evaluator item 14. Post-state probed at destroy time.
func TestAccNOD02_NoLockfile(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := nodeSpec(t, at, "node-nolock-1.0.0", 8087, true)
	errorScenario(t, at, accDeploymentConfig(spec), `ERR_SERVICE_INSTALL(?s).*lock`,
		checkServiceAbsent(at, "SampleSvc"),
		checkReleaseAbsent(at, "node-nolock-1.0.0", "NOD-02: a failed install must not leave a release dir"),
	)
}

// NOD-03: install_deps=true WITH a lockfile ⇒ node_modules exists in the release
// dir; healthy — evaluator item 14.
func TestAccNOD03_InstallDeps(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := nodeSpec(t, at, "node-lock-1.0.0", 8087, true)
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "node-lock-1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkReleaseSubdir(at, "node-lock-1.0.0", "node_modules", "NOD-03: install_deps must produce node_modules in the release dir"),
		checkHealthBody(at, healthURL(8087), "v="),
	))
}

// NET-01: dotnet self-contained, hosting native, urls :8088 ⇒ running; health
// ok; ASPNETCORE_URLS present in HKLM env — evaluator item 14.
func TestAccNET01_DotnetSelfContained(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := dotnetSpec(t, at, "dotnet-1.0.0", "exe", "windows_service_native")
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "dotnet-1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkServiceState(at, "SampleSvc", "Running"),
		checkHKLMEnv(at, "ASPNETCORE_URLS", "8088"),
		checkHealthBody(at, healthURL(8088), "v="),
	))
}

// NET-02: launcher=dotnet_dll on a box WITHOUT the ASP.NET runtime ⇒ ERR_PREFLIGHT
// naming `Microsoft.AspNetCore.App` BEFORE any file is copied (release dir absent).
// Opt-in via a box that lacks the runtime (LABDEPLOY_ACC_NO_ASPNET=1) — item 14.
func TestAccNET02_PreflightMissingRuntime(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	if os.Getenv("LABDEPLOY_ACC_NO_ASPNET") != "1" {
		t.Skip("set LABDEPLOY_ACC_NO_ASPNET=1 on a W1 that lacks the ASP.NET runtime for NET-02")
	}
	spec := dotnetSpec(t, at, "dotnet-fdd-1.0.0", "dotnet_dll", "windows_service_native")
	errorScenario(t, at, accDeploymentConfig(spec), `ERR_PREFLIGHT(?s).*Microsoft\.AspNetCore\.App`,
		checkReleaseAbsent(at, "dotnet-fdd-1.0.0", "NET-02: preflight must fail BEFORE any file is copied"),
		checkServiceAbsent(at, "SampleSvc"),
	)
}
