//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// readDriftDeploymentModel mirrors the (unexported) provider.deploymentModel tfsdk
// shape so this external package can build tfsdk.State/Plan through the REAL resource
// schema by tfsdk tag (the framework maps by tag, not Go type identity). Stage 5.3
// covers Read drift reconciliation and the insecure-transport WARN surface
// (DESIGN §10.4, §11).
type readDriftDeploymentModel struct {
	ID              types.String   `tfsdk:"id"`
	Name            types.String   `tfsdk:"name"`
	Spec            types.String   `tfsdk:"spec"`
	SpecFile        types.String   `tfsdk:"spec_file"`
	Variables       types.Map      `tfsdk:"variables"`
	VersionOverride types.String   `tfsdk:"version_override"`
	DestroyMode     types.String   `tfsdk:"destroy_mode"`
	SpecHash        types.String   `tfsdk:"spec_hash"`
	ResolvedSpec    types.String   `tfsdk:"resolved_spec"`
	DeployedVersion types.String   `tfsdk:"deployed_version"`
	PreviousVersion types.String   `tfsdk:"previous_version"`
	Hosts           types.List     `tfsdk:"hosts"`
	ReleasePath     types.String   `tfsdk:"release_path"`
	ServiceStatus   types.String   `tfsdk:"service_status"`
	Timeouts        timeouts.Value `tfsdk:"timeouts"`
}

func readDriftNullTimeouts() timeouts.Value {
	return timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
		"create": types.StringType,
		"update": types.StringType,
		"delete": types.StringType,
	})}
}

// readDriftWSSpecYAML is a valid inline winrm/windows deployment spec. Because it is set
// as the inline `spec` attribute (with resolved_spec null), stateSpec treats it as a
// faithful verbatim, VERIFIED deployed identity — so Read is allowed to treat a missing
// manifest as a genuine absence (DESIGN §10.4).
func readDriftWSSpecYAML(version string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: %s
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, version, 0)
}

// readDriftVerifiedModel returns a Read-ready state model whose inline `spec` makes
// stateSpec VERIFIED, so Read may honestly reconcile drift against the manifest.
func readDriftVerifiedModel(version string) *readDriftDeploymentModel {
	return &readDriftDeploymentModel{
		Spec:         types.StringValue(readDriftWSSpecYAML(version)),
		SpecFile:     types.StringNull(),
		ResolvedSpec: types.StringNull(),
		SpecHash:     types.StringNull(),
		DestroyMode:  types.StringNull(),
		Hosts:        types.ListNull(types.StringType),
		Variables:    types.MapNull(types.StringType),
		Timeouts:     readDriftNullTimeouts(),
	}
}

// readDriftReadReqResp builds a real resource.ReadRequest/ReadResponse whose State
// carries the given model through the resource's OWN schema, so Read runs end-to-end
// and the resulting State can be inspected (RemoveResource nulls State.Raw).
func readDriftReadReqResp(r *provider.DeploymentResource, m *readDriftDeploymentModel) (resource.ReadRequest, *resource.ReadResponse, error) {
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	st := tfsdk.State{Schema: sr.Schema}
	if d := st.Set(ctx, m); d.HasError() {
		return resource.ReadRequest{}, nil, fmt.Errorf("build state: %v", d)
	}
	return resource.ReadRequest{State: st}, &resource.ReadResponse{State: st}, nil
}

// --- scenario state -----------------------------------------------------------

type readDriftState struct {
	resource     *provider.DeploymentResource
	readResp     *resource.ReadResponse
	refreshed    *readDriftDeploymentModel
	planResp     *resource.ModifyPlanResponse
	createResp   *resource.CreateResponse
	warnSetting  string
	expectedWarn []string
}

// runRead drives the REAL DeploymentResource.Read on a verified model and captures the
// response + refreshed state (when not removed).
func (s *readDriftState) runRead(version string) error {
	req, resp, err := readDriftReadReqResp(s.resource, readDriftVerifiedModel(version))
	if err != nil {
		return err
	}
	s.resource.Read(context.Background(), req, resp)
	s.readResp = resp
	if !resp.State.Raw.IsNull() {
		var m readDriftDeploymentModel
		if d := resp.State.Get(context.Background(), &m); d.HasError() {
			return fmt.Errorf("read refreshed state: %v", d)
		}
		s.refreshed = &m
	}
	return nil
}

// --- scenario 1: absent manifest removes resource -----------------------------

func (s *readDriftState) givenVerifiedMissingManifest() error {
	// Real engine seam whose ReadStatus reports an absent manifest (nil,nil).
	s.resource = provider.NewDeploymentResourceReadAbsent()
	return nil
}

// --- scenario 2: failed op marker forces plan change --------------------------

