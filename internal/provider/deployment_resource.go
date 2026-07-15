package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

var (
	_ resource.Resource                = (*DeploymentResource)(nil)
	_ resource.ResourceWithConfigure   = (*DeploymentResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*DeploymentResource)(nil)
	_ resource.ResourceWithImportState = (*DeploymentResource)(nil)
)

func NewDeploymentResource() resource.Resource { return &DeploymentResource{} }

type DeploymentResource struct{ pd *providerData }

type deploymentModel struct {
	ID              types.String `tfsdk:"id"`
	Spec            types.String `tfsdk:"spec"`
	SpecFile        types.String `tfsdk:"spec_file"`
	Variables       types.Map    `tfsdk:"variables"`
	VersionOverride types.String `tfsdk:"version_override"`
	DestroyMode     types.String `tfsdk:"destroy_mode"`
	SpecHash        types.String `tfsdk:"spec_hash"`
	DeployedVersion types.String `tfsdk:"deployed_version"`
	PreviousVersion types.String `tfsdk:"previous_version"`
	Hosts           types.List   `tfsdk:"hosts"`
	ReleasePath     types.String `tfsdk:"release_path"`
	ServiceStatus   types.String `tfsdk:"service_status"`
}

func (r *DeploymentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deployment"
}

func (r *DeploymentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One application deployed to one host or one WSFC cluster (DESIGN §5.2).",
		Attributes: map[string]schema.Attribute{
			"spec":      schema.StringAttribute{Optional: true, MarkdownDescription: "Inline YAML/JSON Deployment spec. Exactly one of spec/spec_file."},
			"spec_file": schema.StringAttribute{Optional: true, MarkdownDescription: "Path to a YAML/JSON Deployment spec on the runner."},
			"variables": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "Values for ${var:NAME} substitution (DESIGN §6.6)."},
			"version_override": schema.StringAttribute{Optional: true,
				MarkdownDescription: "Overrides artifact.version after substitution — the pipeline's rollout/rollback lever."},
			"destroy_mode": schema.StringAttribute{Optional: true, Computed: true,
				Default:             stringdefault.StaticString("purge"),
				MarkdownDescription: "purge | unregister | abandon (DESIGN §10.6)."},
			"id":               schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"spec_hash":        schema.StringAttribute{Computed: true},
			"deployed_version": schema.StringAttribute{Computed: true},
			"previous_version": schema.StringAttribute{Computed: true},
			"hosts":            schema.ListAttribute{Computed: true, ElementType: types.StringType},
			"release_path":     schema.StringAttribute{Computed: true},
			"service_status":   schema.StringAttribute{Computed: true},
		},
	}
}

func (r *DeploymentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.pd = req.ProviderData.(*providerData)
}

// resolveSpec loads+parses the deployment for a model (DESIGN §6.1).
func (r *DeploymentResource) resolveSpec(ctx context.Context, m *deploymentModel) (*spec.Deployment, string, error) {
	hasSpec := !m.Spec.IsNull() && m.Spec.ValueString() != ""
	hasFile := !m.SpecFile.IsNull() && m.SpecFile.ValueString() != ""
	if hasSpec == hasFile {
		return nil, "", fmt.Errorf("[ERR_SPEC_INVALID] exactly one of spec/spec_file must be set")
	}
	raw := m.Spec.ValueString()
	if hasFile {
		b, err := os.ReadFile(m.SpecFile.ValueString())
		if err != nil {
			return nil, "", fmt.Errorf("[ERR_SPEC_INVALID] spec_file: %w", err)
		}
		raw = string(b)
	}
	vars := map[string]string{}
	if !m.Variables.IsNull() {
		if diag := m.Variables.ElementsAs(ctx, &vars, false); diag.HasError() {
			return nil, "", fmt.Errorf("[ERR_SPEC_INVALID] variables: %v", diag.Errors())
		}
	}
	d, hash, err := spec.ParseDeploymentLenient(raw, vars, m.VersionOverride.ValueString())
	if err != nil {
		return nil, "", fmt.Errorf("[ERR_SPEC_INVALID] %w", err)
	}
	if r.pd != nil && r.pd.DefaultTarget != nil {
		spec.MergeTargetDefaults(&d.Target, r.pd.DefaultTarget)
	}
	if err := spec.ValidateDeployment(d); err != nil { // validate post-merge (DESIGN §6.2)
		return nil, "", fmt.Errorf("[ERR_SPEC_INVALID] %w", err)
	}
	return d, hash, nil
}

