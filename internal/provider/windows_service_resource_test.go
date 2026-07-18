package provider

import (
	"context"
	"strings"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

const testChecksum = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// validWSModel returns a windowsServiceModel that projects to a valid
// windows_service Deployment (single host, zip artifact with checksum, http
// source).
func validWSModel(t *testing.T) *windowsServiceModel {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "s3cret")
	args, d := types.ListValueFrom(context.Background(), types.StringType, []string{"--config", `C:\deploy\svc.json`})
	if d.HasError() {
		t.Fatalf("args list: %v", d)
	}
	env, d := types.MapValueFrom(context.Background(), types.StringType, map[string]string{"ASPNETCORE_ENVIRONMENT": "Production"})
	if d.HasError() {
		t.Fatalf("env map: %v", d)
	}
	return &windowsServiceModel{
		Name:               types.StringValue("payments-svc"),
		Host:               types.StringValue("lab-node-01"),
		Transport:          types.StringValue("winrm"),
		Port:               types.Int64Value(5986),
		Username:           types.StringValue("labadmin"),
		PasswordEnv:        types.StringValue("LABDEPLOY_PASSWORD"),
		ServiceName:        types.StringValue("PaymentsSvc"),
		DisplayName:        types.StringValue("Payments Service"),
		Description:        types.StringValue("Handles payments"),
		Exe:                types.StringValue(`bin\payments.exe`),
		Args:               args,
		StartType:          types.StringValue("auto"),
		Wrapper:            types.StringValue("none"),
		InstallRoot:        types.StringValue(`C:\deploy`),
		StopTimeoutSeconds: types.Int64Value(45),
		Environment:        env,
		VersionOverride:    types.StringNull(),
		DestroyMode:        types.StringValue("purge"),
		Artifact: &artifactModel{
			Type:     types.StringValue("zip"),
			Version:  types.StringValue("1.2.3"),
			Checksum: types.StringValue(testChecksum),
			Source: &sourceModel{
				Type: types.StringValue("http"),
				URL:  types.StringValue("https://artifacts.example/payments-1.2.3.zip"),
			},
		},
	}
}

// TestWSResourceMetadataTypeName pins the resource type name.
func TestWSResourceMetadataTypeName(t *testing.T) {
	r := &WindowsServiceResource{}
	resp := &fwresource.MetadataResponse{}
	r.Metadata(context.Background(), fwresource.MetadataRequest{ProviderTypeName: "labdeploy"}, resp)
	if resp.TypeName != "labdeploy_windows_service" {
		t.Fatalf("TypeName = %q, want labdeploy_windows_service", resp.TypeName)
	}
}

// TestWSResourceSchemaValid asserts the schema is internally consistent (the
// framework validates required/computed/default combinations for us).
func TestWSResourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	r := &WindowsServiceResource{}
	resp := &fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	if _, ok := resp.Schema.Attributes["service_name"]; !ok {
		t.Fatal("schema missing service_name")
	}
	// service_name must force replacement (immutable identity).
	sn := resp.Schema.Attributes["service_name"].(fwschema.StringAttribute)
	if len(sn.PlanModifiers) == 0 {
		t.Fatal("service_name should carry a RequiresReplace plan modifier")
	}
}

// TestWSBuildDeployment covers the core projection: typed attributes ⇒ a valid
// windows_service Deployment with exactly one host and the pattern fields set.
func TestWSBuildDeployment(t *testing.T) {
	m := validWSModel(t)
	d, diags := m.buildDeployment(context.Background(), nil)
	if diags.HasError() {
		t.Fatalf("buildDeployment diagnostics: %v", diags)
	}
	if d.APIVersion != "labdeploy/v1" || d.Kind != "Deployment" {
		t.Fatalf("apiVersion/kind = %q/%q", d.APIVersion, d.Kind)
	}
	if d.Metadata.Name != "payments-svc" {
		t.Fatalf("metadata.name = %q", d.Metadata.Name)
	}
	if len(d.Target.Hosts) != 1 || d.Target.Hosts[0] != "lab-node-01" {
		t.Fatalf("target.hosts = %v, want [lab-node-01]", d.Target.Hosts)
	}
	if d.Target.OS != spec.OSWindows {
		t.Fatalf("target.os = %q, want windows", d.Target.OS)
	}
	if d.Pattern.Type != spec.PatternWindowsService {
		t.Fatalf("pattern.type = %q", d.Pattern.Type)
	}
	if d.Pattern.ServiceName != "PaymentsSvc" || d.Pattern.Exe != `bin\payments.exe` {
		t.Fatalf("pattern service/exe = %q/%q", d.Pattern.ServiceName, d.Pattern.Exe)
	}
	if d.Pattern.StopTimeoutSeconds != 45 {
		t.Fatalf("stop_timeout_seconds = %d, want 45", d.Pattern.StopTimeoutSeconds)
	}
	if len(d.Pattern.Args) != 2 || d.Pattern.Args[0] != "--config" {
		t.Fatalf("pattern.args = %v", d.Pattern.Args)
	}
	if d.Environment["ASPNETCORE_ENVIRONMENT"] != "Production" {
		t.Fatalf("environment = %v", d.Environment)
	}
	if d.Artifact.Version != "1.2.3" {
		t.Fatalf("artifact.version = %q", d.Artifact.Version)
	}
}

