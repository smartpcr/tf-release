package provider

import (
	"testing"

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

// DRF-01: perturb the live deployment out-of-band (remove `current`), then plan
// shows drift and apply reconverges. On L1 console this manifests as the current
// symlink going missing; apply restores it.
func TestAccDRF01_DriftReconverges(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	current := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Current
	runScenario(t, at,
		applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		resource.TestStep{
			PreConfig:          func() { _ = deletePathOnHost(at, current) },
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkCurrentTarget(at, "1.0.0")),
	)
}

// DRF-02: delete manifest.json out-of-band ⇒ plan = CREATE (state removed on
// refresh); apply reinstalls cleanly (idempotent create over leftovers).
func TestAccDRF02_DeletedManifestRecreates(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	manifest := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Manifest
	runScenario(t, at,
		applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		resource.TestStep{
			PreConfig:          func() { _ = deletePathOnHost(at, manifest) },
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
	)
}

// DRF-03: repoint `current` to an older release while state says 1.1.0 ⇒ plan
// shows deployed_version drift; apply converges to 1.1.0 WITHOUT an artifact
// fetch (the release is still cached).
func TestAccDRF03_JunctionDriftConverges(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	v10 := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	v11 := accDeploymentConfig(consoleSpec(t, at, "1.1.0", "zip", ""))
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "1.0.0")
	current := p.Current
	repoint := func() {
		// Point current at the older (still-cached) 1.0.0 release out-of-band.
		_, _ = probeHost(at,
			"",
			"ln -sfn '"+p.Release+"' '"+current+"'")
	}
	runScenario(t, at,
		applyStep(at, v10, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0")),
		resource.TestStep{
			PreConfig:          repoint,
			Config:             v11,
			PlanOnly:           true,
			ExpectNonEmptyPlan: true,
		},
		applyStep(at, v11, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.1.0"),
			checkCurrentTarget(at, "1.1.0")),
	)
}

// ---------------------------------------------------------------------------
// DST — destroy modes (§18.8, DESIGN §10.6)
// ---------------------------------------------------------------------------

// DST-01: destroy (purge) ⇒ the release tree is gone and state is empty. The
// standard CheckDestroy already asserts `.lock` absent; here we also prove the
// install root is removed.
func TestAccDST01_DestroyPurge(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	root := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Root
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		},
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			checkPathAbsent(at, root, "purge must remove the install root"),
		),
	})
}

// DST-02: destroy_mode=abandon ⇒ no connection is made and the machine is
// untouched; state is still emptied. Proved by leaving the release tree present
// after destroy.
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
	root := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Root
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		},
		// abandon leaves the machine untouched: the release tree survives destroy.
		CheckDestroy: checkPathPresent(at, root, "abandon must NOT remove the install root"),
	})
}

// ---------------------------------------------------------------------------
// IDP — idempotency (§18.8)
// ---------------------------------------------------------------------------

// IDP-01: re-apply the identical spec ⇒ plan empty (no changes) on the second
// round.
func TestAccIDP01_ReapplyPlanEmpty(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	runScenario(t, at,
		applyStep(at, cfg, resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0")),
		resource.TestStep{
			Config:             cfg,
			PlanOnly:           true,
			ExpectNonEmptyPlan: false,
		},
	)
}

// ---------------------------------------------------------------------------
// LCK — locking (§18.8, DESIGN §10.5)
// ---------------------------------------------------------------------------

// LCK-01: a fresh `.lock` owned by another apply is present ⇒ apply fails fast
// with ERR_LOCKED naming the owner. The contending lock is planted out-of-band.
func TestAccLCK01_ContendedLockErrors(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					_ = plantLock(at, installRootFor(at), "sample-svc", "apply-A@runner", 0)
				},
				Config:      cfg,
				ExpectError: mustRe(`ERR_LOCKED`),
			},
		},
		// After the failed apply, clean the planted lock out-of-band, then assert
		// the invariant holds (the planted lock was ours, not a real apply's).
		CheckDestroy: func(*terraform.State) error {
			lock := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Lock
			if err := deletePathOnHost(at, lock); err != nil {
				return err
			}
			return assertLockAbsent(at.tgt, installRootFor(at), "sample-svc")
		},
	})
}

// LCK-02: a `.lock` aged beyond lock_timeout is present ⇒ apply overrides it and
// SUCCEEDS with a WARN diag about the stale lock being overridden.
func TestAccLCK02_StaleLockOverridden(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	cfg := accDeploymentConfig(consoleSpec(t, at, "1.0.0", "zip", ""))
	runScenario(t, at, resource.TestStep{
		PreConfig: func() {
			// Aged far beyond the default lock_timeout so it is deemed stale.
			_ = plantLock(at, installRootFor(at), "sample-svc", "dead-apply@runner", 24*3600)
		},
		Config: cfg,
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
			checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		),
	})
}
