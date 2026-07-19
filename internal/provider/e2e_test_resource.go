package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

var (
	_ resource.Resource                     = (*E2ETestResource)(nil)
	_ resource.ResourceWithConfigure        = (*E2ETestResource)(nil)
	_ resource.ResourceWithConfigValidators = (*E2ETestResource)(nil)
)

func NewE2ETestResource() resource.Resource { return &E2ETestResource{} }

type E2ETestResource struct {
	pd *providerData
	// newEngine is swappable for provider-level fake-transport tests so
	// E2ETestResource.Create/Delete can be exercised end-to-end without a live
	// target (DESIGN §17 seam). nil ⇒ engine.New().
	newEngine func() *engine.Engine
}

// engineNew returns the injected engine factory result, or a real engine.
func (r *E2ETestResource) engineNew() *engine.Engine {
	if r.newEngine != nil {
		return r.newEngine()
	}
	return engine.New()
}

// ConfigValidators enforces the VAL-06 exactly-one-of(spec, spec_file) rule at
// the Terraform config layer (DESIGN §14).
func (r *E2ETestResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{exactlyOneOfSpecValidator{}}
}

type e2eModel struct {
	ID                types.String `tfsdk:"id"`
	Spec              types.String `tfsdk:"spec"`
	SpecFile          types.String `tfsdk:"spec_file"`
	Variables         types.Map    `tfsdk:"variables"`
	DeploymentID      types.String `tfsdk:"deployment_id"`
	Triggers          types.Map    `tfsdk:"triggers"`
	FailOnTestFailure types.Bool   `tfsdk:"fail_on_test_failure"`
	Passed            types.Bool   `tfsdk:"passed"`
	ExitCode          types.Int64  `tfsdk:"exit_code"`
	TotalTests        types.Int64  `tfsdk:"total_tests"`
	PassedTests       types.Int64  `tfsdk:"passed_tests"`
	FailedTests       types.Int64  `tfsdk:"failed_tests"`
	SkippedTests      types.Int64  `tfsdk:"skipped_tests"`
	ResultsDir        types.String `tfsdk:"results_dir"`
	DurationSeconds   types.Int64  `tfsdk:"duration_seconds"`
	Summary           types.String `tfsdk:"summary"`
}

func (r *E2ETestResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_e2e_test"
}

func (r *E2ETestResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Runs a test package on a target once per triggers change (DESIGN §5.3). Tests execute during apply. A completed run is immutable history: every argument except `deployment_id` is RequiresReplace, so changing the spec, variables, `fail_on_test_failure`, or `triggers` replaces the resource and re-runs the tests (DESIGN §5.3 \"Update is never in-place\").",
		Attributes: map[string]schema.Attribute{
			"spec": schema.StringAttribute{Optional: true,
				MarkdownDescription: "Inline YAML/JSON TestRun spec.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"spec_file": schema.StringAttribute{Optional: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"variables": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplace()}},
			"deployment_id": schema.StringAttribute{Optional: true,
				MarkdownDescription: "Set to `labdeploy_deployment.x.id` purely to create the dependency edge so Terraform Core orders this test after the deployment (DESIGN §5.3). No RequiresReplace: changing it never forces a re-run — `triggers` owns re-execution."},
			"triggers": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "Optional (DESIGN §5.3). Any change forces re-run (RequiresReplace); pipelines set `{ run = var.build_id }` to force a re-run per build.",
				PlanModifiers:       []planmodifier.Map{mapRequiresReplace{}}},
			"fail_on_test_failure": schema.BoolAttribute{Optional: true, Computed: true,
				Default:             booldefault.StaticBool(true),
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
				MarkdownDescription: "false ⇒ apply succeeds even when tests fail; gate on `passed` output instead. RequiresReplace: a completed run is immutable, so flipping this re-runs the tests."},
			"id":               schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"passed":           schema.BoolAttribute{Computed: true},
			"exit_code":        schema.Int64Attribute{Computed: true},
			"total_tests":      schema.Int64Attribute{Computed: true},
			"passed_tests":     schema.Int64Attribute{Computed: true},
			"failed_tests":     schema.Int64Attribute{Computed: true},
			"skipped_tests":    schema.Int64Attribute{Computed: true},
			"results_dir":      schema.StringAttribute{Computed: true},
			"duration_seconds": schema.Int64Attribute{Computed: true},
			"summary":          schema.StringAttribute{Computed: true},
		},
	}
}

// mapRequiresReplace: any triggers delta ⇒ replace (re-run tests).
type mapRequiresReplace struct{}

func (mapRequiresReplace) Description(context.Context) string         { return "replace on change" }
func (mapRequiresReplace) MarkdownDescription(context.Context) string { return "replace on change" }
func (mapRequiresReplace) PlanModifyMap(ctx context.Context, req planmodifier.MapRequest, resp *planmodifier.MapResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if !req.StateValue.Equal(req.PlanValue) {
		resp.RequiresReplace = true
	}
}

func (r *E2ETestResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.pd = req.ProviderData.(*providerData)
}

