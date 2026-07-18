package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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

// Timeout defaults for the deployment lifecycle (DESIGN §5.2): create/update run
// long-lived deploys (30m); destroy is bounded tighter (15m).
const (
	defaultCreateTimeout = 30 * time.Minute
	defaultUpdateTimeout = 30 * time.Minute
	defaultDeleteTimeout = 15 * time.Minute
)

var (
	_ resource.Resource                     = (*DeploymentResource)(nil)
	_ resource.ResourceWithConfigure        = (*DeploymentResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*DeploymentResource)(nil)
	_ resource.ResourceWithImportState      = (*DeploymentResource)(nil)
	_ resource.ResourceWithConfigValidators = (*DeploymentResource)(nil)
	_ resource.ResourceWithUpgradeState     = (*DeploymentResource)(nil)
)

func NewDeploymentResource() resource.Resource { return &DeploymentResource{} }

type DeploymentResource struct {
	pd *providerData
	// newEngine is an OPTIONAL test seam (DESIGN §17): when nil the resource uses
	// the real engine.New(); unit tests inject a fake so a lifecycle path (e.g. the
	// state-rm + apply recovery Create) can be exercised end-to-end without a live
	// transport. Production always leaves it nil.
	newEngine func() deployEngine
}

// deployEngine is the narrow engine surface the resource depends on. It exists so
// tests can inject a fake at the engine boundary (DESIGN §17) rather than
// reimplementing the transport-script protocol. realEngine adapts *engine.Engine.
type deployEngine interface {
	Update(ctx context.Context, s, prior *spec.Deployment) (*engine.Status, error)
	ReadStatus(ctx context.Context, s *spec.Deployment) (*engine.Status, error)
	Destroy(ctx context.Context, s *spec.Deployment, mode string) error
	Warns() []string
}

type realEngine struct{ *engine.Engine }

func (r realEngine) Warns() []string { return r.Engine.Warnings }

func (r *DeploymentResource) engine() deployEngine {
	if r.newEngine != nil {
		return r.newEngine()
	}
	return realEngine{engine.New()}
}

// ConfigValidators enforces the VAL-06 exactly-one-of(spec, spec_file) rule at
// the Terraform config layer (DESIGN §14).
func (r *DeploymentResource) ConfigValidators(_ context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{exactlyOneOfSpecValidator{}}
}

type deploymentModel struct {
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

func (r *DeploymentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_deployment"
}

func (r *DeploymentResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = deploymentSchema(ctx, 1)
}

// deploymentSchema builds the resource schema at the given version. The attribute
// shape is identical across versions — only Version differs — so the same builder
// serves both the current Schema (v1) and the UpgradeState PriorSchema (v0). The v0→v1
// bump exists solely to trigger the state upgrader that backfills resolved_spec for
// legacy state persisted before the snapshot was populated.
func deploymentSchema(ctx context.Context, version int64) schema.Schema {
	return schema.Schema{
		Version:             version,
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
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				MarkdownDescription: "Deployment name, from spec metadata.name (DESIGN §5.2). Immutable, so it uses the prior state value across in-place updates."},
			"spec_hash": schema.StringAttribute{Computed: true},
			"resolved_spec": schema.StringAttribute{Computed: true, Sensitive: true,
				MarkdownDescription: "Internal: the fully-resolved deployment spec (post-substitution/version_override) as of the last apply, persisted so an Update can faithfully compare against the prior artifact and configuration even when spec_file contents have since changed on the runner. Marked sensitive because ${var:...} substitutions may embed secret values into the resolved spec."},
			"deployed_version": schema.StringAttribute{Computed: true},
			"previous_version": schema.StringAttribute{Computed: true},
			"hosts":            schema.ListAttribute{Computed: true, ElementType: types.StringType},
			"release_path":     schema.StringAttribute{Computed: true},
			"service_status":   schema.StringAttribute{Computed: true},
		},
		Blocks: map[string]schema.Block{
			// DESIGN §5.2: create/update default 30m, delete 15m. The block only
			// declares the attributes; the defaults are applied at read time via
			// Timeouts.Create/Update/Delete(ctx, default) in the CRUD methods.
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create: true,
				Update: true,
				Delete: true,
			}),
		},
	}
}

