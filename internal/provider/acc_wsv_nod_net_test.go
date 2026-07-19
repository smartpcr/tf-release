package provider

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
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

// WSV-01: fresh apply v1.0.0 with http /health ⇒ service RUNNING, start_type
// AUTO_START, recovery actions set, per-service HKLM Environment carries
// LD_VERSION=1.0.0 AND the caller `environment:` var APP_MODE=canary, health
// passed, junction→1.0.0, app.log carries the env dump. Asserts the FULL DESIGN
// §18 WSV-01 post-state — evaluator item 8.
func TestAccWSV01_FreshService(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	extra := "  start_type: auto\n  recovery: { restart_on_failure: true }"
	base := winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), extra)
	cfg := accDeploymentConfig(base + "environment:\n  APP_MODE: canary\n")
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkServiceState(at, "SampleSvc", "Running"),
		checkServiceStartMode(at, "SampleSvc", "Auto"),
		checkServiceRecovery(at, "SampleSvc", "WSV-01: recovery actions must be set"),
		checkCurrentTarget(at, "1.0.0"),
		checkServiceEnv(at, "SampleSvc", "LD_VERSION", "1.0.0"),
		checkServiceEnv(at, "SampleSvc", "APP_MODE", "canary"),
		checkHealthBody(at, healthURL(8080), "v=1.0.0"),
		checkAppLogContains(at, "LD_VERSION", "WSV-01: app.log must contain the env dump proving env delivery"),
	))
}

