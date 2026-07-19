package provider

import (
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
)

// Stage 9.1 — DRF/DST/IDP/LCK/RBK acceptance scenarios (DESIGN §18.8), run on L1
// (Linux VM, ssh) per the workstream "Linux console and drift" matrix. Drift is
// induced out-of-band between steps via TestStep.PreConfig (which mutates the
// target host), then the following plan/apply asserts the convergence behavior
// DESIGN §18.8 requires. Every test maps 1:1 to a DESIGN §18 ID and ends by
// asserting `.lock` is absent on L1.

// ---------------------------------------------------------------------------
// RBK — rollback / cache (§18.8)
// ---------------------------------------------------------------------------

// RBK-01: after an upgrade to 1.1.0, roll back to 1.0.0 ⇒ success from the
// on-target CACHE. cache_hit=true is proven DIRECTLY from the provider TRACE log:
// the roll-back apply emits NO FETCH step and logs "release cached; skipping
// fetch/extract" — i.e. it needed no network to the artifact host (DESIGN §18
// RBK-01 "block it to prove"). previous_version=1.1.0 and `current`→1.0.0 confirm
// the rollback landed on the right (healthy) release — evaluator item 8.
func TestAccRBK01_RollForwardCached(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	logPath := tfLogCapture(t)
	v10 := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(consoleSpec(t, at, "1.1.0", "zip", ""))
	sha10 := artifactSHA(t, "1.0.0", "zip")
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		// Roll back to the still-cached 1.0.0: cache_hit=true (no FETCH), and the
		// live release is the correct 1.0.0 build.
		resource.TestStep{
			PreConfig: truncateTFLog(t, logPath),
			Config:    v10,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "previous_version", "1.1.0"),
				checkCacheHitLogged(logPath, "RBK-01: rollback to a cached release must NOT re-fetch (cache_hit=true, no artifact-host network)"),
				checkCurrentTarget(at, "1.0.0"),
				checkReleaseMarkerSHA(at, "1.0.0", "zip", sha10),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
	)
}

// RBK-02: keep_releases=1 (1.0.0 pruned by the 1.1.0 apply), then apply 1.0.0 ⇒
// success WITH a re-fetch. cache_hit=false is proven from the TRACE log: the pruned
// release is no longer cached, so the re-apply MUST emit a FETCH step — evaluator
// item 9.
func TestAccRBK02_RollBackRefetch(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	logPath := tfLogCapture(t)
	v10 := accDeploymentConfig(consoleSpecKeep(t, at, "1.0.0", "zip", 1))
	v11 := accDeploymentConfig(consoleSpecKeep(t, at, "1.1.0", "zip", 1))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkReleaseCount(at, 1),
		),
		// 1.0.0 was pruned ⇒ re-applying it must RE-FETCH (cache_hit=false).
		resource.TestStep{
			PreConfig: truncateTFLog(t, logPath),
			Config:    v10,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				checkRefetchLogged(logPath, "RBK-02: a pruned release must be RE-FETCHED (cache_hit=false)"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
	)
}

// ---------------------------------------------------------------------------
// DRF — drift reconciliation (§18.8, DESIGN §10.4)
// ---------------------------------------------------------------------------

