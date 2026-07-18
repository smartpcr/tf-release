package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// deploymentPattern is implemented by the strongly-typed, single-pattern
// resources (labdeploy_windows_service today; node_web_app / dotnet_api later).
// Instead of accepting an opaque YAML/JSON `spec` string like
// labdeploy_deployment, these resources expose the pattern's fields as first
// class Terraform attributes and PROJECT them onto the canonical
// spec.Deployment the engine consumes. Implementing this seam once lets the
// single-target apply/read/destroy plumbing (single_target_deployment.go) be
// written a single time and reused across every typed pattern resource.
type deploymentPattern interface {
	// buildDeployment converts the typed resource model into a validated
	// spec.Deployment, merged with the provider `default_target` block. The
	// returned Deployment always targets exactly one host — the typed resources
	// model a single lab machine, matching DESIGN §10.2 single-target rollout.
	buildDeployment(ctx context.Context, pd *providerData) (*spec.Deployment, diag.Diagnostics)
}

// --- Shared nested models (reused by every typed pattern resource) -----------

// accountModel maps pattern.account (DESIGN §9.2 S4): a non-builtin service
// account whose password is injected from a runner env var by NAME.
type accountModel struct {
	Username    types.String `tfsdk:"username"`
	PasswordEnv types.String `tfsdk:"password_env"`
}

// recoveryModel maps pattern.recovery: SCM restart-on-failure actions.
type recoveryModel struct {
	RestartOnFailure types.Bool `tfsdk:"restart_on_failure"`
}

// artifactModel mirrors spec.Artifact (DESIGN §6.3).
type artifactModel struct {
	Type      types.String `tfsdk:"type"`
	Version   types.String `tfsdk:"version"`
	Checksum  types.String `tfsdk:"checksum"`
	FetchMode types.String `tfsdk:"fetch_mode"`
	Source    *sourceModel `tfsdk:"source"`
}

// sourceModel mirrors spec.Source (DESIGN §6.3) — the subset relevant to
// zip/nupkg artifacts a windows_service is delivered as.
type sourceModel struct {
	Type      types.String `tfsdk:"type"`
	URL       types.String `tfsdk:"url"`
	Path      types.String `tfsdk:"path"`
	FeedURL   types.String `tfsdk:"feed_url"`
	PackageID types.String `tfsdk:"package_id"`
}

// --- Shared projection helpers ----------------------------------------------

// targetInput is the flat, single-host connection description a typed resource
// passes to buildTarget. Fields left at their zero value are merged from the
// provider `default_target` block downstream (DESIGN §6.2 merge-then-validate).
type targetInput struct {
	Host          string
	Transport     string
	OS            string
	Port          int64
	Username      string
	PasswordEnv   string
	PrivateKeyEnv string
	WinRMUseHTTPS types.Bool
	WinRMInsecure bool
}

// buildTarget projects the flat connection attributes onto a spec.Target with
// exactly one host.
func buildTarget(in targetInput) spec.Target {
	t := spec.Target{
		Transport: spec.TransportKind(in.Transport),
		OS:        spec.OSKind(in.OS),
		Port:      int(in.Port),
	}
	if in.Host != "" {
		t.Hosts = []string{in.Host}
	}
	t.Credentials.Username = in.Username
	t.Credentials.PasswordEnv = in.PasswordEnv
	t.Credentials.PrivateKeyEnv = in.PrivateKeyEnv
	if !in.WinRMUseHTTPS.IsNull() && !in.WinRMUseHTTPS.IsUnknown() {
		v := in.WinRMUseHTTPS.ValueBool()
		t.WinRM.UseHTTPS = &v
	}
	t.WinRM.InsecureSkipVerify = in.WinRMInsecure
	return t
}

// buildArtifact projects the nested artifact block onto a spec.Artifact.
func buildArtifact(m *artifactModel) (spec.Artifact, diag.Diagnostics) {
	var diags diag.Diagnostics
	if m == nil {
		diags.AddError("[ERR_SPEC_INVALID] artifact required", "the `artifact` block must be set")
		return spec.Artifact{}, diags
	}
	a := spec.Artifact{
		Type:      spec.ArtifactType(m.Type.ValueString()),
		Version:   m.Version.ValueString(),
		Checksum:  m.Checksum.ValueString(),
		FetchMode: m.FetchMode.ValueString(),
	}
	if m.Source != nil {
		a.Source = spec.Source{
			Type:      m.Source.Type.ValueString(),
			URL:       m.Source.URL.ValueString(),
			Path:      m.Source.Path.ValueString(),
			FeedURL:   m.Source.FeedURL.ValueString(),
			PackageID: m.Source.PackageID.ValueString(),
		}
	}
	return a, diags
}

// accountFrom projects the optional account block onto spec.ServiceAccount.
func accountFrom(m *accountModel) spec.ServiceAccount {
	if m == nil {
		return spec.ServiceAccount{}
	}
	return spec.ServiceAccount{
		Username:    m.Username.ValueString(),
		PasswordEnv: m.PasswordEnv.ValueString(),
	}
}

// recoveryFrom projects the optional recovery block onto spec.Recovery.
func recoveryFrom(m *recoveryModel) spec.Recovery {
	if m == nil {
		return spec.Recovery{}
	}
	if m.RestartOnFailure.IsNull() || m.RestartOnFailure.IsUnknown() {
		return spec.Recovery{}
	}
	v := m.RestartOnFailure.ValueBool()
	return spec.Recovery{RestartOnFailure: &v}
}

// finalizeDeployment merges the provider default_target, validates the assembled
// Deployment, and returns its canonical spec hash. Every typed pattern resource
// funnels through here so provider defaults and validation behave identically to
// the spec-string labdeploy_deployment path (DESIGN §6.2).
func finalizeDeployment(d *spec.Deployment, pd *providerData) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if pd != nil && pd.DefaultTarget != nil {
		spec.MergeTargetDefaults(&d.Target, pd.DefaultTarget)
	}
	if err := spec.ValidateDeployment(d); err != nil {
		diags.AddError("Invalid windows_service configuration", err.Error())
		return "", diags
	}
	hash, err := hashDeployment(d)
	if err != nil {
		diags.AddError("internal", fmt.Sprintf("spec hash: %v", err))
		return "", diags
	}
	return hash, diags
}

// hashDeployment reproduces the spec_hash contract used by
// spec.ParseDeploymentLenient: canonical JSON (sorted keys) over the assembled
// Deployment, sha256-hexed. This keeps the typed resource's spec_hash comparable
// to the labdeploy_deployment hash for the same effective spec (DESIGN D7).
func hashDeployment(d *spec.Deployment) (string, error) {
	canonBytes, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return spec.CanonicalHash(canonBytes)
}