func (s *readDriftState) givenVerifiedFailedLastOperation() error {
	// Drive the REAL drift-reconciliation impl: a manifest whose last_operation.result
	// is "failed" reconciles to a "<current_version>!failed" deployed_version. The suffix
	// is produced by engine.ReconcileManifest, not fabricated by the test.
	m := &engine.Manifest{
		CurrentVersion: "1.0.0",
		LastOperation:  engine.LastOp{Type: "deploy", Result: "failed", Started: "2026-01-01T00:00:00Z"},
	}
	present, deployedVersion, _ := engine.ReconcileManifest(m)
	if !present {
		return fmt.Errorf("a present manifest must reconcile as present")
	}
	if !strings.HasSuffix(deployedVersion, engine.FailedMarker) {
		return fmt.Errorf("engine.ReconcileManifest must mark a failed op with %q, got %q", engine.FailedMarker, deployedVersion)
	}
	s.resource = provider.NewDeploymentResourceReadStatus(&engine.Status{
		DeployedVersion: deployedVersion, ServiceStatus: "stopped", Hosts: []string{"lab-01"},
	})
	return nil
}

// --- scenario 3: unreachable target is a loud Read error ----------------------

func (s *readDriftState) givenVerifiedDialFailure() error {
	// Real engine wired to a fake transport whose Connect returns transport.ErrConnect,
	// so ReadStatus fails through the real wrapTransportErr taxonomy path.
	s.resource = provider.NewDeploymentResourceWithConnectError()
	return nil
}

// --- shared When for the Read scenarios ---------------------------------------

func (s *readDriftState) whenReadRefreshRuns() error {
	return s.runRead("1.0.0")
}

// --- Then steps (Read scenarios) ----------------------------------------------

func (s *readDriftState) thenResourceRemoved() error {
	if s.readResp == nil {
		return fmt.Errorf("Read was not driven")
	}
	if !s.readResp.State.Raw.IsNull() {
		return fmt.Errorf("absent manifest must RemoveResource (State.Raw null) so the next plan is a create")
	}
	return nil
}

func (s *readDriftState) thenReadNoError() error {
	if s.readResp.Diagnostics.HasError() {
		return fmt.Errorf("Read must not error, got %v", s.readResp.Diagnostics.Errors())
	}
	return nil
}

func (s *readDriftState) thenDeployedVersionFailedSuffix() error {
	if s.readResp.Diagnostics.HasError() {
		return fmt.Errorf("failed-marker Read must not error, got %v", s.readResp.Diagnostics.Errors())
	}
	if s.refreshed == nil {
		return fmt.Errorf("failed-marker Read must retain state, not remove the resource")
	}
	if got := s.refreshed.DeployedVersion.ValueString(); !strings.HasSuffix(got, engine.FailedMarker) {
		return fmt.Errorf("deployed_version must carry the %q suffix, got %q", engine.FailedMarker, got)
	}
	return nil
}

func (s *readDriftState) thenSubsequentPlanNonEmpty() error {
	if s.refreshed == nil {
		return fmt.Errorf("no refreshed state to plan against")
	}
	// Same (unchanged) config against the failed-marked prior state. Without the forcing
	// behavior Terraform would plan no changes; ModifyPlan must instead mark the tainted
	// computed outputs unknown so Terraform schedules a converging Update.
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	s.resource.Schema(ctx, resource.SchemaRequest{}, &sr)

	plan := readDriftVerifiedModel("1.0.0")
	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		return fmt.Errorf("build plan: %v", d)
	}
	st := tfsdk.State{Schema: sr.Schema}
	s.refreshed.Timeouts = readDriftNullTimeouts()
	if d := st.Set(ctx, s.refreshed); d.HasError() {
		return fmt.Errorf("build state: %v", d)
	}
	resp := &resource.ModifyPlanResponse{Plan: p}
	s.resource.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:   p,
		State:  st,
		Config: tfsdk.Config{Schema: sr.Schema, Raw: p.Raw},
	}, resp)
	if resp.Diagnostics.HasError() {
		return fmt.Errorf("ModifyPlan on failed-marked state must not error, got %v", resp.Diagnostics.Errors())
	}
	var planned readDriftDeploymentModel
	if d := resp.Plan.Get(ctx, &planned); d.HasError() {
		return fmt.Errorf("read planned model: %v", d)
	}
	if !planned.DeployedVersion.IsUnknown() {
		return fmt.Errorf("failed marker must force deployed_version unknown (non-empty converging plan), got %q",
			planned.DeployedVersion.ValueString())
	}
	if !planned.ServiceStatus.IsUnknown() {
		return fmt.Errorf("failed marker must force service_status unknown so the plan re-applies")
	}
	return nil
}

func (s *readDriftState) thenReadErrorNotWarning() error {
	if s.readResp == nil {
		return fmt.Errorf("Read was not driven")
	}
	if !s.readResp.Diagnostics.HasError() {
		return fmt.Errorf("unreachable target must produce a Read ERROR diagnostic, not a warning")
	}
	if s.readResp.Diagnostics.WarningsCount() != 0 {
		return fmt.Errorf("unreachable target must not be downgraded to a WARN, got %d warnings",
			s.readResp.Diagnostics.WarningsCount())
	}
	return nil
}