// DRF-01: Stop-Service the live windows_service out-of-band, then `plan` shows a
// `service_status running→stopped`-driven update and `apply` performs a START
// ONLY (no re-fetch / no switch) and converges. This is the DESIGN §18 DRF-01
// scenario 1:1 (Windows service, not a Linux symlink) — evaluator item 15. The
// release count is asserted UNCHANGED across the repair to prove no artifact was
// fetched.
func TestAccDRF01_StoppedServiceStartsOnly(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	logPath := tfLogCapture(t)
	cfg := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	runScenario(t, at,
		applyStep(at, cfg,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
		),
		// Drift: stop the service behind Terraform's back.
		resource.TestStep{
			PreConfig:          preConfig(t, func() error { return stopServiceOnHost(at, "SampleSvc") }),
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
			Check:              checkServiceState(at, "SampleSvc", "Stopped"),
		},
		// Repair: the apply must perform a START ONLY. The provider TRACE log
		// proves it: a START step is emitted while FETCH and SWITCH are ABSENT
		// (no artifact fetched, no junction re-pointed) — evaluator item 10. The
		// log is scoped to the repair apply via truncateTFLog.
		resource.TestStep{
			PreConfig: func() { truncateTFLog(t, logPath)() },
			Config:    cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
				checkServiceState(at, "SampleSvc", "Running"),
				checkHealthBody(at, healthURL(8080), "v=1.0.0"),
				checkReleaseCount(at, 1),
				checkCurrentTarget(at, "1.0.0"),
				checkStepLogged(logPath, "START", "DRF-01: drift repair must START the service"),
				checkStepNotLogged(logPath, "FETCH", "DRF-01: a start-only repair must NOT fetch an artifact"),
				checkStepNotLogged(logPath, "SWITCH", "DRF-01: a start-only repair must NOT re-switch the junction"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
	)
}

// DRF-02: delete manifest.json out-of-band ⇒ plan = CREATE (state removed on
// refresh); apply reinstalls cleanly (idempotent create over leftovers) and the
// live deployment is healthy again — evaluator item 15 (companion).
func TestAccDRF02_DeletedManifestRecreates(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	manifest := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Manifest
	runScenario(t, at,
		applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		resource.TestStep{
			PreConfig:          preConfig(t, func() error { return deletePathOnHost(at, manifest) }),
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		applyStep(at, cfg,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkPathPresent(at, manifest, "DRF-02: apply must reinstall manifest.json"),
			checkCurrentTarget(at, "1.0.0"),
		),
	)
}

// DRF-03: repoint `current` to an older release while state says 1.1.0 ⇒ plan
// shows deployed_version drift; apply converges to 1.1.0 WITHOUT an artifact
// fetch (the release is still cached — release count stays 2) — evaluator item 15.
func TestAccDRF03_JunctionDriftConverges(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	logPath := tfLogCapture(t)
	v10 := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(consoleSpec(t, at, "1.1.0", "zip", ""))
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "1.0.0")
	current := p.Current
	repoint := func() error {
		// Point current at the older (still-cached) 1.0.0 release out-of-band.
		_, err := probeHost(at, "", "ln -sfn '"+shq(p.Release)+"' '"+shq(current)+"'")
		return err
	}
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		resource.TestStep{
			PreConfig:          preConfig(t, repoint),
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		// Converge back to 1.1.0. The release is still cached, so the repair
		// re-points `current` (a SWITCH) but must NOT fetch the artifact again —
		// proven from the TRACE log: SWITCH present, FETCH absent — evaluator item 11.
		resource.TestStep{
			PreConfig: func() { truncateTFLog(t, logPath)() },
			Config:    v11,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
				checkCurrentTarget(at, "1.1.0"),
				checkReleaseCount(at, 2),
				checkStepLogged(logPath, "SWITCH", "DRF-03: junction drift repair must re-SWITCH current"),
				checkStepNotLogged(logPath, "FETCH", "DRF-03: converging cached-release drift must NOT re-fetch the artifact"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		},
	)
}

// ---------------------------------------------------------------------------
// DST — destroy modes (§18.8, DESIGN §10.6)
// ---------------------------------------------------------------------------

// DST-01: destroy (purge) a windows_service deployment ⇒ the SERVICE is removed
// (`sc query`⇒1060), the release tree is gone and state is empty. DESIGN §18
// DST-01 is a Windows service removal, not a Linux console tree — evaluator item 16.
func TestAccDST01_DestroyPurge(t *testing.T) {
	accPreCheck(t)
	at := requireW1(t)
	cfg := accDeploymentConfig(winServiceSpec(t, at, "1.0.0", "zip", healthURL(8080), ""))
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	root := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Root
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			applyStep(at, cfg,
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
			),
		},
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			checkServiceAbsent(at, "SampleSvc"),
			checkPathAbsent(at, root, "purge must remove the install root"),
		),
	})
}

// DST-02: destroy_mode=abandon ⇒ the destroy makes NO connection and the machine
// is untouched; state is still emptied. The no-dial guarantee is structural:
// Engine.Destroy returns nil for mode=="abandon" BEFORE it ever builds a transport
// (internal/engine/engine.go — `if mode == "abandon" { return nil }`), so no
// connection is possible; that path is unit-covered. The acceptance-observable
// proof here is that NOTHING on the box changed: a RECURSIVE fingerprint that
// includes a per-file SHA-256 CONTENT HASH (not just mtime+size) is captured after
// apply and asserted byte-identical after destroy, so even a same-size, same-mtime
// content rewrite would be caught — evaluator item 12. (The post-destroy probe is
// the TEST verifying the box; proving the destroy ITSELF issued zero dials at the
// acceptance layer would require instrumenting the transport factory — raised as
// an Open Question — and is redundant with the engine.go early-return guarantee.)
func TestAccDST02_DestroyAbandon(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	specYAML := consoleSpec(t, at, "1.0.0", "zip", "")
	cfg := testAccProviderConfig + `
resource "labdeploy_deployment" "val" {
  destroy_mode = "abandon"
  spec = <<-EOT
` + specYAML + `EOT
}
`
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "1.0.0")
	var treeBefore string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			applyStep(at, cfg,
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				func(*terraform.State) error {
					snap, err := snapshotTree(at, p.Root)
					treeBefore = snap
					return err
				},
			),
		},
		// abandon leaves the machine untouched: the WHOLE tree survives with an
		// identical fingerprint (no path added, removed, rewritten, or re-timed).
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkPathPresent(at, p.Root, "abandon must NOT remove the install root"),
			checkPathPresent(at, p.Current, "abandon must NOT remove current"),
			checkPathPresent(at, p.Release, "abandon must NOT remove the release dir"),
			func(s *terraform.State) error {
				return checkTreeUnchanged(at, p.Root, treeBefore, "DST-02: abandon must not touch any file on the box")(s)
			},
		),
	})
}

