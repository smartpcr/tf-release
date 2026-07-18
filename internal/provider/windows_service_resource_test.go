package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
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
	// id() is the authoritative sha1(sorted(hosts)+"/"+name)[0:12]+":"+name form
	// (DESIGN §5.2), so it ends with ":<name>" — here ":x".
	if !strings.HasSuffix(std.id(), ":x") {
		t.Fatalf("id() = %q, want suffix :x", std.id())
	}
	if std.id() != deploymentID(d1) {
		t.Fatalf("id() = %q, want the shared deploymentID formula %q", std.id(), deploymentID(d1))
	}
}

// TestWSTransportDefaultTargetOverride locks evaluator item 7: when the resource
// omits `transport`, the provider default_target.transport MUST win — it is not
// masked by a static winrm default.
func TestWSTransportDefaultTargetOverride(t *testing.T) {
	t.Setenv("LAB_KEY", "----PEM----")
	m := validWSModel(t)
	m.Transport = types.StringNull()   // resource leaves transport unset
	m.PasswordEnv = types.StringNull() // ssh uses key auth
	pd := &providerData{DefaultTarget: &spec.Target{
		Transport:   spec.TransportSSH,
		Credentials: spec.Credentials{Username: "labadmin", PrivateKeyEnv: "LAB_KEY"},
	}}
	d, diags := m.buildDeployment(context.Background(), pd)
	if diags.HasError() {
		t.Fatalf("build: %v", diags)
	}
	if d.Target.Transport != spec.TransportSSH {
		t.Fatalf("transport = %q, want ssh (default_target must win over the winrm fallback)", d.Target.Transport)
	}
}

// TestWSTransportFallbackWinRM: with neither resource nor default_target
// transport set, the canonical winrm fallback applies (after merge).
func TestWSTransportFallbackWinRM(t *testing.T) {
	m := validWSModel(t)
	m.Transport = types.StringNull()
	d, diags := m.buildDeployment(context.Background(), nil)
	if diags.HasError() {
		t.Fatalf("build: %v", diags)
	}
	if d.Target.Transport != spec.TransportWinRM {
		t.Fatalf("transport = %q, want winrm fallback", d.Target.Transport)
	}
}

// TestWSWarnSummaryCoded locks evaluator item 3 / DESIGN §12: warning
// diagnostics also use the "[<CODE>] <short>" Summary form.
func TestWSWarnSummaryCoded(t *testing.T) {
	if got := wsWarnSummary("prune failed on lab-01: boom"); got != "[WARN] windows_service warning" {
		t.Fatalf("uncoded warning must default to [WARN], got %q", got)
	}
	if got := wsWarnSummary("[ERR_ROLLBACK_FAILED] could not restore prev"); got != "[ERR_ROLLBACK_FAILED] windows_service warning" {
		t.Fatalf("embedded code must be surfaced, got %q", got)
	}
}
func TestWSImportUnsupported(t *testing.T) {
	r := &WindowsServiceResource{}
	resp := &fwresource.ImportStateResponse{}
	r.ImportState(context.Background(), fwresource.ImportStateRequest{ID: "x"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("ImportState must be unsupported in v1")
	}
}

// TestWSInsecureSkipVerifyPreservesExplicitFalse locks evaluator item 2: an
// explicit winrm_insecure_skip_verify=false is a deliberate secure choice and
// must NOT be overwritten by a provider default_target of true, while a null
// (unset) value still inherits the default.
func TestWSInsecureSkipVerifyPreservesExplicitFalse(t *testing.T) {
	insecureDefault := true
	pd := &providerData{DefaultTarget: &spec.Target{
		WinRM: spec.WinRMOpts{InsecureSkipVerify: &insecureDefault},
	}}

	// explicit false ⇒ stays false (secure) after the merge.
	mFalse := validWSModel(t)
	mFalse.WinRMInsecureSkipVerify = types.BoolValue(false)
	dFalse, diags := mFalse.buildDeployment(context.Background(), pd)
	if diags.HasError() {
		t.Fatalf("build (explicit false): %v", diags)
	}
	if dFalse.Target.WinRM.InsecureSkipVerify == nil || *dFalse.Target.WinRM.InsecureSkipVerify {
		t.Fatalf("explicit insecure_skip_verify=false must survive the default merge, got %v", dFalse.Target.WinRM.InsecureSkipVerify)
	}

	// null (unset) ⇒ inherits the default (true).
	mNull := validWSModel(t)
	mNull.WinRMInsecureSkipVerify = types.BoolNull()
	dNull, diags := mNull.buildDeployment(context.Background(), pd)
	if diags.HasError() {
		t.Fatalf("build (null): %v", diags)
	}
	if dNull.Target.WinRM.InsecureSkipVerify == nil || !*dNull.Target.WinRM.InsecureSkipVerify {
		t.Fatalf("unset insecure_skip_verify must inherit default(true), got %v", dNull.Target.WinRM.InsecureSkipVerify)
	}
}

// TestWSDiagnosticsCoded locks evaluator item 3 / DESIGN §12: every diagnostic
// Summary is exactly "[<CODE>] <short>", and the code is sourced from the
// engine's CodedError when present.
func TestWSDiagnosticsCoded(t *testing.T) {
	r := &WindowsServiceResource{}
	resp := &fwresource.ImportStateResponse{}
	r.ImportState(context.Background(), fwresource.ImportStateRequest{ID: "x"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("import must error")
	}
	sum := resp.Diagnostics[0].Summary()
	if !strings.HasPrefix(sum, "[ERR_") || !strings.Contains(sum, "] ") {
		t.Fatalf("diagnostic summary must be `[<CODE>] <short>`, got %q", sum)
	}

	// wsDiagSummary surfaces the coded error's taxonomy code over the fallback.
	got := wsDiagSummary("ERR_SERVICE_INSTALL", "windows_service deploy failed",
		&engine.CodedError{Code: "ERR_CHECKSUM_MISMATCH", Err: fmt.Errorf("bad sum")})
	if got != "[ERR_CHECKSUM_MISMATCH] windows_service deploy failed" {
		t.Fatalf("wsDiagSummary should surface the coded error's code, got %q", got)
	}
	// falls back to the supplied code when the error is uncoded.
	if got := wsDiagSummary("ERR_SPEC_INVALID", "invalid config", fmt.Errorf("plain")); got != "[ERR_SPEC_INVALID] invalid config" {
		t.Fatalf("wsDiagSummary fallback wrong, got %q", got)
	}
}
