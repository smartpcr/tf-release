package provider

import (
	"context"
	"fmt"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// singleTargetDeployment is the execution seam shared by the typed pattern
// resources. It wraps the deployment state machine (internal/engine) for a
// Deployment that targets EXACTLY ONE host — the single-target rollout /
// rollback-on-failure / drift / destroy flow of DESIGN §10.2. Cluster (multi
// host WSFC) rollout is intentionally out of scope for the typed resources; the
// spec-string labdeploy_deployment resource remains the surface for that.
type singleTargetDeployment struct {
	dep *spec.Deployment
}

// newSingleTargetDeployment wraps d after asserting the single-host invariant.
// A typed pattern resource models one lab machine, so more (or zero) hosts is a
// configuration error surfaced before any transport dial.
func newSingleTargetDeployment(d *spec.Deployment) (*singleTargetDeployment, error) {
	if d == nil {
		return nil, fmt.Errorf("[ERR_SPEC_INVALID] deployment is nil")
	}
	if len(d.Target.Hosts) != 1 {
		return nil, fmt.Errorf(
			"[ERR_SPEC_INVALID] single-target deployment requires exactly one host, got %d",
			len(d.Target.Hosts))
	}
	return &singleTargetDeployment{dep: d}, nil
}

// host returns the single resolved target host.
func (s *singleTargetDeployment) host() string { return s.dep.Target.Hosts[0] }

// id returns the stable resource id `<metadata.name>@<host>` (lower-cased),
// matching the identity scheme used by labdeploy_deployment.
func (s *singleTargetDeployment) id() string {
	return deploymentID(s.dep)
}

// apply runs the create/upgrade state machine (DESIGN §10.1/§10.2). It returns
// the post-apply status and any non-fatal engine warnings so the caller can
// surface them as Terraform warning diagnostics.
func (s *singleTargetDeployment) apply(ctx context.Context) (*engine.Status, []string, error) {
	eng := engine.New()
	st, err := eng.Deploy(ctx, s.dep)
	return st, eng.Warnings, err
}

// reconfigure re-applies pattern configuration to the current release without a
// full re-deploy. It backs configuration-only Terraform updates, which change
// mutable settings (service description, start type, recovery, environment)
// without touching artifact.version/checksum and would therefore hit the engine
// idempotency short-circuit (DESIGN §10.1 step 2) if routed through apply/Deploy.
func (s *singleTargetDeployment) reconfigure(ctx context.Context) (*engine.Status, []string, error) {
	eng := engine.New()
	st, err := eng.Reconfigure(ctx, s.dep)
	return st, eng.Warnings, err
}

// read refreshes status from the target manifest (DESIGN §10.4). A nil status
// with a nil error means the manifest is absent — the resource is gone.
func (s *singleTargetDeployment) read(ctx context.Context) (*engine.Status, []string, error) {
	eng := engine.New()
	st, err := eng.ReadStatus(ctx, s.dep)
	return st, eng.Warnings, err
}

// destroy tears the deployment down per the destroy_mode (DESIGN §10.5).
func (s *singleTargetDeployment) destroy(ctx context.Context, mode string) ([]string, error) {
	eng := engine.New()
	err := eng.Destroy(ctx, s.dep, mode)
	return eng.Warnings, err
}