// UpgradeState migrates state persisted at an earlier schema version. The v0→v1
// upgrader BACKFILLS resolved_spec for legacy state written before the snapshot
// attribute was populated, converting an unverifiable spec_file deployment into a
// VERIFIED snapshot BEFORE any plan/refresh acts on it. This is the migration path
// that breaks the legacy deadlock (ModifyPlan forcing replacement ↔ Delete refusing
// snapshot-less state) WITHOUT any unsafe deletion.
//
// The backfill is TRUSTWORTHY ONLY when the current spec_file still resolves to the
// SAME spec_hash that was persisted at the last apply — i.e. the file has NOT been
// edited since the deployment. If the operator upgraded the provider AND edited the
// spec in the same step, the file now describes the DESIRED (not deployed) config, so
// snapshotting it would fabricate a false prior — suppressing immutable replacement,
// reconfiguration, changed post_install, and truthful rollback. In that case (or when
// no baseline spec_hash exists, or the spec cannot be resolved) we DECLINE to backfill,
// leave resolved_spec empty, warn, and let the downstream Read/Delete guards +
// legacyReplaceRequired safeguard keep the state fail-safe.
func (r *DeploymentResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	priorSchema := deploymentSchema(ctx, 0)
	return map[int64]resource.StateUpgrader{
		0: {
			PriorSchema: &priorSchema,
			StateUpgrader: func(ctx context.Context, req resource.UpgradeStateRequest, resp *resource.UpgradeStateResponse) {
				var state deploymentModel
				resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
				if resp.Diagnostics.HasError() {
					return
				}
				if state.ResolvedSpec.IsNull() || state.ResolvedSpec.ValueString() == "" {
					r.backfillResolvedSpec(ctx, &state, &resp.Diagnostics)
				}
				resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			},
		},
	}
}

// backfillResolvedSpec reconstructs the resolved_spec snapshot for legacy state, but
// ONLY when the current spec resolves to the persisted spec_hash (proving the source
// is unchanged since deployment, so the snapshot faithfully captures the DEPLOYED
// identity). A missing baseline hash, a hash mismatch (source edited), a resolve
// error, or a provider default_target that contributes merged fields the pre-merge
// hash never covered all cause it to DECLINE and warn — never fabricate a prior.
func (r *DeploymentResource) backfillResolvedSpec(ctx context.Context, state *deploymentModel, diags *diag.Diagnostics) {
	d, hash, defaultsContributed, err := r.resolveSpecDetail(ctx, state)
	if err != nil {
		diags.AddWarning("[WARN] labdeploy state not fully migrated",
			"could not reconstruct a resolved_spec snapshot for this legacy resource "+
				"(the spec could not be resolved: "+err.Error()+"). Lifecycle operations "+
				"remain fail-safe; run terraform apply once the spec is resolvable to persist "+
				"the snapshot.")
		return
	}
	persisted := state.SpecHash.ValueString()
	if persisted == "" || persisted != hash {
		// The source no longer matches what was deployed (edited alongside the provider
		// upgrade) or there is no baseline to compare. Snapshotting now would record the
		// DESIRED config as the deployed prior — a false, high-impact fabrication. Decline.
		diags.AddWarning("[WARN] labdeploy state not fully migrated",
			"the current spec no longer matches the spec_hash recorded at the last apply "+
				"(or no baseline spec_hash exists), so a resolved_spec snapshot cannot be "+
				"trusted to reflect the DEPLOYED configuration. Leaving the snapshot unset so "+
				"immutable-field replacement, reconfiguration, and rollback stay truthful; "+
				"re-apply against the currently deployed spec to persist a faithful snapshot.")
		return
	}
	if defaultsContributed {
		// spec_hash matches because it is computed over the RAW spec, BEFORE provider
		// default_target is merged. A default_target that now contributes target fields
		// (transport/os/port/credentials/winrm) means the resolved_spec identity depends
		// on provider config the persisted hash never covered — the merged fields may
		// never have been deployed. Decline so a provider-default change cannot fabricate
		// the deployed identity during state migration.
		diags.AddWarning("[WARN] labdeploy state not fully migrated",
			"the current spec relies on provider default_target values to fill one or more "+
				"target fields, but spec_hash is computed before those defaults are merged, "+
				"so it cannot prove the merged configuration was ever deployed. Leaving the "+
				"resolved_spec snapshot unset so immutable-field replacement, reconfiguration, "+
				"and rollback stay truthful; re-apply against the currently deployed spec to "+
				"persist a faithful snapshot.")
		return
	}
	state.ResolvedSpec = marshalResolvedSpec(d)
}

