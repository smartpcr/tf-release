package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// wsSpecYAML is a minimal, valid single-host windows_service deployment at the
// given artifact version. The checksum is a well-formed sha256:<64 hex> so the
// spec passes validation without a real artifact.
func wsSpecYAML(version string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
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
`, version, 0)
}

// TestUpdatePriorFromPersistedResolvedSpec is the item-2/3 regression: the prior
// spec is reconstructed from the PERSISTED resolved_spec, so it stays faithful even
// after the spec_file on disk is changed to a new version. This lets a same-artifact
// spec_file config change route correctly (rather than no-op) while an artifact
// upgrade is still detectable, and gives rollback the real prior configuration.
func TestUpdatePriorFromPersistedResolvedSpec(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	// Prior apply resolved+persisted version 1.0.0 from this file.
	if err := os.WriteFile(path, []byte(wsSpecYAML("1.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	resolvedState := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path)}
	prior1, _, err := r.resolveSpec(context.Background(), resolvedState)
	if err != nil {
		t.Fatalf("resolve prior: %v", err)
	}
	resolvedState.ResolvedSpec = marshalResolvedSpec(prior1)

	// The spec_file is then edited to a NEW artifact version on disk.
	if err := os.WriteFile(path, []byte(wsSpecYAML("2.0.0")), 0o600); err != nil {
		t.Fatalf("rewrite spec: %v", err)
	}
	// priorFromState must return the PERSISTED 1.0.0, NOT the mutated 2.0.0 on disk,
	// so Engine.Update can tell 2.0.0 is a real upgrade (Deploy) vs a config change.
	prior := r.priorFromState(context.Background(), resolvedState)
	if prior == nil {
		t.Fatalf("priorFromState must reconstruct persisted spec, got nil")
	}
	if prior.Artifact.Version != "1.0.0" {
		t.Fatalf("prior version = %q, want persisted 1.0.0 (not mutated 2.0.0)", prior.Artifact.Version)
	}
}

// TestPriorFromStateBackCompat covers state written before resolved_spec existed:
// inline spec is still faithful; a mutated spec_file is not, so priorFromState
// declines (nil ⇒ safe Deploy) rather than misroute.
func TestPriorFromStateBackCompat(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	r := &DeploymentResource{}
	inline := &deploymentModel{Spec: types.StringValue(wsSpecYAML("1.0.0")), SpecFile: types.StringNull(),
		ResolvedSpec: types.StringNull()}
	if prior := r.priorFromState(context.Background(), inline); prior == nil || prior.Artifact.Version != "1.0.0" {
		t.Fatalf("inline back-compat prior must reconstruct 1.0.0, got %+v", prior)
	}
	fileState := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue("/nonexistent/spec.yaml"),
		ResolvedSpec: types.StringNull(), DeployedVersion: types.StringNull()}
	if prior := r.priorFromState(context.Background(), fileState); prior != nil {
		t.Fatalf("spec_file back-compat prior with no deployed_version must decline (nil), got version %q", prior.Artifact.Version)
	}
}

// TestPriorFromStateLegacySpecFilePinsDeployedVersion covers item 2: legacy
// spec_file state (no resolved_spec) that DOES record a deployed_version must
// reconstruct a best-effort prior pinned to that version, so a same-version
// configuration change routes through Reconfigure (which re-runs CONFIGURE)
// instead of no-op'ing on Deploy's version/checksum short-circuit.
func TestPriorFromStateLegacySpecFilePinsDeployedVersion(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	// The file on disk now holds 2.0.0 (a config edit may have bumped it), but the
	// version actually deployed and recorded in state is 1.0.0.
	if err := os.WriteFile(path, []byte(wsSpecYAML("2.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	state := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path),
		ResolvedSpec: types.StringNull(), DeployedVersion: types.StringValue("1.0.0")}
	prior := r.priorFromState(context.Background(), state)
	if prior == nil {
		t.Fatalf("legacy spec_file with deployed_version must reconstruct a prior, got nil")
	}
	if prior.Artifact.Version != "1.0.0" {
		t.Fatalf("legacy prior must pin to recorded deployed_version 1.0.0, got %q", prior.Artifact.Version)
	}
}

// TestConfiguredSpecPath covers item 3 (second half): a forced replacement must be
// attributed to whichever spec attribute the config actually sets — spec_file when
// that is the source, not always path.Root("spec").
func TestConfiguredSpecPath(t *testing.T) {
	fileCfg := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue("/x/spec.yaml")}
	if got := configuredSpecPath(fileCfg); !got.Equal(path.Root("spec_file")) {
		t.Fatalf("spec_file config must attribute replacement to spec_file, got %s", got)
	}
	inlineCfg := &deploymentModel{Spec: types.StringValue("y"), SpecFile: types.StringNull()}
	if got := configuredSpecPath(inlineCfg); !got.Equal(path.Root("spec")) {
		t.Fatalf("inline config must attribute replacement to spec, got %s", got)
	}
}
