package provider

import (
	"context"
	"encoding/json"
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
	_ resource.Resource                     = (*DeploymentResource)(nil)
	_ resource.ResourceWithConfigure        = (*DeploymentResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*DeploymentResource)(nil)
	_ resource.ResourceWithImportState      = (*DeploymentResource)(nil)
	_ resource.ResourceWithConfigValidators = (*DeploymentResource)(nil)
)

func NewDeploymentResource() resource.Resource { return &DeploymentResource{} }

type DeploymentResource struct{ pd *providerData }

// ConfigValidators enforces the VAL-06 exactly-one-of(spec, spec_file) rule at
// the Terraform config layer (DESIGN §14).
func (r *DeploymentResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{exactlyOneOfSpecValidator{}}
}

type deploymentModel struct {
	ID              types.String `tfsdk:"id"`
	Spec            types.String `tfsdk:"spec"`
	SpecFile        types.String `tfsdk:"spec_file"`
	Variables       types.Map    `tfsdk:"variables"`
	VersionOverride types.String `tfsdk:"version_override"`
	DestroyMode     types.String `tfsdk:"destroy_mode"`
	SpecHash        types.String `tfsdk:"spec_hash"`
	ResolvedSpec    types.String `tfsdk:"resolved_spec"`
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
			"id":        schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"spec_hash": schema.StringAttribute{Computed: true},
			"resolved_spec": schema.StringAttribute{Computed: true, Sensitive: true,
				MarkdownDescription: "Internal: the fully-resolved deployment spec (post-substitution/version_override) as of the last apply, persisted so an Update can faithfully compare against the prior artifact and configuration even when spec_file contents have since changed on the runner. Marked sensitive because ${var:...} substitutions may embed secret values into the resolved spec."},
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
		return nil, "", err // already [ERR_SPEC_INVALID]-coded by the spec package
	}
	if r.pd != nil && r.pd.DefaultTarget != nil {
		spec.MergeTargetDefaults(&d.Target, r.pd.DefaultTarget)
	}
	if err := spec.ValidateDeployment(d); err != nil { // validate post-merge (DESIGN §6.2)
		return nil, "", err // already [ERR_SPEC_INVALID]-coded
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
	// Persist the fully-resolved spec at plan time so it is known (no perpetual
	// "known after apply" diff) and so a later Update can compare against it even
	// after spec_file contents change on disk.
	plan.ResolvedSpec = marshalResolvedSpec(newSpec)
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)

	if req.State.Raw.IsNull() { // create
		return
	}
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Compare against the PERSISTED prior spec, not a fresh re-read of spec_file
	// (which by plan time already holds the NEW contents and would make old == new,
	// hiding immutable-field changes).
	oldSpec := r.priorFromState(ctx, &state)
	if oldSpec == nil {
		// No faithful prior snapshot (state written before resolved_spec, from a
		// spec_file whose on-disk contents cannot be trusted as the prior). We cannot
		// honestly Reconfigure — that needs the ACTUAL prior configuration for
		// changed-post_install detection, rollback restoration, and prior-health
		// validation. When the resolved spec has actually changed, force a REPLACEMENT
		// so the change is applied via a full, truthful create/deploy (which also
		// persists resolved_spec, self-healing the legacy state for future updates).
		// No drift ⇒ nothing to migrate.
		if legacyReplaceRequired(oldSpec, newHash, &state) {
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root("spec_hash"))
		}
		return
	}
	if immutableKey(oldSpec) != immutableKey(newSpec) {
		resp.RequiresReplace = append(resp.RequiresReplace, configuredSpecPath(&plan))
	}
}

// legacyReplaceRequired reports whether an update must be handled as a truthful
// REPLACEMENT because no faithful prior snapshot exists to reconfigure against.
// oldSpec==nil means priorFromState could not reconstruct a trustworthy prior
// (legacy spec_file state written before resolved_spec); newHash != the persisted
// spec_hash means the resolved spec really changed. When both hold, forcing a
// replacement is safer than feeding fabricated prior state to Reconfigure. No drift
// ⇒ no change to apply, so no replacement.
func legacyReplaceRequired(oldSpec *spec.Deployment, newHash string, state *deploymentModel) bool {
	return oldSpec == nil && newHash != state.SpecHash.ValueString()
}