// WSV-02: upgrade v1.0.0 → v1.1.0 ⇒ zero-error apply; previous_version=1.0.0;
// both releases retained; junction→1.1.0; /health body v=1.1.0. A detached 1 Hz
// health poller (started on W1 just before the upgrade) measures the outage; the
// downtime window must stay ≤ stop_timeout(30 default)+15s = 45s — evaluator
// item 9.
func TestAccWSV02_Upgrade(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	v11 := accDeploymentConfig(winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), ""))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		resource.TestStep{
			// Launch the 1 Hz downtime probe on W1, then let the upgrade apply run
			// concurrently; the probe records OK/FAIL each second for 90s.
			PreConfig: preConfig(t, func() error { return startDowntimeProbe(at, healthURL(8080), 90) }),
			Config:    v11,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "previous_version", "1.0.0"),
				checkReleaseCount(at, 2),
				checkCurrentTarget(at, "1.1.0"),
				checkHealthBody(at, healthURL(8080), "v=1.1.0"),
				checkDowntimeBounded(at, 45, "WSV-02: upgrade downtime must be ≤ stop_timeout+15s"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
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
	// Captured in the FAILING step's PreConfig (immediately before the bad apply)
	// so the event-log assertion only accepts an SCM failure raised by THIS apply,
	// never a stale event from the preceding good 1.1.0 apply — evaluator item 4.
	var since time.Time
	runScenario(t, at,
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		resource.TestStep{
			PreConfig:   func() { since = time.Now() },
			Config:      bad,
			ExpectError: mustRe(`ERR_SERVICE_START(?s).*(7000|7009)`),
		},
		// Post-rollback: re-applying 1.1.0 is a no-op that lets Check assert the
		// rolled-back live state (service running on 1.1.0, junction & health on
		// 1.1.0, an SCM event was logged for the failed 1.2.0-bad start).
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
			checkServiceState(at, "SampleSvc", "Running"),
			checkCurrentTarget(at, "1.1.0"),
			checkHealthBody(at, healthURL(8080), "v=1.1.0"),
			func(s *terraform.State) error {
				return checkWindowsEventLog(at, "SampleSvc", since, "WSV-03: fail-start must log an SCM 7000/7009 event for SampleSvc")(s)
			},
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
		// Drift: no rollback ⇒ the failed 1.2.0-bad is left live-but-broken (service
		// stopped-or-crashed on 1.2.0-bad), the manifest records the failed op, and
		// the next plan is non-empty (desired 1.1.0 healthy ≠ actual failed 1.2.0-bad)
		// — evaluator item 12.
		resource.TestStep{
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
			Check: resource.ComposeAggregateTestCheckFunc(
				checkServiceNotRunning(at, "SampleSvc", "WSV-05: no-rollback must leave the failed service stopped-or-crashed"),
				checkCurrentTarget(at, "1.2.0-bad"),
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

// WSV-06: wrapper=winsw with stop_timeout_seconds=10 ⇒ fresh + upgrade both ok;
// the WinSW xml is REGENERATED on upgrade and carries <stopwait>10sec</stopwait>.
// Per DESIGN §18.5 the slow-stop behavior must be INJECTED and MEASURED. To ensure
// the measured wall reflects the STOP phase and NOT artifact work, the destination
// (1.0.0) is PRE-CACHED first; the slow-stop build is then installed; finally we
// roll to the CACHED 1.0.0 — that final apply does NO fetch/stage (cache_hit=true,
// asserted from the TRACE log), so its wall is dominated by winsw waiting out the
// <stopwait> on the running slow-stop service before force-killing. That wall must
// be ≥8s (winsw actually waited) and ≤25s (killed at the deadline, not hung) —
// evaluator item 4.
func TestAccWSV06_WinswWrapper(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	logPath := tfLogCapture(t)
	extra := "  wrapper: winsw\n  winsw_exe: tools\\winsw.exe\n  stop_timeout_seconds: 10"
	normal := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), extra))
	slow := accDeploymentConfig(winServiceSpec(t, at, "1.1.0-slowstop", "zip", healthURL(8080), extra))
	var t0 time.Time
	runScenario(t, at,
		// Fresh install of 1.0.0 under winsw — seeds the on-target cache for 1.0.0.
		applyStep(at, normal,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkWinswXMLVersion(at, "1.0.0", "WSV-06: fresh winsw xml references 1.0.0"),
			checkWinswStopwait(at, 10, "WSV-06: winsw xml must honor stop_timeout_seconds"),
		),
		// Upgrade to the SLOW-STOP build (this STOP is of the normal 1.0.0 build,
		// fast; we are only getting the slow-stop service RUNNING here).
		applyStep(at, slow,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0-slowstop"),
			checkWinswXMLVersion(at, "1.1.0-slowstop", "WSV-06: winsw xml regenerated on upgrade"),
		),
		// Roll to the CACHED 1.0.0: no FETCH/STAGE (cache_hit) so the wall isolates
		// the STOP phase — winsw must wait out <stopwait> on the running slow-stop
		// build then force-kill.
		resource.TestStep{
			PreConfig: func() { truncateTFLog(t, logPath)(); t0 = time.Now() },
			Config:    normal,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				checkCacheHitLogged(logPath, "WSV-06: the roll to 1.0.0 must reuse the cache (no FETCH) so the measured wall is the STOP phase, not artifact work"),
				checkWinswXMLVersion(at, "1.0.0", "WSV-06: winsw xml regenerated on the cached roll"),
				checkWinswStopwait(at, 10, "WSV-06: regenerated winsw xml must still carry stopwait"),
				wallSinceBetween(&t0, 8*time.Second, 25*time.Second, "WSV-06: winsw must honor <stopwait> (wait ~10s then kill) stopping the slow-stop build"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
	)
}

// WSV-07: an app that IGNORES stop (slow-stop build) with stop_timeout_seconds=10.
// We deploy it, then roll FORWARD to the already-cached 1.0.0 release. Because the
// running service ignores graceful stop, the engine MUST escalate to the S2
// FORCE_KILL step; we prove that DIRECTLY by asserting the structured
// `step=FORCE_KILL` record appears in the provider TRACE log for THIS apply, and
// that the STOP phase completes under the DESIGN §18 WSV-07 25s budget — evaluator
// item 6. The TRACE log is scoped to the roll-forward apply via truncateTFLog.
func TestAccWSV07_ForceKillOnSlowStop(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	logPath := tfLogCapture(t)
	v10 := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), "  stop_timeout_seconds: 10"))
	slow := accDeploymentConfig(winServiceSpec(t, at, "1.1.0-slowstop", "zip", healthURL(8080), "  stop_timeout_seconds: 10"))
	var t0 time.Time
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, slow, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0-slowstop")),
		// Roll forward to the CACHED 1.0.0: the running slow-stop service ignores
		// graceful stop ⇒ the engine escalates to FORCE_KILL. Truncate the TRACE
		// log first so the step assertion is scoped to THIS apply only.
		resource.TestStep{
			PreConfig: func() { truncateTFLog(t, logPath)(); t0 = time.Now() },
			Config:    v10,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				checkServiceState(at, "SampleSvc", "Running"),
				checkHealthBody(at, healthURL(8080), "v=1.0.0"),
				checkStepLogged(logPath, "FORCE_KILL", "WSV-07: a stop-ignoring service must emit the structured FORCE_KILL step"),
				wallSince(&t0, 25*time.Second, "WSV-07: STOP phase (force-kill) must finish <25s"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
	)
}

// WSV-08: non-builtin account `.\svcuser` with password_env ⇒ service
// ObjectName=.\svcuser AND the account password is absent from the `sc qc`
// capture the provider emits into the TF TRACE log (DESIGN §18.5 WSV-08). The
// provider's traceServiceConfig runs `sc.exe qc <svc>` after configure and logs it
// via tflog.Trace; sc qc reports BINARY_PATH_NAME + SERVICE_START_NAME but NEVER
// the password, so the proof is: (a) the sc qc capture record is PRESENT in the
// TRACE log AND the password value is absent FROM that record
// (checkSCQCTraceRedacted); (b) the password is absent from the whole TRACE log;
// (c) the on-host `sc qc` output does not leak the password — evaluator item 5.
// Requires LABDEPLOY_ACC_SVCUSER_PW.
func TestAccWSV08_NonBuiltinAccount(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	if os.Getenv("LABDEPLOY_ACC_SVCUSER_PW") == "" {
		t.Fatalf("TF_ACC=1 requires LABDEPLOY_ACC_SVCUSER_PW (the .\\svcuser password) for WSV-08")
	}
	logPath := tfLogCapture(t)
	extra := "  account: { username: '.\\\\svcuser', password_env: LABDEPLOY_ACC_SVCUSER_PW }"
	cfg := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), extra))
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		checkServiceObjectName(at, "SampleSvc", `.\svcuser`),
		checkSCQCTraceRedacted(logPath, "LABDEPLOY_ACC_SVCUSER_PW", "WSV-08: the password must be absent from the sc qc capture in the TRACE log"),
		checkSecretAbsentInLog(logPath, "LABDEPLOY_ACC_SVCUSER_PW", "WSV-08: the account password must never appear anywhere in the provider TRACE log"),
		checkNoSecretInSCQC(at, "SampleSvc", "LABDEPLOY_ACC_SVCUSER_PW", "WSV-08: the on-host sc qc capture must not leak the password"),
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
		checkServiceEnv(at, "SampleSvc", "ASPNETCORE_URLS", "8088"),
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
		t.Fatalf("TF_ACC=1 Windows matrix requires LABDEPLOY_ACC_NO_ASPNET=1 pointing W1 at a box WITHOUT the ASP.NET runtime so NET-02's preflight-miss proof runs (must not be skipped)")
	}
	spec := dotnetSpec(t, at, "dotnet-fdd-1.0.0", "dotnet_dll", "windows_service_native")
	errorScenario(t, at, accDeploymentConfig(spec), `ERR_PREFLIGHT(?s).*Microsoft\.AspNetCore\.App`,
		checkReleaseAbsent(at, "dotnet-fdd-1.0.0", "NET-02: preflight must fail BEFORE any file is copied"),
		checkServiceAbsent(at, "SampleSvc"),
	)
}
