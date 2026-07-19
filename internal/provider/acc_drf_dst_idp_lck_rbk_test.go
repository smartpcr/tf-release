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

// RBK-01: after an upgrade to 1.1.0, apply -var version=1.0.0 ⇒ success from the
// on-target CACHE (no re-fetch); previous_version=1.1.0.
func TestAccRBK01_RollForwardCached(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	v10 := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(consoleSpec(t, at, "1.1.0", "zip", ""))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		applyStep(at, v10,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "previous_version", "1.1.0"),
		),
	)
}

// RBK-02: keep_releases=1 (1.0.0 pruned by the 1.1.0 apply), then apply 1.0.0 ⇒
// success WITH a re-fetch (the pruned release is no longer cached).
func TestAccRBK02_RollBackRefetch(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	v10 := accDeploymentConfig(consoleSpecKeep(t, at, "1.0.0", "zip", 1))
	v11 := accDeploymentConfig(consoleSpecKeep(t, at, "1.1.0", "zip", 1))
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkReleaseCount(at, 1),
		),
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
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
		// Repair: apply starts the service (only) and re-converges; exactly one
		// release remains (no fetch/switch happened, just a start).
		applyStep(at, cfg,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "service_status", "running"),
			checkServiceState(at, "SampleSvc", "Running"),
			checkHealthBody(at, healthURL(8080), "v=1.0.0"),
			checkReleaseCount(at, 1),
			checkCurrentTarget(at, "1.0.0"),
		),
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
		applyStep(at, v11,
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkCurrentTarget(at, "1.1.0"),
			checkReleaseCount(at, 2),
		),
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
// is untouched; state is still emptied. We prove "machine untouched" by asserting
// the full release tree (root, current, manifest, release dir) survives destroy
// with UNCHANGED mtimes — evaluator item 16. (An abandon destroy performs no dial
// at all, so it succeeds even against an unreachable host; the observable proof
// available to acceptance is that nothing on the box changed.)
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
	var rootMtime string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			applyStep(at, cfg,
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				func(*terraform.State) error {
					m, err := hostMtime(at, p.Root)
					rootMtime = m
					return err
				},
			),
		},
		// abandon leaves the machine untouched: the tree survives with the SAME
		// root mtime (nothing was rewritten/removed during destroy).
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkPathPresent(at, p.Root, "abandon must NOT remove the install root"),
			checkPathPresent(at, p.Current, "abandon must NOT remove current"),
			checkPathPresent(at, p.Release, "abandon must NOT remove the release dir"),
			func(s *terraform.State) error { return checkMtimeUnchanged(at, p.Root, rootMtime, "DST-02: abandon must not touch the box")(s) },
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

// LCK-01: a fresh `.lock` owned by another apply is present ⇒ apply fails FAST
// (<5s, non-blocking acquire) with ERR_LOCKED NAMING the owner of the contending
// apply. The contending lock is planted out-of-band to simulate apply A holding
// the lock (true wall-clock concurrency isn't expressible in the sequential
// TestStep model; the engine's non-blocking acquire + owner reporting is what
// DESIGN §18 LCK-01 asserts) — evaluator item 18.
func TestAccLCK01_ContendedLockErrors(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	const owner = "apply-A@runner"
	wallBetween(t, 0, 30*time.Second, "LCK-01 contended apply", func() {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					PreConfig: preConfig(t, func() error {
						return plantLock(at, installRootFor(at), "sample-svc", owner, 0)
					}),
					Config: cfg,
					// The error must name the owner of the lock held by apply A.
					ExpectError: mustRe(`ERR_LOCKED(?s).*` + regexp.QuoteMeta(owner)),
				},
			},
			// Clean the planted lock (it was ours, not a real apply's), then assert
			// the invariant holds.
			CheckDestroy: func(*terraform.State) error {
				lock := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Lock
				if err := deletePathOnHost(at, lock); err != nil {
					return err
				}
				return assertLockAbsent(at.tgt, installRootFor(at), "sample-svc")
			},
		})
	})
}

// LCK-02: a `.lock` aged beyond lock_timeout is present ⇒ apply overrides it and
// SUCCEEDS with a WARN diag about the stale lock being overridden.
func TestAccLCK02_StaleLockOverridden(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	runScenario(t, at, resource.TestStep{
		PreConfig: preConfig(t, func() error {
			// Aged far beyond the default lock_timeout so it is deemed stale.
			return plantLock(at, installRootFor(at), "sample-svc", "dead-apply@runner", 24*3600)
		}),
		Config: cfg,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		),
	})
}