func (r *DeploymentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.pd = req.ProviderData.(*providerData)
}

// resolveSpec loads+parses the deployment for a model (DESIGN §6.1).
func (r *DeploymentResource) resolveSpec(ctx context.Context, m *deploymentModel) (*spec.Deployment, string, error) {
	d, hash, _, err := r.resolveSpecDetail(ctx, m)
	return d, hash, err
}

// resolveSpecDetail is resolveSpec plus a defaultsContributed flag reporting whether
// the provider's default_target actually filled any target field during the merge.
// spec_hash is computed over the RAW (pre-merge) spec, so it CANNOT detect a change to
// provider default_target: an unchanged spec_file hashes identically even when a new
// default now contributes never-deployed fields to the resolved spec. The flag lets the
// state-migration backfill refuse to snapshot a resolved_spec whose identity depends on
// provider config that the persisted hash never covered.
func (r *DeploymentResource) resolveSpecDetail(ctx context.Context, m *deploymentModel) (*spec.Deployment, string, bool, error) {
	hasSpec := !m.Spec.IsNull() && m.Spec.ValueString() != ""
	hasFile := !m.SpecFile.IsNull() && m.SpecFile.ValueString() != ""
	if hasSpec == hasFile {
		return nil, "", false, fmt.Errorf("[ERR_SPEC_INVALID] exactly one of spec/spec_file must be set")
	}
	raw := m.Spec.ValueString()
	if hasFile {
		b, err := os.ReadFile(m.SpecFile.ValueString())
		if err != nil {
			return nil, "", false, fmt.Errorf("[ERR_SPEC_INVALID] spec_file: %w", err)
		}
		raw = string(b)
	}
	vars := map[string]string{}
	if !m.Variables.IsNull() {
		if diag := m.Variables.ElementsAs(ctx, &vars, false); diag.HasError() {
			return nil, "", false, fmt.Errorf("[ERR_SPEC_INVALID] variables: %v", diag.Errors())
		}
	}
	d, hash, err := spec.ParseDeploymentLenient(raw, vars, m.VersionOverride.ValueString())
	if err != nil {
		return nil, "", false, err // already [ERR_SPEC_INVALID]-coded by the spec package
	}
	defaultsContributed := false
	if r.pd != nil && r.pd.DefaultTarget != nil {
		before := d.Target
		spec.MergeTargetDefaults(&d.Target, r.pd.DefaultTarget)
		defaultsContributed = !reflect.DeepEqual(before, d.Target)
	}
	if err := spec.ValidateDeployment(d); err != nil { // validate post-merge (DESIGN §6.2)
		return nil, "", false, err // already [ERR_SPEC_INVALID]-coded
	}
	return d, hash, defaultsContributed, nil
}

// immutableKey captures the spec paths that force replacement (DESIGN §5.2):
// pattern.type, service_name, role_name, install_root, metadata.name,
// target.hosts, target.os. target.hosts is compared as a SET (lower-cased and
// sorted) per architecture.md line 350: reordering the same hosts is NOT a
// replacement, only adding/removing a host is.
func immutableKey(d *spec.Deployment) string {
	hosts := make([]string, len(d.Target.Hosts))
	for i, h := range d.Target.Hosts {
		hosts[i] = strings.ToLower(h)
	}
	sort.Strings(hosts)
	return strings.Join([]string{
		string(d.Pattern.Type), d.Pattern.ServiceName, d.Pattern.RoleName,
		d.Pattern.EffectiveInstallRoot(d.Target.OS), d.Metadata.Name,
		strings.Join(hosts, ","), string(d.Target.OS),
	}, "|")
}