// configuredSpecPath returns the schema path of whichever spec attribute the
// configuration actually sets, so a forced replacement is attributed to spec_file
// when that is the source rather than always to spec.
func configuredSpecPath(m *deploymentModel) path.Path {
	if !m.SpecFile.IsNull() && m.SpecFile.ValueString() != "" {
		return path.Root("spec_file")
	}
	return path.Root("spec")
}

// marshalResolvedSpec serializes a resolved deployment to canonical JSON for
// persistence in the resolved_spec computed attribute. password_env and other
// secret references are stored by NAME, but ${var:...} substitutions may embed
// secret values into other fields, so the snapshot can contain sensitive data —
// it is persisted only in the Sensitive resolved_spec attribute. Returns null on
// the (practically impossible) marshal error.
func marshalResolvedSpec(d *spec.Deployment) types.String {
	b, err := json.Marshal(d)
	if err != nil {
		return types.StringNull()
	}
	return types.StringValue(string(b))
}

func (r *DeploymentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, nil, &resp.Diagnostics, func(m *deploymentModel) {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
	})
}

func (r *DeploymentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan deploymentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Resolve the PRIOR spec from state so a same-artifact configuration change is
	// routed through the transactional Reconfigure path instead of Deploy, whose
	// idempotency short-circuit would otherwise skip it. Decoding errors are NOT
	// swallowed — corrupt/incompatible state must surface rather than silently
	// downgrade to a blind Deploy.
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prior := r.priorFromState(ctx, &state)
	r.apply(ctx, &plan, prior, &resp.Diagnostics, func(m *deploymentModel) {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
	})
}

// priorFromState reconstructs the prior deployment from the PERSISTED resolved_spec
// (written verbatim on the last apply/plan), so it is faithful regardless of whether
// the prior spec came from inline `spec` or from a `spec_file` whose contents have
// since changed on the runner. This is what lets a same-artifact spec_file
// configuration change route correctly through Reconfigure (applying CONFIGURE and a
// changed post_install) while a real artifact upgrade still routes to Deploy, and it
// gives rollback the ACTUAL prior configuration to restore. Falls back to re-parsing
// inline spec for state written before resolved_spec existed (still faithful verbatim);
// returns nil for legacy spec_file state (whose on-disk contents cannot be trusted as
// the prior) so ModifyPlan can force a truthful replacement rather than fabricate a
// prior for Reconfigure.
func (r *DeploymentResource) priorFromState(ctx context.Context, state *deploymentModel) *spec.Deployment {
	if rs := state.ResolvedSpec.ValueString(); rs != "" {
		var d spec.Deployment
		if err := json.Unmarshal([]byte(rs), &d); err == nil {
			return &d
		}
	}
	// Back-compat: state persisted before resolved_spec existed. Inline spec is
	// still faithful verbatim.
	if !state.Spec.IsNull() && state.Spec.ValueString() != "" {
		if d, _, err := r.resolveSpec(ctx, state); err == nil {
			return d
		}
		return nil
	}
	// Legacy spec_file state (no resolved_spec): re-reading the file at Update time
	// yields the CURRENT (possibly already-changed) contents, and only deployed_version
	// is recorded — that is NOT the actual prior configuration. Fabricating a prior from
	// it (desired config + old version) would make a changed post_install look unchanged,
	// make rollback restore the DESIRED settings, and health-check against the new spec —
	// a false promise of transactional restoration. So we decline (nil); ModifyPlan
	// instead forces a truthful replacement when the spec has actually changed.
	return nil
}

