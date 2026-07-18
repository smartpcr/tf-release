//go:build e2e

package e2e

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// e2eCrudDeploymentModel mirrors the (unexported) provider.deploymentModel tfsdk
// shape so this external package can build tfsdk.Plan/State through the REAL resource
// schema by tfsdk tag (the framework maps by tag, not Go type identity). It includes
// the timeouts block field so a Set against the real schema (which declares that
// block) round-trips faithfully (DESIGN §5.2).
type e2eCrudDeploymentModel struct {
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

func e2eNullTimeouts() timeouts.Value {
	return timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
		"create": types.StringType,
		"update": types.StringType,
		"delete": types.StringType,
	})}
}

// --- spec fixtures (mirror the in-package helpers so validation passes) --------

func e2eWSSpecYAMLHost(version, host string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["%s"]
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
`, host, version, 0)
}

func e2eWSSpecServiceYAML(service, host string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["%s"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: windows_service
  service_name: %s
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, host, 0, service)
}

func e2eDotnetSpecYAML(host string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["%s"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: dotnet_api
  service_name: SampleSvc
  launcher: exe
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, host, 0)
}

func e2eConsoleSpecYAML(os, host string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["%s"]
  os: %s
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: console_app
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, host, os, 0)
}

func e2eClusterSpecYAMLHosts(hosts ...string) string {
	hl := `["` + strings.Join(hosts, `", "`) + `"]`
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: %s
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: cluster_generic_service
  service_name: SampleSvc
  role_name: SampleRole
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, hl, 0)
}

// --- scenario state -----------------------------------------------------------

type crudState struct {
	baselineYAML string // becomes the persisted resolved_spec snapshot (prior)
	modifiedYAML string // becomes the new plan's inline spec

	modifyResp *resource.ModifyPlanResponse

	createSummary string
	createErrored bool

	idOriginal  string
	idReordered string
	idExpected  string

	tCreate, tUpdate, tDelete time.Duration

	importErrored bool
	importDetail  string
}

// runPlan drives the REAL DeploymentResource.ModifyPlan with a prior state carrying
// the baseline deployment (as a verbatim inline spec — the "verified" legacy prior
// that priorFromState reconstructs faithfully) and a plan carrying the modified inline
// spec, so the immutableKey comparison runs against the real resolved specs.
func runPlan(baselineYAML, modifiedYAML string) (*resource.ModifyPlanResponse, error) {
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	(&provider.DeploymentResource{}).Schema(ctx, resource.SchemaRequest{}, &sr)

	plan := &e2eCrudDeploymentModel{
		Spec:         types.StringValue(modifiedYAML),
		SpecFile:     types.StringNull(),
		Variables:    types.MapNull(types.StringType),
		ResolvedSpec: types.StringNull(),
		SpecHash:     types.StringNull(),
		Hosts:        types.ListNull(types.StringType),
		Timeouts:     e2eNullTimeouts(),
	}
	state := &e2eCrudDeploymentModel{
		Spec:         types.StringValue(baselineYAML),
		SpecFile:     types.StringNull(),
		Variables:    types.MapNull(types.StringType),
		ResolvedSpec: types.StringNull(),
		SpecHash:     types.StringValue("prior-hash"),
		Hosts:        types.ListNull(types.StringType),
		Timeouts:     e2eNullTimeouts(),
	}

	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		return nil, fmt.Errorf("build plan: %v", d)
	}
	st := tfsdk.State{Schema: sr.Schema}
	if d := st.Set(ctx, state); d.HasError() {
		return nil, fmt.Errorf("build state: %v", d)
	}
	resp := &resource.ModifyPlanResponse{Plan: p}
	(&provider.DeploymentResource{}).ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:   p,
		State:  st,
		Config: tfsdk.Config{Schema: sr.Schema, Raw: p.Raw},
	}, resp)
	return resp, nil
}

// --- immutable-path / reorder steps -------------------------------------------

func (s *crudState) givenPriorSnapshot() error { return nil }

func (s *crudState) givenPlanChangesImmutableField(field string) error {
	switch field {
	case "pattern.type":
		s.baselineYAML = e2eWSSpecYAMLHost("1.0.0", "lab-01")
		s.modifiedYAML = e2eDotnetSpecYAML("lab-01")
	case "service_name":
		s.baselineYAML = e2eWSSpecServiceYAML("SampleSvc", "lab-01")
		s.modifiedYAML = e2eWSSpecServiceYAML("OtherSvc", "lab-01")
	case "target.hosts":
		s.baselineYAML = e2eWSSpecYAMLHost("1.0.0", "lab-01")
		s.modifiedYAML = e2eWSSpecYAMLHost("1.0.0", "lab-02")
	case "target.os":
		s.baselineYAML = e2eConsoleSpecYAML("windows", "lab-01")
		s.modifiedYAML = e2eConsoleSpecYAML("linux", "lab-01")
	default:
		return fmt.Errorf("unknown immutable field %q", field)
	}
	return nil
}

func (s *crudState) givenPriorSnapshotWithHosts(hosts string) error {
	s.baselineYAML = e2eClusterSpecYAMLHosts(splitHosts(hosts)...)
	return nil
}

func (s *crudState) givenPlanReorderedTo(hosts string) error {
	s.modifiedYAML = e2eClusterSpecYAMLHosts(splitHosts(hosts)...)
	return nil
}

func (s *crudState) whenPlanComputed() error {
	resp, err := runPlan(s.baselineYAML, s.modifiedYAML)
	if err != nil {
		return err
	}
	if resp.Diagnostics.HasError() {
		return fmt.Errorf("plan computation errored: %v", resp.Diagnostics.Errors())
	}
	s.modifyResp = resp
	return nil
}

func (s *crudState) thenReplaceForced() error {
	if s.modifyResp == nil {
		return fmt.Errorf("plan was not computed")
	}
	if len(s.modifyResp.RequiresReplace) == 0 {
		return fmt.Errorf("expected the immutable-path change to force RequiresReplace, but it did not")
	}
	return nil
}

func (s *crudState) thenReplaceDoesNotFire() error {
	if s.modifyResp == nil {
		return fmt.Errorf("plan was not computed")
	}
	if len(s.modifyResp.RequiresReplace) != 0 {
		return fmt.Errorf("host reorder must NOT force replacement, got RequiresReplace=%v", s.modifyResp.RequiresReplace)
	}
	return nil
}

func splitHosts(csv string) []string {
	parts := strings.Split(csv, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// --- coded diagnostic (ERR_CONNECT) steps -------------------------------------

func (s *crudState) givenEnginePreflightFails() error { return nil }

func (s *crudState) whenCreateSurfacesFailure() error {
	ctx := context.Background()
	r := provider.NewDeploymentResourceWithConnectError()
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)

	plan := &e2eCrudDeploymentModel{
		Spec:         types.StringValue(e2eWSSpecYAMLHost("1.0.0", "lab-01")),
		SpecFile:     types.StringNull(),
		Variables:    types.MapNull(types.StringType),
		ResolvedSpec: types.StringNull(),
		SpecHash:     types.StringNull(),
		Hosts:        types.ListNull(types.StringType),
		Timeouts:     e2eNullTimeouts(),
	}
	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		return fmt.Errorf("build plan: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sr.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: p}, resp)
	s.createErrored = resp.Diagnostics.HasError()
	if s.createErrored {
		s.createSummary = resp.Diagnostics.Errors()[0].Summary()
	}
	return nil
}

func (s *crudState) thenSummaryBeginsWith(prefix string) error {
	if !s.createErrored {
		return fmt.Errorf("Create must surface the engine deploy failure")
	}
	if !strings.HasPrefix(s.createSummary, prefix) {
		return fmt.Errorf("deploy failure Summary must begin with %q, got %q", prefix, s.createSummary)
	}
	return nil
}

// --- deterministic id steps ---------------------------------------------------

func (s *crudState) givenSpecNameHosts(name, hosts string) error {
	hs := splitHosts(hosts)
	d1 := &spec.Deployment{Metadata: spec.Metadata{Name: name}, Target: spec.Target{Hosts: hs}}

	rev := make([]string, len(hs))
	for i := range hs {
		rev[i] = hs[len(hs)-1-i]
	}
	d2 := &spec.Deployment{Metadata: spec.Metadata{Name: name}, Target: spec.Target{Hosts: rev}}

	s.idOriginal = provider.DeploymentIDForE2E(d1)
	s.idReordered = provider.DeploymentIDForE2E(d2)

	lowered := make([]string, len(hs))
	for i, h := range hs {
		lowered[i] = strings.ToLower(h)
	}
	sort.Strings(lowered)
	sum := sha1.Sum([]byte(strings.Join(lowered, ",") + "/" + name))
	s.idExpected = hex.EncodeToString(sum[:])[:12] + ":" + name
	return nil
}

func (s *crudState) whenIDComputed() error { return nil }

func (s *crudState) thenIDMatchesFormula() error {
	if s.idOriginal != s.idExpected {
		return fmt.Errorf("id = %q, want the sha1 formula result %q", s.idOriginal, s.idExpected)
	}
	return nil
}

func (s *crudState) thenIDByteIdenticalReordered() error {
	if s.idOriginal != s.idReordered {
		return fmt.Errorf("id must be byte-identical under host reorder: %q != %q", s.idOriginal, s.idReordered)
	}
	return nil
}

// --- timeout defaults steps ---------------------------------------------------

func (s *crudState) givenConfigNoTimeouts() error { return nil }

func (s *crudState) whenTimeoutDefaultsRead() error {
	s.tCreate, s.tUpdate, s.tDelete = provider.DefaultTimeoutsForE2E()
	return nil
}

func (s *crudState) thenTimeoutDefaults() error {
	if s.tCreate != 30*time.Minute {
		return fmt.Errorf("create default = %s, want 30m", s.tCreate)
	}
	if s.tUpdate != 30*time.Minute {
		return fmt.Errorf("update default = %s, want 30m", s.tUpdate)
	}
	if s.tDelete != 15*time.Minute {
		return fmt.Errorf("delete default = %s, want 15m", s.tDelete)
	}
	return nil
}

// --- import-unsupported steps -------------------------------------------------

func (s *crudState) givenDeploymentResource() error { return nil }

func (s *crudState) whenImportInvoked() error {
	r := &provider.DeploymentResource{}
	resp := &resource.ImportStateResponse{}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "anything"}, resp)
	s.importErrored = resp.Diagnostics.HasError()
	if s.importErrored {
		s.importDetail = resp.Diagnostics.Errors()[0].Detail()
	}
	return nil
}

func (s *crudState) thenImportError(msg string) error {
	if !s.importErrored {
		return fmt.Errorf("ImportState must surface an error")
	}
	if s.importDetail != msg {
		return fmt.Errorf("import error message = %q, want the pinned %q", s.importDetail, msg)
	}
	return nil
}

func InitializeScenario_terraform_provider_surface_deployment_resource_crud_and_plan_modifiers(ctx *godog.ScenarioContext) {
	st := &crudState{}
	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		*st = crudState{}
		// Specs reference password_env: LABDEPLOY_PASSWORD; set it so parse/validate succeed.
		if err := os.Setenv("LABDEPLOY_PASSWORD", "pw"); err != nil {
			return ctx, err
		}
		return ctx, nil
	})

	ctx.Step(`^a prior deployment snapshot in state$`, st.givenPriorSnapshot)
	ctx.Step(`^a plan that changes only the immutable field "([^"]*)"$`, st.givenPlanChangesImmutableField)
	ctx.Step(`^a prior deployment snapshot with hosts "([^"]*)"$`, st.givenPriorSnapshotWithHosts)
	ctx.Step(`^a plan with the same hosts reordered to "([^"]*)" and no other change$`, st.givenPlanReorderedTo)
	ctx.Step(`^the deployment plan is computed$`, st.whenPlanComputed)
	ctx.Step(`^the change forces RequiresReplace$`, st.thenReplaceForced)
	ctx.Step(`^RequiresReplace does not fire$`, st.thenReplaceDoesNotFire)

	ctx.Step(`^a deployment whose engine preflight fails with the ERR_CONNECT taxonomy code$`, st.givenEnginePreflightFails)
	ctx.Step(`^Create surfaces the failure$`, st.whenCreateSurfacesFailure)
	ctx.Step(`^the diagnostic Summary begins with "([^"]*)"$`, st.thenSummaryBeginsWith)

	ctx.Step(`^a spec with name "([^"]*)" and hosts "([^"]*)"$`, st.givenSpecNameHosts)
	ctx.Step(`^the deployment id is computed$`, st.whenIDComputed)
	ctx.Step(`^it equals the sha1\(sorted\(hosts\)\+"/"\+name\)\[0:12\] \+ ":" \+ name formula$`, st.thenIDMatchesFormula)
	ctx.Step(`^it is byte-identical when the same hosts are supplied in a different order$`, st.thenIDByteIdenticalReordered)

	ctx.Step(`^a deployment resource config with no explicit timeouts$`, st.givenConfigNoTimeouts)
	ctx.Step(`^the resource timeout defaults are read$`, st.whenTimeoutDefaultsRead)
	ctx.Step(`^create and update default to 30m and delete defaults to 15m$`, st.thenTimeoutDefaults)

	ctx.Step(`^the labdeploy_deployment resource$`, st.givenDeploymentResource)
	ctx.Step(`^terraform import is invoked on it$`, st.whenImportInvoked)
	ctx.Step(`^the error is "([^"]*)"$`, st.thenImportError)
}

func TestE2E_terraform_provider_surface_deployment_resource_crud_and_plan_modifiers(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_terraform_provider_surface_deployment_resource_crud_and_plan_modifiers,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"terraform_provider_surface_deployment_resource_crud_and_plan_modifiers.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run deployment_resource_crud_and_plan_modifiers e2e feature tests")
	}
}