// Recovery procedures surfaced in migration/corruption diagnostics. These are
// EXECUTABLE and reach VERIFIED state without the dead-ends the guards block: neither
// `terraform apply -replace` (whose Delete half is refused for unverifiable/corrupt
// state) nor "pin the defaulted fields then apply" (which changes the pre-merge hash and
// so forces the same blocked replacement). The escape hatch is `terraform state rm`
// followed by `terraform apply`: state rm is a state-only operation (it never touches the
// live service or invokes Delete), and the subsequent apply recreates the resource via
// Create, which deploys IDEMPOTENTLY (adopting the already-running service when the spec
// matches it) and persists a verified resolved_spec snapshot. ModifyPlan's create branch
// returns before the update guards, so this path is never blocked.
const (
	// recoveryUnverifiableDefaults applies when the deployed identity cannot be verified
	// from state AND provider default_target contributes immutable fields the pre-merge
	// spec_hash does not cover.
	recoveryUnverifiableDefaults = "the deployed identity cannot be verified from state and " +
		"the provider default_target contributes one or more target fields (transport/os/port/" +
		"hosts/credentials/winrm) that spec_hash — computed before defaults are merged — does not " +
		"cover, so planning an update could deploy to a newly-defaulted target and orphan the " +
		"original deployment. Recovery: first make this spec (or default_target) describe the " +
		"ACTUALLY deployed identity, then run `terraform state rm <address>` followed by " +
		"`terraform apply`. state rm is state-only (it never destroys the live service or invokes " +
		"the guarded Delete); the apply then recreates via Create, which deploys idempotently " +
		"(adopting the existing service when the spec matches) and persists a verified resolved_spec " +
		"snapshot. Do NOT instead pin the defaults and re-apply in place: that changes the pre-merge " +
		"hash and forces a replacement whose Delete half is refused for this unverifiable state."

	// recoveryUnverifiableLegacy applies to legacy spec_file state with no snapshot and no
	// contributing defaults.
	recoveryUnverifiableLegacy = "this resource predates resolved_spec and is configured via " +
		"spec_file, so its deployed identity cannot be verified from state. Recovery: run " +
		"`terraform apply` WITHOUT editing the spec_file — with no immutable change pending, planning " +
		"does not force a replacement, so the apply flows through Update and persists a resolved_spec " +
		"snapshot from the currently-deployed spec. If planning instead forces a replacement (e.g. the " +
		"spec must change), run `terraform state rm <address>` then `terraform apply` to re-adopt via a " +
		"fresh Create. Both reach verified state without the guarded Delete destroying an unverifiable " +
		"identity. (Do NOT use `terraform apply -replace` — its destroy half is blocked for exactly " +
		"this reason.)"

	// recoveryCorruptSnapshot applies when a non-empty resolved_spec cannot be decoded. It
	// cannot be repaired in place; the state entry must be removed so the resource can be
	// re-adopted against the live service.
	recoveryCorruptSnapshot = "the persisted resolved_spec snapshot is corrupt and cannot be " +
		"decoded, so the deployed identity is unknown; the Read/Delete guards refuse to act on it to " +
		"avoid querying or destroying the wrong identity. Recovery: run `terraform state rm <address>` " +
		"then `terraform apply` to re-adopt via a fresh Create (which deploys idempotently and persists " +
		"a verified snapshot), or restore a known-good state backup — the corrupt snapshot cannot be " +
		"repaired in place, and state rm never invokes the guarded Delete."
)

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
	newSpec, newHash, defaultsContributed, err := r.resolveSpecDetail(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_SPEC_INVALID", "invalid spec", err), err.Error())
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
	oldSpec, perr := r.priorFromState(ctx, &state)
	if perr != nil {
		// Corrupt persisted snapshot: fail loudly (consistent with stateSpec) rather than
		// planning an update against an untrustworthy prior that could target the wrong host.
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_SPEC_INVALID", "invalid spec in state", perr), perr.Error())
		return
	}
	if oldSpec == nil {
		// No faithful prior snapshot. When provider default_target contributes fields to the
		// resolved spec, spec_hash (computed pre-merge) cannot detect a default change, and
		// because that hash is unchanged we CANNOT hang a replacement off spec_hash. Adopting
		// the merged spec would silently deploy to the newly-defaulted target and orphan the
		// original deployment. BLOCK the plan with an executable recovery instead of laundering
		// the unverifiable merged identity into state.
		if defaultsContributed {
			resp.Diagnostics.AddError("[ERR_SPEC_INVALID] labdeploy state migration required", recoveryUnverifiableDefaults)
			return
		}
		// Otherwise the v0→v1 state upgrader normally backfilled resolved_spec already; this
		// branch is only reached when even the upgrader could not reconstruct a snapshot. We
		// cannot honestly Reconfigure without the ACTUAL prior configuration, so when the
		// resolved spec really changed (hash drift), force a REPLACEMENT to apply the change via
		// a full, truthful create/deploy. No drift ⇒ nothing to do.
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
	createTimeout, diags := plan.Timeouts.Create(ctx, defaultCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, createTimeout)
	defer cancel()
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
	updateTimeout, diags := plan.Timeouts.Update(ctx, defaultUpdateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
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
	prior, perr := r.priorFromState(ctx, &state)
	if perr != nil {
		// Corrupt persisted snapshot: surface it (consistent with stateSpec / ModifyPlan)
		// rather than feeding a fabricated or nil prior into a blind Deploy.
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_SPEC_INVALID", "invalid spec in state", perr), perr.Error())
		return
	}
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
// gives rollback the ACTUAL prior configuration to restore. A non-empty resolved_spec
// that fails to decode is STATE CORRUPTION: it returns an error (consistent with
// stateSpec) so Update/ModifyPlan fail loudly instead of fabricating a prior. Falls back
// to re-parsing inline spec for state written before resolved_spec existed (still faithful
// verbatim); returns (nil, nil) for legacy spec_file state (whose on-disk contents cannot
// be trusted as the prior) so ModifyPlan can force a truthful replacement.
func (r *DeploymentResource) priorFromState(ctx context.Context, state *deploymentModel) (*spec.Deployment, error) {
	if rs := state.ResolvedSpec.ValueString(); rs != "" {
		var d spec.Deployment
		if err := json.Unmarshal([]byte(rs), &d); err != nil {
			// Corrupt snapshot: do NOT fall back to the mutable spec_file (which could target
			// the wrong identity) and do NOT return a nil "no prior" (which Update would treat
			// as a blind first deploy). Surface it as state corruption.
			return nil, fmt.Errorf("[ERR_SPEC_INVALID] resolved_spec in state is not decodable: %w; %s", err, recoveryCorruptSnapshot)
		}
		return &d, nil
	}
	// Back-compat: state persisted before resolved_spec existed. Inline spec is
	// still faithful verbatim.
	if !state.Spec.IsNull() && state.Spec.ValueString() != "" {
		if d, _, err := r.resolveSpec(ctx, state); err == nil {
			return d, nil
		}
		return nil, nil
	}
	// Legacy spec_file state (no resolved_spec): re-reading the file at Update time
	// yields the CURRENT (possibly already-changed) contents, and only deployed_version
	// is recorded — that is NOT the actual prior configuration. Fabricating a prior from
	// it (desired config + old version) would make a changed post_install look unchanged,
	// make rollback restore the DESIRED settings, and health-check against the new spec —
	// a false promise of transactional restoration. So we decline (nil); ModifyPlan
	// instead forces a truthful replacement when the spec has actually changed.
	return nil, nil
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
			return nil, "", false, fmt.Errorf("[ERR_SPEC_INVALID] resolved_spec in state is not decodable: %w; %s", uerr, recoveryCorruptSnapshot)
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
		diags.AddError(diagSummary(ctx, "ERR_SPEC_INVALID", "invalid spec", err), err.Error())
		return
	}
	eng := r.engine()
	st, err := eng.Update(ctx, d, prior)
	for _, w := range eng.Warns() {
		diags.AddWarning(warnSummary(w), w)
	}
	if err != nil {
		diags.AddError(diagSummary(ctx, "ERR_CONNECT", "deploy failed", err), err.Error())
		return
	}
	plan.ID = types.StringValue(deploymentID(d))
	plan.Name = types.StringValue(d.Metadata.Name)
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
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_SPEC_INVALID", "invalid spec in state", err), err.Error())
		return
	}
	eng := r.engine()
	st, err := eng.ReadStatus(ctx, d)
	for _, w := range eng.Warns() {
		resp.Diagnostics.AddWarning(warnSummary(w), w)
	}
	if err != nil {
		// DESIGN §10.4: refresh MUST fail loudly on transport errors (no silent
		// drop). We surface an ERROR diagnostic but do NOT RemoveResource, so the
		// prior state is retained — an unreachable target is not a deleted
		// resource (evaluator item 4).
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_CONNECT", "read failed", err), err.Error())
		return
	}
	if st == nil { // manifest absent
		if !verified {
			// Unverifiable legacy spec_file target: we queried a spec_file re-read, not a
			// persisted snapshot, so a missing manifest may only mean the file was edited to
			// point at a DIFFERENT target — treating it as absence would drop state and
			// orphan the still-deployed service. Retain state and demand an explicit
			// migration instead of silently removing the resource.
			resp.Diagnostics.AddError("[ERR_SPEC_INVALID] labdeploy state migration required",
				"a missing manifest here may be the result of a spec_file edit pointing at a "+
					"different target rather than a real deletion, so removing state would orphan the "+
					"still-deployed service. "+recoveryUnverifiableLegacy)
			return
		}
		resp.State.RemoveResource(ctx) // verified snapshot ⇒ genuine absence
		return
	}
	state.Name = types.StringValue(d.Metadata.Name)
	fillStatus(ctx, &state, st, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *DeploymentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state deploymentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, diags := state.Timeouts.Delete(ctx, defaultDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	d, _, verified, err := r.stateSpec(ctx, &state)
	if err != nil {
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_SPEC_INVALID", "invalid spec in state", err), err.Error())
		return
	}
	if !verified {
		// Legacy spec_file state with no persisted resolved_spec snapshot: the identity
		// we would destroy comes from a fresh spec_file reread, NOT the deployed snapshot.
		// An immutable-field edit on disk (host, service_name, install_root) would make
		// Destroy target the WRONG service and orphan the one actually deployed. Refuse to
		// destroy an unverifiable identity; demand an explicit migration first.
		resp.Diagnostics.AddError("[ERR_SPEC_INVALID] labdeploy state migration required",
			"the identity to destroy cannot be verified from state — it would be re-derived from "+
				"the current spec_file, which may have been edited to point at a different target, so "+
				"destroying now could orphan the deployed service. "+recoveryUnverifiableLegacy)
		return
	}
	eng := r.engine()
	if err := eng.Destroy(ctx, d, state.DestroyMode.ValueString()); err != nil {
		resp.Diagnostics.AddError(diagSummary(ctx, "ERR_CONNECT", "destroy failed", err), err.Error())
		return
	}
	for _, w := range eng.Warns() {
		resp.Diagnostics.AddWarning(warnSummary(w), w)
	}
}

// ImportState is unsupported in v1 (DESIGN §5.2): manifests carry target truth
// but TF state needs the spec input, which import cannot reconstruct. The message
// is the pinned string from DESIGN §5.2.
func (r *DeploymentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.AddError("[ERR_UNSUPPORTED] import is not supported; adopt via apply",
		"import is not supported; adopt via apply")
}

// ------------------------------ shared helpers ------------------------------

// diagSummary formats a diagnostic Summary per DESIGN §12: exactly
// "[<CODE>] <short>", using only codes from the closed §12 taxonomy table.
//
// Timeout taxonomy (DESIGN §8.1 vs §12): ERR_TIMEOUT is reserved for the OUTER
// Terraform resource-operation deadline (the create/update/delete timeouts block,
// or runner.timeout_seconds) expiring; a dial/transport/WinRM CHILD deadline that
// fires while the outer operation context is still live is a connectivity failure
// and MUST stay ERR_CONNECT. We therefore classify on the OPERATION CONTEXT ITSELF
// (ctx.Err() == context.DeadlineExceeded at the CRUD boundary) rather than on
// errors.Is(err, context.DeadlineExceeded) — the latter would wrongly reclassify a
// nested transport deadline (which the engine surfaces as ERR_CONNECT) as a TF
// timeout even when the resource context has not expired.
//
// When the outer context is live, an *engine.CodedError surfaces its taxonomy code
// (e.g. ERR_CONNECT); otherwise the operation-appropriate fallback so the contract
// also holds for pre-flight (spec/state) errors that never reached the engine.
func diagSummary(ctx context.Context, fallback, short string, err error) string {
	code := fallback
	switch {
	case ctx != nil && ctx.Err() == context.DeadlineExceeded:
		code = "ERR_TIMEOUT"
	default:
		var ce *engine.CodedError
		if errors.As(err, &ce) && ce.Code != "" {
			code = ce.Code
		}
	}
	return fmt.Sprintf("[%s] %s", code, short)
}

// warnSummary formats a warning Summary per DESIGN §12. Engine warnings are
// free-form and carry no taxonomy code, so they default to the stable WARN code;
// a warning that already embeds a bracketed code (e.g. "[ERR_ROLLBACK_FAILED]
// ...") surfaces that code.
func warnSummary(w string) string {
	if strings.HasPrefix(w, "[") {
		if i := strings.Index(w, "]"); i > 1 {
			return fmt.Sprintf("[%s] labdeploy warning", w[1:i])
		}
	}
	return "[WARN] labdeploy warning"
}

type diagAppender = interface {
	AddError(summary, detail string)
	AddWarning(summary, detail string)
}

// deploymentID computes the stable, host-order-independent resource id
// (DESIGN §5.2 / architecture.md line 78): sha1(sorted(hosts)+"/"+name)[0:12] +
// ":" + name. Hosts are lower-cased and sorted before hashing so the id is
// byte-identical whenever the same host set is supplied in a different order OR a
// different case. Case-folding is REQUIRED to stay consistent with immutableKey
// (which lower-cases hosts) and the case-insensitive host semantics of DESIGN §9.5:
// a case-only host edit ("LAB-01" -> "lab-01") leaves immutableKey unchanged (so it
// is planned as an in-place Update, not a replacement) yet still changes the
// canonical JSON, so spec_hash changes and Update runs. If this function preserved
// case the Update would recompute a new id while the id attribute's
// UseStateForUnknown plan modifier carried the prior id forward, and Terraform would
// reject the apply with "Provider produced inconsistent result after apply: .id".
func deploymentID(d *spec.Deployment) string {
	hosts := make([]string, len(d.Target.Hosts))
	for i, h := range d.Target.Hosts {
		hosts[i] = strings.ToLower(h)
	}
	sort.Strings(hosts)
	sum := sha1.Sum([]byte(strings.Join(hosts, ",") + "/" + d.Metadata.Name))
	return hex.EncodeToString(sum[:])[:12] + ":" + d.Metadata.Name
}

func fillStatus(ctx context.Context, m *deploymentModel, st *engine.Status, diags diagAppender) {
	m.DeployedVersion = types.StringValue(st.DeployedVersion)
	m.PreviousVersion = types.StringValue(st.PreviousVersion)
	m.ReleasePath = types.StringValue(st.ReleasePath)
	m.ServiceStatus = types.StringValue(st.ServiceStatus)
	hosts, d := types.ListValueFrom(ctx, types.StringType, st.Hosts)
	if d.HasError() {
		diags.AddError("[ERR_SPEC_INVALID] internal error", fmt.Sprintf("hosts conversion: %v", d.Errors()))
		return
	}
	m.Hosts = hosts
}