func (r *E2ETestResource) resolveSpec(ctx context.Context, m *e2eModel) (*spec.TestRun, error) {
	hasSpec := !m.Spec.IsNull() && m.Spec.ValueString() != ""
	hasFile := !m.SpecFile.IsNull() && m.SpecFile.ValueString() != ""
	if hasSpec == hasFile {
		return nil, fmt.Errorf("[ERR_SPEC_INVALID] exactly one of spec/spec_file must be set")
	}
	raw := m.Spec.ValueString()
	if hasFile {
		b, err := os.ReadFile(m.SpecFile.ValueString())
		if err != nil {
			return nil, fmt.Errorf("[ERR_SPEC_INVALID] spec_file: %w", err)
		}
		raw = string(b)
	}
	vars := map[string]string{}
	if !m.Variables.IsNull() {
		if diag := m.Variables.ElementsAs(ctx, &vars, false); diag.HasError() {
			return nil, fmt.Errorf("[ERR_SPEC_INVALID] variables: %v", diag.Errors())
		}
	}
	t, _, err := spec.ParseTestRunLenient(raw, vars)
	if err != nil {
		return nil, err // already [ERR_SPEC_INVALID]-coded by the spec package
	}
	if r.pd != nil && r.pd.DefaultTarget != nil {
		spec.MergeTargetDefaults(&t.Target, r.pd.DefaultTarget)
	}
	if err := spec.ValidateTestRun(t); err != nil { // validate post-merge (DESIGN §6.2)
		return nil, err // already [ERR_SPEC_INVALID]-coded
	}
	return t, nil
}

func (r *E2ETestResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan e2eModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tr, err := r.resolveSpec(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError(codedSummary(err, "ERR_SPEC_INVALID", "invalid TestRun spec"), err.Error())
		return
	}
	eng := r.engineNew()
	out, err := eng.RunTest(ctx, tr)
	for _, w := range eng.Warnings {
		resp.Diagnostics.AddWarning("labdeploy", w)
	}
	if err != nil && out == nil {
		resp.Diagnostics.AddError(codedSummary(err, "ERR_TEST_FAILED", "test execution failed"), err.Error())
		return
	}
	plan.ID = types.StringValue(tr.Metadata.Name + "@" + tr.Target.Hosts[0])
	plan.Passed = types.BoolValue(out.Passed)
	plan.ExitCode = types.Int64Value(int64(out.ExitCode))
	plan.TotalTests = types.Int64Value(int64(out.Total))
	plan.PassedTests = types.Int64Value(int64(out.PassedTests))
	plan.FailedTests = types.Int64Value(int64(out.FailedTests))
	plan.SkippedTests = types.Int64Value(int64(out.SkippedTests))
	plan.ResultsDir = types.StringValue(out.ResultsDir)
	plan.DurationSeconds = types.Int64Value(int64(out.DurationSeconds))
	plan.Summary = types.StringValue(out.Summary)
	// State is persisted BEFORE pass gating so results survive a failed apply
	// (E2E-05 collect-then-fail).
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if err != nil { // parse error surfaced alongside collected results
		resp.Diagnostics.AddError(codedSummary(err, "ERR_TEST_FAILED", "test results invalid"), err.Error())
		return
	}
	if !out.Passed && plan.FailOnTestFailure.ValueBool() {
		resp.Diagnostics.AddError("[ERR_TEST_FAILED] E2E tests failed", out.Summary)
	}
}

// Read is a no-op: a completed test run is immutable history.
func (r *E2ETestResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state e2eModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only fires when `deployment_id` (the sole non-RequiresReplace argument)
// changes without any other edit — persist the new plan but carry the immutable
// computed outcomes forward; the test does NOT re-run (every real argument is
// RequiresReplace, so a re-run goes through Create on replacement).
func (r *E2ETestResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan e2eModel
	var state e2eModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// carry forward computed outcomes
	plan.ID = state.ID
	plan.Passed = state.Passed
	plan.ExitCode = state.ExitCode
	plan.TotalTests = state.TotalTests
	plan.PassedTests = state.PassedTests
	plan.FailedTests = state.FailedTests
	plan.SkippedTests = state.SkippedTests
	plan.ResultsDir = state.ResultsDir
	plan.DurationSeconds = state.DurationSeconds
	plan.Summary = state.Summary
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete best-effort removes the remote test directory the run created
// (`<install_root>/<name>-tests`). It NEVER fails destroy (DESIGN §5.3): a spec
// that no longer resolves, or a transport/cleanup error, is surfaced as a
// WARNING only so Terraform can still drop the resource from state.
func (r *E2ETestResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state e2eModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tr, err := r.resolveSpec(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddWarning(codedSummary(err, "ERR_SPEC_INVALID", "e2e test cleanup skipped"),
			fmt.Sprintf("could not resolve spec to locate the remote test dir; skipping best-effort cleanup: %v", err))
		return
	}
	eng := r.engineNew()
	if derr := eng.DeleteTestDir(ctx, tr); derr != nil {
		resp.Diagnostics.AddWarning(codedSummary(derr, "ERR_CONNECT", "remote test dir cleanup failed"),
			fmt.Sprintf("best-effort removal of the remote test dir failed (destroy still succeeds): %v", derr))
	}
	for _, w := range eng.Warnings {
		resp.Diagnostics.AddWarning("labdeploy", w)
	}
}

// codedSummary preserves the engine's coded-error contract in a diagnostic
// Summary ("[ERR_*] <short>"): the code is taken from err's `[ERR_*]` prefix
// when present, else fallbackCode is used. The full err text still lands in the
// diagnostic Detail.
func codedSummary(err error, fallbackCode, short string) string {
	code := extractCode(err)
	if code == "" {
		code = fallbackCode
	}
	return "[" + code + "] " + short
}

// extractCode returns the `ERR_*` token from a leading `[ERR_*]` prefix, or "".
func extractCode(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.HasPrefix(s, "[") {
		if i := strings.IndexByte(s, ']'); i > 1 {
			return s[1:i]
		}
	}
	return ""
}
