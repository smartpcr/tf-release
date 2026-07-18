package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
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
	_ resource.Resource                = (*WindowsServiceResource)(nil)
	_ resource.ResourceWithConfigure   = (*WindowsServiceResource)(nil)
	_ resource.ResourceWithImportState = (*WindowsServiceResource)(nil)
	_ deploymentPattern                = (*windowsServiceModel)(nil)
)

// NewWindowsServiceResource is the factory registered with the provider.
func NewWindowsServiceResource() resource.Resource { return &WindowsServiceResource{} }

// WindowsServiceResource is the strongly-typed, single-target surface for the
// DESIGN §9.2 windows_service pattern. Unlike labdeploy_deployment (opaque
// YAML/JSON `spec`), every field of the pattern is a first-class Terraform
// attribute; the resource projects them onto a spec.Deployment via
// buildDeployment and drives the single-target state machine through
// singleTargetDeployment (DESIGN §10.2).
type WindowsServiceResource struct{ pd *providerData }

// windowsServiceModel is the tfsdk model. It implements deploymentPattern.
type windowsServiceModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`

	// connection (single host)
	Host                    types.String `tfsdk:"host"`
	Transport               types.String `tfsdk:"transport"`
	OS                      types.String `tfsdk:"os"`
	Port                    types.Int64  `tfsdk:"port"`
	Username                types.String `tfsdk:"username"`
	PasswordEnv             types.String `tfsdk:"password_env"`
	WinRMUseHTTPS           types.Bool   `tfsdk:"winrm_use_https"`
	WinRMInsecureSkipVerify types.Bool   `tfsdk:"winrm_insecure_skip_verify"`

	// windows_service pattern
	ServiceName        types.String   `tfsdk:"service_name"`
	DisplayName        types.String   `tfsdk:"display_name"`
	Description        types.String   `tfsdk:"description"`
	Exe                types.String   `tfsdk:"exe"`
	Args               types.List     `tfsdk:"args"`
	StartType          types.String   `tfsdk:"start_type"`
	Wrapper            types.String   `tfsdk:"wrapper"`
	WinswExe           types.String   `tfsdk:"winsw_exe"`
	InstallRoot        types.String   `tfsdk:"install_root"`
	StopTimeoutSeconds types.Int64    `tfsdk:"stop_timeout_seconds"`
	PostInstall        types.String   `tfsdk:"post_install"`
	Account            *accountModel  `tfsdk:"account"`
	Recovery           *recoveryModel `tfsdk:"recovery"`

	Environment     types.Map      `tfsdk:"environment"`
	Artifact        *artifactModel `tfsdk:"artifact"`
	VersionOverride types.String   `tfsdk:"version_override"`
	DestroyMode     types.String   `tfsdk:"destroy_mode"`

	// computed
	SpecHash        types.String `tfsdk:"spec_hash"`
	DeployedVersion types.String `tfsdk:"deployed_version"`
	PreviousVersion types.String `tfsdk:"previous_version"`
	ReleasePath     types.String `tfsdk:"release_path"`
	ServiceStatus   types.String `tfsdk:"service_status"`
}

func (r *WindowsServiceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_windows_service"
}

// requiresReplace on the immutable identity/layout attributes (DESIGN §5.2):
// changing them cannot be an in-place upgrade of the same service on the same
// box, so Terraform must destroy+create.
func strReplace() []planmodifier.String {
	return []planmodifier.String{stringplanmodifier.RequiresReplace()}
}

func (r *WindowsServiceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Deploys and manages one Windows service on a single lab host, using the " +
			"`windows_service` deployment pattern (DESIGN §9.2). A strongly-typed alternative to " +
			"`labdeploy_deployment` for the single-target Windows service case.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "Logical deployment name (`metadata.name`). Immutable — changing it replaces the resource.",
				PlanModifiers:       strReplace()},

			// connection
			"host": schema.StringAttribute{Required: true,
				MarkdownDescription: "The single target host. Immutable — changing it replaces the resource.",
				PlanModifiers:       strReplace()},
			"transport": schema.StringAttribute{Optional: true, Computed: true,
				Default:             stringdefault.StaticString("winrm"),
				MarkdownDescription: "`winrm` (default) | `ssh` | `local`."},
			"os": schema.StringAttribute{Computed: true,
				MarkdownDescription: "Always `windows` for this resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"port":         schema.Int64Attribute{Optional: true, MarkdownDescription: "0 ⇒ transport default (winrm-https 5986, winrm-http 5985, ssh 22)."},
			"username":     schema.StringAttribute{Optional: true},
			"password_env": schema.StringAttribute{Optional: true, MarkdownDescription: "ENV VAR NAME on the runner holding the connection password (never the secret itself)."},
			"winrm_use_https": schema.BoolAttribute{Optional: true,
				MarkdownDescription: "Use WinRM over HTTPS (default true when transport is winrm)."},
			"winrm_insecure_skip_verify": schema.BoolAttribute{Optional: true,
				MarkdownDescription: "Skip TLS verification for lab certs."},

			// pattern
			"service_name": schema.StringAttribute{Required: true,
				MarkdownDescription: "Windows service name (`sc.exe` name). Immutable — changing it replaces the resource.",
				PlanModifiers:       strReplace()},
			"display_name": schema.StringAttribute{Optional: true, MarkdownDescription: "Service display name (defaults to `service_name`)."},
			"description":  schema.StringAttribute{Optional: true},
			"exe":          schema.StringAttribute{Required: true, MarkdownDescription: "Service executable, relative to the current release (or absolute)."},
			"args":         schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Executable arguments."},
			"start_type": schema.StringAttribute{Optional: true, Computed: true,
				Default:             stringdefault.StaticString("auto"),
				MarkdownDescription: "`auto` (default) | `manual` | `delayed`."},
			"wrapper": schema.StringAttribute{Optional: true, Computed: true,
				Default:             stringdefault.StaticString("none"),
				MarkdownDescription: "`none` (native `sc.exe`, default) | `winsw` (WinSW wrapper)."},
			"winsw_exe": schema.StringAttribute{Optional: true, MarkdownDescription: "WinSW wrapper exe (relative to the release), required when `wrapper = winsw`."},
			"install_root": schema.StringAttribute{Optional: true, Computed: true,
				Default:             stringdefault.StaticString(`C:\deploy`),
				MarkdownDescription: "Root install dir on the target. Immutable — changing it replaces the resource.",
				PlanModifiers:       strReplace()},
			"stop_timeout_seconds": schema.Int64Attribute{Optional: true, MarkdownDescription: "Graceful stop budget before force-kill (default 30)."},
			"post_install":         schema.StringAttribute{Optional: true, MarkdownDescription: "PowerShell run in the release dir after extract, before configure."},
			"account": schema.SingleNestedAttribute{Optional: true,
				MarkdownDescription: "Non-builtin service account (defaults to LocalSystem).",
				Attributes: map[string]schema.Attribute{
					"username":     schema.StringAttribute{Optional: true},
					"password_env": schema.StringAttribute{Optional: true, MarkdownDescription: "ENV VAR NAME holding the account password."},
				}},
			"recovery": schema.SingleNestedAttribute{Optional: true,
				MarkdownDescription: "SCM failure recovery actions.",
				Attributes: map[string]schema.Attribute{
					"restart_on_failure": schema.BoolAttribute{Optional: true, MarkdownDescription: "Register restart/restart/restart actions (default true)."},
				}},

			"environment":      schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Service environment written to HKLM ...\\Services\\<svc>\\Environment (REG_MULTI_SZ)."},
			"version_override": schema.StringAttribute{Optional: true, MarkdownDescription: "Overrides `artifact.version` — the pipeline's rollout/rollback lever."},
			"destroy_mode": schema.StringAttribute{Optional: true, Computed: true,
				Default:             stringdefault.StaticString("purge"),
				MarkdownDescription: "`purge` (default) | `unregister` | `abandon` (DESIGN §10.5)."},

			// artifact
			"artifact": schema.SingleNestedAttribute{Required: true,
				MarkdownDescription: "The build artifact to deploy (DESIGN §6.3).",
				Attributes: map[string]schema.Attribute{
					"type":       schema.StringAttribute{Required: true, MarkdownDescription: "`zip` | `nupkg`."},
					"version":    schema.StringAttribute{Required: true},
					"checksum":   schema.StringAttribute{Optional: true, MarkdownDescription: "`sha256:<hex>` — required for `zip`."},
					"fetch_mode": schema.StringAttribute{Optional: true},
					"source": schema.SingleNestedAttribute{Required: true,
						MarkdownDescription: "Where the artifact is fetched from.",
						Attributes: map[string]schema.Attribute{
							"type":       schema.StringAttribute{Required: true, MarkdownDescription: "`http` | `file` | `nuget_feed`."},
							"url":        schema.StringAttribute{Optional: true},
							"path":       schema.StringAttribute{Optional: true},
							"feed_url":   schema.StringAttribute{Optional: true},
							"package_id": schema.StringAttribute{Optional: true},
						}},
				}},

			// computed identity + status
			"id": schema.StringAttribute{Computed: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"spec_hash":        schema.StringAttribute{Computed: true},
			"deployed_version": schema.StringAttribute{Computed: true},
			"previous_version": schema.StringAttribute{Computed: true},
			"release_path":     schema.StringAttribute{Computed: true},
			"service_status":   schema.StringAttribute{Computed: true},
		},
	}
}

func (r *WindowsServiceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	r.pd = req.ProviderData.(*providerData)
}

// buildDeployment implements deploymentPattern: project the typed model onto a
// validated spec.Deployment merged with the provider default_target.
func (m *windowsServiceModel) buildDeployment(ctx context.Context, pd *providerData) (*spec.Deployment, diag.Diagnostics) {
	var diags diag.Diagnostics

	art, aDiags := buildArtifact(m.Artifact)
	diags.Append(aDiags...)
	if diags.HasError() {
		return nil, diags
	}

	var args []string
	if !m.Args.IsNull() && !m.Args.IsUnknown() {
		diags.Append(m.Args.ElementsAs(ctx, &args, false)...)
	}
	env := map[string]string{}
	if !m.Environment.IsNull() && !m.Environment.IsUnknown() {
		diags.Append(m.Environment.ElementsAs(ctx, &env, false)...)
	}
	if diags.HasError() {
		return nil, diags
	}
	if len(env) == 0 {
		env = nil
	}

	tgt := buildTarget(targetInput{
		Host:          m.Host.ValueString(),
		Transport:     m.Transport.ValueString(),
		OS:            string(spec.OSWindows),
		Port:          m.Port.ValueInt64(),
		Username:      m.Username.ValueString(),
		PasswordEnv:   m.PasswordEnv.ValueString(),
		WinRMUseHTTPS: m.WinRMUseHTTPS,
		WinRMInsecure: m.WinRMInsecureSkipVerify.ValueBool(),
	})

	version := m.Artifact.Version.ValueString()
	if v := m.VersionOverride.ValueString(); v != "" {
		version = v
	}
	art.Version = version

	d := &spec.Deployment{
		APIVersion: "labdeploy/v1",
		Kind:       "Deployment",
		Metadata:   spec.Metadata{Name: m.Name.ValueString()},
		Target:     tgt,
		Artifact:   art,
		Pattern: spec.Pattern{
			Type:               spec.PatternWindowsService,
			InstallRoot:        m.InstallRoot.ValueString(),
			PostInstall:        m.PostInstall.ValueString(),
			ServiceName:        m.ServiceName.ValueString(),
			DisplayName:        m.DisplayName.ValueString(),
			Description:        m.Description.ValueString(),
			Exe:                m.Exe.ValueString(),
			Args:               args,
			Wrapper:            m.Wrapper.ValueString(),
			WinswExe:           m.WinswExe.ValueString(),
			StartType:          m.StartType.ValueString(),
			Account:            accountFrom(m.Account),
			Recovery:           recoveryFrom(m.Recovery),
			StopTimeoutSeconds: int(m.StopTimeoutSeconds.ValueInt64()),
		},
		Environment: env,
	}

	if _, fDiags := finalizeDeployment(d, pd); fDiags.HasError() {
		diags.Append(fDiags...)
		return nil, diags
	}
	return d, diags
}

// prepare builds the deployment + single-target wrapper and stamps identity onto
// the model.
func (r *WindowsServiceResource) prepare(ctx context.Context, m *windowsServiceModel) (*singleTargetDeployment, diag.Diagnostics) {
	d, diags := m.buildDeployment(ctx, r.pd)
	if diags.HasError() {
		return nil, diags
	}
	std, err := newSingleTargetDeployment(d)
	if err != nil {
		diags.AddError("Invalid windows_service configuration", err.Error())
		return nil, diags
	}
	hash, err := hashDeployment(d)
	if err != nil {
		diags.AddError("internal", fmt.Sprintf("spec hash: %v", err))
		return nil, diags
	}
	m.ID = types.StringValue(std.id())
	m.OS = types.StringValue(string(spec.OSWindows))
	m.SpecHash = types.StringValue(hash)
	return std, diags
}

func (r *WindowsServiceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan windowsServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	std, diags := r.prepare(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	st, warns, err := std.apply(ctx)
	appendWarnings(&resp.Diagnostics, warns)
	if err != nil {
		resp.Diagnostics.AddError("Deploy failed", err.Error())
		return
	}
	fillWSStatus(&plan, st)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *WindowsServiceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan windowsServiceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	std, diags := r.prepare(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	st, warns, err := std.apply(ctx)
	appendWarnings(&resp.Diagnostics, warns)
	if err != nil {
		resp.Diagnostics.AddError("Deploy failed", err.Error())
		return
	}
	fillWSStatus(&plan, st)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *WindowsServiceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state windowsServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	std, diags := r.prepare(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	st, warns, err := std.read(ctx)
	appendWarnings(&resp.Diagnostics, warns)
	if err != nil {
		// DESIGN §10.4: refresh MUST fail loudly on transport errors — an
		// unreachable target is not a deleted resource. Retain prior state.
		resp.Diagnostics.AddError("labdeploy read failed", err.Error())
		return
	}
	if st == nil { // manifest absent ⇒ resource gone
		resp.State.RemoveResource(ctx)
		return
	}
	fillWSStatus(&state, st)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *WindowsServiceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state windowsServiceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, diags := state.buildDeployment(ctx, r.pd)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	std, err := newSingleTargetDeployment(d)
	if err != nil {
		resp.Diagnostics.AddError("Invalid windows_service configuration", err.Error())
		return
	}
	mode := state.DestroyMode.ValueString()
	if mode == "" {
		mode = "purge"
	}
	warns, err := std.destroy(ctx, mode)
	appendWarnings(&resp.Diagnostics, warns)
	if err != nil {
		resp.Diagnostics.AddError("Destroy failed", err.Error())
		return
	}
}

// ImportState is unsupported in v1 (DESIGN §5.2): the typed attributes cannot be
// reconstructed from the target manifest alone.
func (r *WindowsServiceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.AddError("Import not supported",
		"labdeploy_windows_service cannot be imported (DESIGN §5.2); re-apply the configuration instead.")
}

// --- helpers ----------------------------------------------------------------

func appendWarnings(diags *diag.Diagnostics, warns []string) {
	for _, w := range warns {
		diags.AddWarning("labdeploy", w)
	}
}

func fillWSStatus(m *windowsServiceModel, st *engine.Status) {
	if st == nil {
		return
	}
	m.DeployedVersion = types.StringValue(st.DeployedVersion)
	m.PreviousVersion = types.StringValue(st.PreviousVersion)
	m.ReleasePath = types.StringValue(st.ReleasePath)
	m.ServiceStatus = types.StringValue(st.ServiceStatus)
}