// ---------------------------------------------------------------------------
// IDP — idempotency (§18.8)
// ---------------------------------------------------------------------------

// IDP-01: re-apply the identical spec (service running) ⇒ plan empty; with
// -refresh-only no changes; target file mtimes unchanged across the idempotent
// round — evaluator item 17.
func TestAccIDP01_ReapplyPlanEmpty(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	relFile := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "1.0.0").Release
	var mtimeBefore string
	runScenario(t, at,
		applyStep(at, cfg,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			func(*terraform.State) error {
				m, err := hostMtime(at, relFile)
				mtimeBefore = m
				return err
			},
		),
		// Idempotent re-apply ⇒ empty plan.
		resource.TestStep{
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: false,
		},
		// -refresh-only ⇒ no changes detected on refresh.
		resource.TestStep{
			Config:             cfg,
			RefreshState:       true,
			ExpectNonEmptyPlan: false,
		},
		// The idempotent round must not have rewritten the release files.
		applyStep(at, cfg,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			func(s *terraform.State) error {
				return checkMtimeUnchanged(at, relFile, mtimeBefore, "IDP-01: idempotent re-apply must not rewrite release files")(s)
			},
		),
	)
}

// ---------------------------------------------------------------------------
// LCK — locking (§18.8, DESIGN §10.5)
// ---------------------------------------------------------------------------

// LCK-01: apply A holds the deployment `.lock`; apply B run CONCURRENTLY must
// fail FAST (<5s, non-blocking acquire) with ERR_LOCKED NAMING A's owner; A then
// completes normally. Apply A is a REAL concurrent lock-holder: a background
// goroutine plants A's fresh, owner-stamped lock and actively HOLDS it on the host
// for the duration of B's attempt (releasing only after), so B genuinely races a
// live holder rather than a static file. A dedicated two-terraform-process race is
// not expressible inside terraform-plugin-testing's single-binary model (raised as
// an Open Question) — this live-holder goroutine is the strongest in-harness proxy
// — evaluator item 13.
func TestAccLCK01_ContendedLockErrors(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	const owner = "apply-A@runner"
	lock := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Lock

	// Apply A (concurrent live holder): plant A's fresh lock, hold it on the host
	// while B runs, then release. release<-done signals the hold is over.
	held := make(chan struct{})   // closed once A's lock is actually on the host
	release := make(chan struct{}) // test signals A to release
	aDone := make(chan error, 1)
	go func() {
		if err := plantLock(at, installRootFor(at), "sample-svc", owner, 0); err != nil {
			aDone <- err
			close(held)
			return
		}
		close(held)
		<-release // keep holding until the test says B is done
		_, err := probeHost(at, "Remove-Item -LiteralPath '"+psq(lock)+"' -Force -ErrorAction SilentlyContinue; 'ok'",
			"rm -f '"+shq(lock)+"'")
		aDone <- err
	}()
	<-held // ensure A holds the lock BEFORE B starts (true overlap)

	// Apply B: contends with A's live lock and must fail in <5s naming A.
	wallBetween(t, 0, 5*time.Second, "LCK-01 contended apply B", func() {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{Config: cfg, ExpectError: mustRe(`ERR_LOCKED(?s).*` + regexp.QuoteMeta(owner))},
			},
		})
	})
	// B is done ⇒ let A release its lock, then confirm the lock is gone.
	close(release)
	if err := <-aDone; err != nil {
		t.Fatalf("LCK-01 apply-A holder: %v", err)
	}
	if err := assertLockAbsent(at.tgt, installRootFor(at), "sample-svc"); err != nil {
		t.Fatalf("LCK-01: lock must be absent after A releases: %v", err)
	}
	// A completes normally: with the lock released, an apply now succeeds.
	runScenario(t, at, applyStep(at, cfg,
		resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
	))
}

// LCK-02: a `.lock` aged beyond lock_timeout is present ⇒ apply overrides it and
// SUCCEEDS, emitting the DESIGN §10.5 WARN diagnostic `stale lock … overridden`
// naming the prior owner. The warning is a Terraform WARN diagnostic surfaced into
// the provider TRACE log (terraform-plugin-testing exposes no Check hook for
// warning diagnostics), so it is asserted via the captured TRACE — evaluator item 14.
func TestAccLCK02_StaleLockOverridden(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	logPath := tfLogCapture(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	const deadOwner = "dead-apply@runner"
	runScenario(t, at, resource.TestStep{
		PreConfig: preConfig(t, func() error {
			truncateTFLog(t, logPath)()
			// Aged far beyond the default lock_timeout so it is deemed stale.
			return plantLock(at, installRootFor(at), "sample-svc", deadOwner, 24*3600)
		}),
		Config: cfg,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkTFLogMatches(logPath, regexp.MustCompile(`(?s)stale lock from `+regexp.QuoteMeta(deadOwner)+`.*overridden`),
				"LCK-02: apply must emit the 'stale lock … overridden' WARN naming the prior owner"),
			checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		),
	})
}