// immutableKey captures the spec paths that force replacement (DESIGN §5.2):
// pattern.type, service_name, role_name, install_root, metadata.name,
// target.hosts, target.os.
func immutableKey(d *spec.Deployment) string {
	return strings.Join([]string{
		string(d.Pattern.Type), d.Pattern.ServiceName, d.Pattern.RoleName,
		d.Pattern.EffectiveInstallRoot(d.Target.OS), d.Metadata.Name,
		strings.ToLower(strings.Join(d.Target.Hosts, ",")), string(d.Target.OS),
	}, "|")
}

// ModifyPlan computes spec_hash drift and RequiresReplace on immutable paths.
func (r *DeploymentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() { // destroy
		return
	}
	var plan deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	newSpec, newHash, err := r.resolveSpec(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid spec", err.Error())
		return
	}
	plan.SpecHash = types.StringValue(newHash)
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)

	if req.State.Raw.IsNull() { // create
		return
	}
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	oldSpec, _, err := r.resolveSpec(ctx, &state)
	if err != nil {
		return // stored spec no longer parses; let Update surface it
	}
	if immutableKey(oldSpec) != immutableKey(newSpec) {
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("spec"))
	}
}

func (r *DeploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &resp.Diagnostics, func(m *deploymentModel) {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
	})
}

func (r *DeploymentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &resp.Diagnostics, func(m *deploymentModel) {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
	})
}

func (r *DeploymentResource) apply(ctx context.Context, plan *deploymentModel,
	diags diagAppender, setState func(*deploymentModel)) {
	d, hash, err := r.resolveSpec(ctx, plan)
	if err != nil {
		diags.AddError("Invalid spec", err.Error())
		return
	}
	eng := engine.New()
	st, err := eng.Deploy(ctx, d)
	for _, w := range eng.Warnings {
		diags.AddWarning("labdeploy", w)
	}
	if err != nil {
		diags.AddError("Deploy failed", err.Error())
		return
	}
	plan.ID = types.StringValue(deploymentID(d))
	plan.SpecHash = types.StringValue(hash)
	fillStatus(ctx, plan, st, diags)
	setState(plan)
}

func (r *DeploymentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, _, err := r.resolveSpec(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddError("Invalid spec in state", err.Error())
		return
	}
	eng := engine.New()
	st, err := eng.ReadStatus(ctx, d)
	if err != nil {
		// Unreachable target ≠ deleted resource (DESIGN §10.4): keep state.
		resp.Diagnostics.AddWarning("labdeploy read degraded", err.Error())
		return
	}
	if st == nil { // manifest absent ⇒ resource gone
		resp.State.RemoveResource(ctx)
		return
	}
	fillStatus(ctx, &state, st, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *DeploymentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, _, err := r.resolveSpec(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddError("Invalid spec in state", err.Error())
		return
	}
	eng := engine.New()
	if err := eng.Destroy(ctx, d, state.DestroyMode.ValueString()); err != nil {
		resp.Diagnostics.AddError("Destroy failed", err.Error())
		return
	}
	for _, w := range eng.Warnings {
		resp.Diagnostics.AddWarning("labdeploy", w)
	}
}

// ImportState is unsupported in v1 (DESIGN §5.2): manifests carry target truth
// but TF state needs the spec input, which import cannot reconstruct.
func (r *DeploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.AddError("Import not supported",
		"labdeploy_deployment cannot be imported (DESIGN §5.2); re-apply the spec instead.")
}

// ------------------------------ shared helpers ------------------------------

type diagAppender = interface {
	AddError(summary, detail string)
	AddWarning(summary, detail string)
}

func deploymentID(d *spec.Deployment) string {
	return strings.ToLower(d.Metadata.Name + "@" + strings.Join(d.Target.Hosts, ","))
}

func fillStatus(ctx context.Context, m *deploymentModel, st *engine.Status, diags diagAppender) {
	m.DeployedVersion = types.StringValue(st.DeployedVersion)
	m.PreviousVersion = types.StringValue(st.PreviousVersion)
	m.ReleasePath = types.StringValue(st.ReleasePath)
	m.ServiceStatus = types.StringValue(st.ServiceStatus)
	hosts, d := types.ListValueFrom(ctx, types.StringType, st.Hosts)
	if d.HasError() {
		diags.AddError("internal", fmt.Sprintf("hosts conversion: %v", d.Errors()))
		return
	}
	m.Hosts = hosts
}
