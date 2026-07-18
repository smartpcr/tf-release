package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// wsSpecYAML is a minimal, valid single-host windows_service deployment at the
// given artifact version. The checksum is a well-formed sha256:<64 hex> so the
// spec passes validation without a real artifact.
func wsSpecYAML(version string) string {
	return wsSpecYAMLHost(version, "lab-01")
}

// wsSpecYAMLHost is wsSpecYAML with a configurable single host, for exercising
// immutable-field (host) edits between the deployed snapshot and the on-disk file.
func wsSpecYAMLHost(version, host string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["%s"]
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
`, host, version, 0)
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

// TestPriorFromStateLegacySpecFileDeclines covers item 2: legacy spec_file state
// (no resolved_spec) must NOT fabricate a prior from the on-disk file + recorded
// deployed_version — that would falsely promise transactional restoration. It must
// decline (nil) so ModifyPlan can force a truthful replacement instead.
func TestPriorFromStateLegacySpecFileDeclines(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(wsSpecYAML("2.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	state := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path),
		ResolvedSpec: types.StringNull(), DeployedVersion: types.StringValue("1.0.0")}
	if prior := r.priorFromState(context.Background(), state); prior != nil {
		t.Fatalf("legacy spec_file prior must decline (nil) even with deployed_version, got version %q", prior.Artifact.Version)
	}
}

// TestLegacyReplaceRequired covers item 2: when no faithful prior snapshot exists
// (oldSpec==nil), a real spec change (spec_hash drift) forces a replacement, while an
// unchanged spec forces nothing; a faithful prior (oldSpec!=nil) never triggers the
// legacy migration path.
func TestLegacyReplaceRequired(t *testing.T) {
	stateHash := &deploymentModel{SpecHash: types.StringValue("old")}
	if !legacyReplaceRequired(nil, "new", stateHash) {
		t.Fatal("no snapshot + hash drift must require replacement")
	}
	if legacyReplaceRequired(nil, "old", stateHash) {
		t.Fatal("no snapshot + no drift must NOT require replacement")
	}
	if legacyReplaceRequired(&spec.Deployment{}, "new", stateHash) {
		t.Fatal("faithful prior must never take the legacy replacement path")
	}
}

// TestStateSpecUsesPersistedSnapshot covers items 2+3: Read/Delete must operate on the
// DEPLOYED identity from resolved_spec, never a fresh re-read of a mutated spec_file. An
// immutable-field edit on disk (here the host) must NOT change what stateSpec returns
// while a snapshot exists; only legacy state without a snapshot falls back to the file.
func TestStateSpecUsesPersistedSnapshot(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	// Deployed against host lab-01; snapshot captured from that resolve.
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("1.0.0", "lab-01")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	state := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path)}
	deployed, _, err := r.resolveSpec(context.Background(), state)
	if err != nil {
		t.Fatalf("resolve deployed: %v", err)
	}
	state.ResolvedSpec = marshalResolvedSpec(deployed)

	// The spec_file is then edited to point at a DIFFERENT host (immutable field).
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("1.0.0", "lab-99")), 0o600); err != nil {
		t.Fatalf("rewrite spec: %v", err)
	}
	got, _, err := r.stateSpec(context.Background(), state)
	if err != nil {
		t.Fatalf("stateSpec: %v", err)
	}
	if len(got.Target.Hosts) != 1 || got.Target.Hosts[0] != "lab-01" {
		t.Fatalf("stateSpec must return DEPLOYED host lab-01 from snapshot, got %v", got.Target.Hosts)
	}
	// Legacy state with no snapshot falls back to the on-disk file (only source).
	legacy := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path),
		ResolvedSpec: types.StringNull()}
	lg, _, err := r.stateSpec(context.Background(), legacy)
	if err != nil {
		t.Fatalf("stateSpec legacy: %v", err)
	}
	if lg.Target.Hosts[0] != "lab-99" {
		t.Fatalf("legacy stateSpec must fall back to on-disk host lab-99, got %v", lg.Target.Hosts)
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