// stateSpec returns the deployment that Read and Delete must operate on: the ACTUAL
// deployed identity captured in the PERSISTED resolved_spec snapshot, never a fresh
// re-read of a mutable spec_file. Re-reading spec_file in these lifecycle operations is
// unsafe — an edit to an immutable field (target.hosts, service_name, install_root)
// would make Read query, and Delete destroy, the DESIRED target instead of the DEPLOYED
// one, removing state while orphaning the live service (Read) or destroying the wrong
// identity and leaving the original service installed (Delete).
//
// A non-empty resolved_spec that fails to decode is treated as STATE CORRUPTION and
// surfaced as an error — we must NOT silently fall back to the mutable spec_file, which
// could target the wrong identity. Legacy state written before resolved_spec existed has
// no snapshot: inline `spec` is faithful verbatim (verified==true), but a spec_file
// reread is NOT a trustworthy deployed identity (verified==false) so callers must not
// treat a missing manifest as resource absence.
func (r *DeploymentResource) stateSpec(ctx context.Context, state *deploymentModel) (d *spec.Deployment, hash string, verified bool, err error) {
	if rs := state.ResolvedSpec.ValueString(); rs != "" {
		var sd spec.Deployment
		if uerr := json.Unmarshal([]byte(rs), &sd); uerr != nil {
			return nil, "", false, fmt.Errorf("[ERR_STATE_CORRUPT] resolved_spec in state is not decodable: %w; "+
				"refusing to fall back to the mutable spec_file (which could target the wrong identity). "+
				"Taint and re-apply (terraform apply -replace) to repair state", uerr)
		}
		return &sd, state.SpecHash.ValueString(), true, nil
	}
	// Legacy state with no snapshot. Inline `spec` is faithful verbatim; a spec_file
	// reread is unverifiable as the deployed identity.
	verified = !state.Spec.IsNull() && state.Spec.ValueString() != ""
	sd, h, rerr := r.resolveSpec(ctx, state)
	return sd, h, verified, rerr
}

func (r *DeploymentResource) apply(ctx context.Context, plan *deploymentModel,
	prior *spec.Deployment, diags diagAppender, setState func(*deploymentModel)) {
	d, hash, err := r.resolveSpec(ctx, plan)
	if err != nil {
		diags.AddError("Invalid spec", err.Error())
		return
	}
	eng := engine.New()
	st, err := eng.Update(ctx, d, prior)
	for _, w := range eng.Warnings {
		diags.AddWarning("labdeploy", w)
	}
	if err != nil {
		diags.AddError("Deploy failed", err.Error())
		return
	}
	plan.ID = types.StringValue(deploymentID(d))
	plan.SpecHash = types.StringValue(hash)
	plan.ResolvedSpec = marshalResolvedSpec(d)
	fillStatus(ctx, plan, st, diags)
	setState(plan)
}

func (r *DeploymentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, _, verified, err := r.stateSpec(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddError("Invalid spec in state", err.Error())
		return
	}
	eng := engine.New()
	st, err := eng.ReadStatus(ctx, d)
	for _, w := range eng.Warnings {
		resp.Diagnostics.AddWarning("labdeploy", w)
	}
	if err != nil {
		// DESIGN §10.4: refresh MUST fail loudly on transport errors (no silent
		// drop). We surface an ERROR diagnostic but do NOT RemoveResource, so the
		// prior state is retained — an unreachable target is not a deleted
		// resource (evaluator item 4).
		resp.Diagnostics.AddError("labdeploy read failed", err.Error())
		return
	}
	if st == nil { // manifest absent
		if !verified {
			// Unverifiable legacy spec_file target: we queried a spec_file re-read, not a
			// persisted snapshot, so a missing manifest may only mean the file was edited to
			// point at a DIFFERENT target — treating it as absence would drop state and
			// orphan the still-deployed service. Retain state and demand an explicit
			// migration instead of silently removing the resource.
			resp.Diagnostics.AddError("labdeploy state migration required",
				"this resource predates resolved_spec and is configured via spec_file, so its "+
					"deployed identity cannot be verified from state. A missing manifest here may be "+
					"the result of a spec_file edit pointing at a different target rather than a real "+
					"deletion. Re-apply (terraform apply, or apply -replace to redeploy) to persist a "+
					"resolved_spec snapshot before relying on refresh or destroy.")
			return
		}
		resp.State.RemoveResource(ctx) // verified snapshot ⇒ genuine absence
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
	d, _, _, err := r.stateSpec(ctx, &state)
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
