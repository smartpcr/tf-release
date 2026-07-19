package provider

import (
	"fmt"
	"os"
	"testing"

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
// HKLM env, health passed. Asserts service_status=running, deployed_version=1.0.0.
func TestAccWSV01_FreshService(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
	))
}

// WSV-02: upgrade v1.0.0 → v1.1.0 ⇒ zero-error apply; previous_version output
// =1.0.0; both releases retained.
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
		),
	)
}

// WSV-03: upgrade to v1.2.0-bad (fail-start) with rollback on ⇒ apply fails
// ERR_SERVICE_START; the deployment stays RUNNING on the prior (1.1.0) version.
// After the failed step a re-apply of 1.1.0 must plan EMPTY (converged).
func TestAccWSV03_FailStartRollsBack(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""))
	bad := accDeploymentConfig(winServiceSpec(t, at, "1.2.0-bad", "zip", healthURL(8080), ""))
	runScenario(t, at,
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		errorStep(bad, `ERR_SERVICE_START`),
		// Post-rollback the service is healthy on 1.1.0 and re-applying it is a no-op.
		resource.TestStep{
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: false,
		},
	)
}

// WSV-04: upgrade to a v1.1.0 build serving 500 on /health, rollback on ⇒
// ERR_HEALTH_CHECK; restored to the prior healthy version. The 500 health URL is
// env-provided (a build/endpoint variant per DESIGN §18: `--health 500`).
func TestAccWSV04_HealthFailRollsBack(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	health500 := os.Getenv("LABDEPLOY_ACC_HEALTH500_URL")
	if health500 == "" {
		t.Fatalf("TF_ACC=1 requires LABDEPLOY_ACC_HEALTH500_URL (the --health 500 endpoint) for WSV-04")
	}
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	bad := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", health500, ""))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		errorStep(bad, `ERR_HEALTH_CHECK`),
	)
}

// WSV-05: fail-start upgrade with rollback_on_failure=false ⇒ apply errors and
// the manifest records last_operation.result=failed; the NEXT plan is NON-empty
// (drift via §10.4) and re-applying the good version repairs.
func TestAccWSV05_NoRollbackLeavesDrift(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""))
	badNoRollback := accDeploymentConfig(winServiceSpecStrategy(t, at, "1.2.0-bad", "zip", healthURL(8080),
		"strategy: { keep_releases: 2, rollback_on_failure: false }"))
	runScenario(t, at,
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		errorStep(badNoRollback, `ERR_SERVICE_START`),
		// Drift: the failed marker forces a non-empty plan on the next round.
		resource.TestStep{
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
	)
}

// WSV-06: wrapper=winsw wrapping a console build ⇒ fresh + upgrade both ok; the
// winsw xml is regenerated on upgrade.
func TestAccWSV06_WinswWrapper(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	extra := "  wrapper: winsw\n  winsw_exe: tools\\winsw.exe"
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), extra))
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), extra))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
	)
}

// WSV-07: app ignores stop (slow-stop build) with stop_timeout_seconds=10 ⇒
// deploy succeeds; the stop phase force-kills within budget. Uses the slow-stop
// build served at version 1.1.0-slowstop (env base template).
func TestAccWSV07_ForceKillOnSlowStop(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), "  stop_timeout_seconds: 10"))
	slow := accDeploymentConfig(winServiceSpec(t, at, "1.1.0-slowstop", "zip", healthURL(8080), "  stop_timeout_seconds: 10"))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, slow, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0-slowstop")),
	)
}

// WSV-08: non-builtin account `.\svcuser` with password_env ⇒ service
// ObjectName=.\svcuser; the secret never appears in TF logs. Requires
// LABDEPLOY_ACC_SVCUSER_PW as the account password env NAME's value.
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
	))
}

// ---------------------------------------------------------------------------
// NOD / NET (§18.6) — node_web_app and dotnet_api on W1
// ---------------------------------------------------------------------------

// NOD-01: node app (bundled node_modules, install_deps=false) on port 8087 ⇒
// service running; GET :8087/health 200; PORT visible in app.log.
func TestAccNOD01_NodeBundled(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := nodeSpec(t, at, "node-1.0.0", 8087, false)
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "node-1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
	))
}

// NOD-02: install_deps=true against an artifact WITHOUT package-lock.json ⇒
// ERR_SERVICE_INSTALL mentioning the lockfile; no service created (fresh).
func TestAccNOD02_NoLockfile(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := nodeSpec(t, at, "node-nolock-1.0.0", 8087, true)
	runScenario(t, at, errorStep(accDeploymentConfig(spec), `ERR_SERVICE_INSTALL`))
}

// NOD-03: install_deps=true WITH a lockfile ⇒ node_modules exists in the release
// dir; healthy.
func TestAccNOD03_InstallDeps(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := nodeSpec(t, at, "node-lock-1.0.0", 8087, true)
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "node-lock-1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
	))
}

// NET-01: dotnet self-contained, hosting native, urls :8088 ⇒ running; health
// ok; ASPNETCORE_URLS present in HKLM env.
func TestAccNET01_DotnetSelfContained(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	spec := dotnetSpec(t, at, "dotnet-1.0.0", "exe", "windows_service_native")
	runScenario(t, at, applyStep(at, accDeploymentConfig(spec),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "dotnet-1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
	))
}

// NET-02: launcher=dotnet_dll on a box WITHOUT the ASP.NET runtime ⇒ ERR_PREFLIGHT
// naming `Microsoft.AspNetCore.App` BEFORE any file is copied. Opt-in via a box
// that lacks the runtime (LABDEPLOY_ACC_NO_ASPNET=1).
func TestAccNET02_PreflightMissingRuntime(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	if os.Getenv("LABDEPLOY_ACC_NO_ASPNET") != "1" {
		t.Skip("set LABDEPLOY_ACC_NO_ASPNET=1 on a W1 that lacks the ASP.NET runtime for NET-02")
	}
	spec := dotnetSpec(t, at, "dotnet-fdd-1.0.0", "dotnet_dll", "windows_service_native")
	runScenario(t, at, errorStep(accDeploymentConfig(spec), `ERR_PREFLIGHT.*Microsoft\.AspNetCore\.App`))
}