func (s *readDriftState) thenPriorStateIntact() error {
	if s.readResp.State.Raw.IsNull() {
		return fmt.Errorf("unreachable target must LEAVE prior state intact — the resource must NOT be removed")
	}
	return nil
}

// --- scenario 4: exactly one insecure-transport warning -----------------------

func (s *readDriftState) givenInsecureTransport(setting string) error {
	var target spec.Target
	switch setting {
	case "winrm_insecure_skip":
		insecure := true
		target = spec.Target{Transport: spec.TransportWinRM, WinRM: spec.WinRMOpts{InsecureSkipVerify: &insecure}}
	case "ssh_unpinned_hostkey":
		target = spec.Target{Transport: spec.TransportSSH, SSH: spec.SSHOpts{HostKey: ""}}
	default:
		return fmt.Errorf("unknown insecure setting %q", setting)
	}
	// Seed the surfaced warnings from the REAL single source of truth so the wording is
	// not duplicated by the test (DESIGN §11).
	warns := engine.InsecureTransportWarnings(&spec.Deployment{Target: target})
	if len(warns) != 1 {
		return fmt.Errorf("precondition: expected exactly one engine insecure warning for %q, got %v", setting, warns)
	}
	s.warnSetting = setting
	s.expectedWarn = warns
	s.resource = provider.NewDeploymentResourceApplyWarnings(warns)
	return nil
}

func (s *readDriftState) whenApplyRuns() error {
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	s.resource.Schema(ctx, resource.SchemaRequest{}, &sr)
	plan := readDriftVerifiedModel("1.0.0")
	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		return fmt.Errorf("build plan: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sr.Schema}}
	s.resource.Create(ctx, resource.CreateRequest{Plan: p}, resp)
	s.createResp = resp
	return nil
}

func (s *readDriftState) thenExactlyOneWarnNaming(names string) error {
	if s.createResp == nil {
		return fmt.Errorf("apply was not driven")
	}
	if got := s.createResp.Diagnostics.WarningsCount(); got != 1 {
		return fmt.Errorf("apply must emit exactly one WARN diagnostic, got %d (%v)", got, s.createResp.Diagnostics.Warnings())
	}
	w := s.createResp.Diagnostics.Warnings()[0]
	if !strings.Contains(w.Detail(), names) {
		return fmt.Errorf("WARN diagnostic must name the insecure setting %q, got detail %q", names, w.Detail())
	}
	return nil
}

func (s *readDriftState) thenApplyNoError() error {
	if s.createResp.Diagnostics.HasError() {
		return fmt.Errorf("insecure apply must not error, got %v", s.createResp.Diagnostics.Errors())
	}
	return nil
}

func InitializeScenario_terraform_provider_surface_read_drift_reconciliation_and_destroy_modes(ctx *godog.ScenarioContext) {
	st := &readDriftState{}
	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		*st = readDriftState{}
		// Specs reference password_env: LABDEPLOY_PASSWORD; set it so parse/validate succeed.
		if err := os.Setenv("LABDEPLOY_PASSWORD", "pw"); err != nil {
			return ctx, err
		}
		return ctx, nil
	})

	// Scenario 1
	ctx.Step(`^a verified deployment in state whose target manifest is missing$`, st.givenVerifiedMissingManifest)
	// Scenario 2
	ctx.Step(`^a verified deployment in state whose manifest last_operation result is failed$`, st.givenVerifiedFailedLastOperation)
	// Scenario 3
	ctx.Step(`^a verified deployment in state whose target fails to dial$`, st.givenVerifiedDialFailure)
	// Shared When for Read scenarios
	ctx.Step(`^the deployment Read refresh runs via a fake transport$`, st.whenReadRefreshRuns)

	ctx.Step(`^the resource is removed from state so the next plan is a create$`, st.thenResourceRemoved)
	ctx.Step(`^the Read refresh raises no error$`, st.thenReadNoError)
	ctx.Step(`^deployed_version carries the "!failed" suffix$`, st.thenDeployedVersionFailedSuffix)
	ctx.Step(`^the subsequent plan is non-empty and converging$`, st.thenSubsequentPlanNonEmpty)
	ctx.Step(`^the Read refresh returns an ERROR diagnostic and not a warning$`, st.thenReadErrorNotWarning)
	ctx.Step(`^the prior state is left intact and the resource is not removed$`, st.thenPriorStateIntact)

	// Scenario 4
	ctx.Step(`^a deployment whose transport enables the insecure setting "([^"]*)"$`, st.givenInsecureTransport)
	ctx.Step(`^an apply runs$`, st.whenApplyRuns)
	ctx.Step(`^exactly one WARN diagnostic naming "([^"]*)" is emitted$`, st.thenExactlyOneWarnNaming)
	ctx.Step(`^the apply raises no error$`, st.thenApplyNoError)
}

func TestE2E_terraform_provider_surface_read_drift_reconciliation_and_destroy_modes(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_terraform_provider_surface_read_drift_reconciliation_and_destroy_modes,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"terraform_provider_surface_read_drift_reconciliation_and_destroy_modes.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run read-drift-reconciliation-and-destroy-modes feature tests")
	}
}