// TestWSVersionOverride: version_override wins over artifact.version and changes
// the spec hash (DESIGN D7).
func TestWSVersionOverride(t *testing.T) {
	base := validWSModel(t)
	dBase, diags := base.buildDeployment(context.Background(), nil)
	if diags.HasError() {
		t.Fatalf("base build: %v", diags)
	}
	hBase, err := hashDeployment(dBase)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}

	over := validWSModel(t)
	over.VersionOverride = types.StringValue("9.9.9")
	dOver, diags := over.buildDeployment(context.Background(), nil)
	if diags.HasError() {
		t.Fatalf("override build: %v", diags)
	}
	if dOver.Artifact.Version != "9.9.9" {
		t.Fatalf("override version = %q, want 9.9.9", dOver.Artifact.Version)
	}
	hOver, err := hashDeployment(dOver)
	if err != nil {
		t.Fatalf("hash override: %v", err)
	}
	if hBase == hOver {
		t.Fatal("version_override must change the spec hash")
	}
}

// TestWSDefaultTargetMerge: provider default_target fills unset connection
// fields (DESIGN §6.2).
func TestWSDefaultTargetMerge(t *testing.T) {
	m := validWSModel(t)
	m.Username = types.StringNull() // leave username to the provider default
	pw := true
	pd := &providerData{DefaultTarget: &spec.Target{
		Credentials: spec.Credentials{Username: "defaultadmin", PasswordEnv: "LABDEPLOY_PASSWORD"},
		WinRM:       spec.WinRMOpts{UseHTTPS: &pw},
	}}
	d, diags := m.buildDeployment(context.Background(), pd)
	if diags.HasError() {
		t.Fatalf("build with defaults: %v", diags)
	}
	if d.Target.Credentials.Username != "defaultadmin" {
		t.Fatalf("username = %q, want merged default", d.Target.Credentials.Username)
	}
}

// TestWSBuildDeploymentInvalid: a bad checksum surfaces an ERR_SPEC_INVALID
// validation diagnostic instead of silently building.
func TestWSBuildDeploymentInvalid(t *testing.T) {
	m := validWSModel(t)
	m.Artifact.Checksum = types.StringValue("not-a-sha")
	_, diags := m.buildDeployment(context.Background(), nil)
	if !diags.HasError() {
		t.Fatal("expected a validation error for a bad checksum")
	}
}

// TestSingleTargetGuard: the single-host invariant is enforced.
func TestSingleTargetGuard(t *testing.T) {
	d := &spec.Deployment{Target: spec.Target{Hosts: []string{"a", "b"}}}
	if _, err := newSingleTargetDeployment(d); err == nil {
		t.Fatal("expected an error for a multi-host deployment")
	}
	d1 := &spec.Deployment{Metadata: spec.Metadata{Name: "x"}, Target: spec.Target{Hosts: []string{"only"}}}
	std, err := newSingleTargetDeployment(d1)
	if err != nil {
		t.Fatalf("single host should be accepted: %v", err)
	}
	if std.host() != "only" {
		t.Fatalf("host() = %q", std.host())
	}
	if !strings.Contains(std.id(), "x@only") {
		t.Fatalf("id() = %q, want to contain x@only", std.id())
	}
}

// TestWSImportUnsupported: import returns an error (v1 unsupported).
func TestWSImportUnsupported(t *testing.T) {
	r := &WindowsServiceResource{}
	resp := &fwresource.ImportStateResponse{}
	r.ImportState(context.Background(), fwresource.ImportStateRequest{ID: "x"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("ImportState must be unsupported in v1")
	}
}
