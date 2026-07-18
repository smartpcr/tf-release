// Package provider wires the terraform-plugin-framework surface (DESIGN §5).
package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// Address is the Terraform registry source address the provider server
// advertises (DESIGN §16.1). main.go serves the provider under this address and
// the provider test asserts it, keeping the two in sync.
const Address = "registry.local/smartpcr/labdeploy"

// ServeOpts returns the base providerserver.ServeOpts main.go serves the
// provider with. Centralizing the served source address here keeps main.go and
// the acceptance harness reading from a single source of truth (DESIGN §16.1);
// callers set Debug per-invocation.
func ServeOpts() providerserver.ServeOpts {
	return providerserver.ServeOpts{Address: Address}
}

var _ provider.Provider = (*LabDeployProvider)(nil)

type LabDeployProvider struct{ version string }

// providerData flows to resources via Configure.
type providerData struct {
	DefaultTarget *spec.Target
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &LabDeployProvider{version: version} }
}

func (p *LabDeployProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "labdeploy"
	resp.Version = p.version
}

type providerModel struct {
	DefaultTarget *defaultTargetModel `tfsdk:"default_target"`
}

type defaultTargetModel struct {
	Transport     types.String `tfsdk:"transport"`
	Hosts         types.List   `tfsdk:"hosts"`
	OS            types.String `tfsdk:"os"`
	Port          types.Int64  `tfsdk:"port"`
	Username      types.String `tfsdk:"username"`
	PasswordEnv   types.String `tfsdk:"password_env"`
	PrivateKeyEnv types.String `tfsdk:"private_key_env"`
	WinRMUseHTTPS types.Bool   `tfsdk:"winrm_use_https"`
	WinRMInsecure types.Bool   `tfsdk:"winrm_insecure_skip_verify"`
}

func (p *LabDeployProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Deploys build artifacts to lab machines over SSH/WinRM (DESIGN.md is normative).",
		Attributes: map[string]schema.Attribute{
			"default_target": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Field-level defaults merged under every spec's `target` block (DESIGN §6.2).",
				Attributes: map[string]schema.Attribute{
					"transport":                  schema.StringAttribute{Optional: true},
					"hosts":                      schema.ListAttribute{Optional: true, ElementType: types.StringType},
					"os":                         schema.StringAttribute{Optional: true},
					"port":                       schema.Int64Attribute{Optional: true},
					"username":                   schema.StringAttribute{Optional: true},
					"password_env":               schema.StringAttribute{Optional: true, MarkdownDescription: "ENV VAR NAME on the runner (never the secret itself)."},
					"private_key_env":            schema.StringAttribute{Optional: true},
					"winrm_use_https":            schema.BoolAttribute{Optional: true},
					"winrm_insecure_skip_verify": schema.BoolAttribute{Optional: true},
				},
			},
		},
	}
}

func (p *LabDeployProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pd := &providerData{}
	if dt := cfg.DefaultTarget; dt != nil {
		t := &spec.Target{
			Transport: spec.TransportKind(dt.Transport.ValueString()),
			OS:        spec.OSKind(dt.OS.ValueString()),
			Port:      int(dt.Port.ValueInt64()),
		}
		t.Credentials.Username = dt.Username.ValueString()
		t.Credentials.PasswordEnv = dt.PasswordEnv.ValueString()
		t.Credentials.PrivateKeyEnv = dt.PrivateKeyEnv.ValueString()
		if !dt.WinRMUseHTTPS.IsNull() {
			v := dt.WinRMUseHTTPS.ValueBool()
			t.WinRM.UseHTTPS = &v
		}
		t.WinRM.InsecureSkipVerify = dt.WinRMInsecure.ValueBool()
		if !dt.Hosts.IsNull() {
			var hosts []string
			resp.Diagnostics.Append(dt.Hosts.ElementsAs(ctx, &hosts, false)...)
			t.Hosts = hosts
		}
		pd.DefaultTarget = t
	}
	resp.ResourceData = pd
	resp.DataSourceData = pd
}

func (p *LabDeployProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewDeploymentResource,
		NewE2ETestResource,
		NewWindowsServiceResource,
	}
}

func (p *LabDeployProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
