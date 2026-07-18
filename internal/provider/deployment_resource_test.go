package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

// TestUpdatePriorSpecFileNotReconstructed is the item-2/3 regression: when the
// prior state used spec_file, the file on the runner may already hold the NEW
// contents by Update time. faithfulPriorSpec MUST NOT reread it (which would make
// an artifact upgrade look like a same-artifact config change and misroute it
// through Reconfigure, skipping the fetch of the new artifact). It returns nil so
// Engine.Update falls back to a safe Deploy. Inline spec, persisted verbatim in
// state, IS reconstructed faithfully.
func TestUpdatePriorSpecFileNotReconstructed(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	// Prior apply deployed version 1.0.0 from this file.
	if err := os.WriteFile(path, []byte(wsSpecYAML("1.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	priorState := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path)}
	// The file is edited to a NEW artifact version before Update runs.
	if err := os.WriteFile(path, []byte(wsSpecYAML("2.0.0")), 0o600); err != nil {
		t.Fatalf("rewrite spec: %v", err)
	}
	if prior := r.faithfulPriorSpec(context.Background(), priorState); prior != nil {
		t.Fatalf("spec_file prior must be treated as unavailable (nil) so an upgrade deploys, got version %q",
			prior.Artifact.Version)
	}

	// Inline spec state is faithful — the prior version is reconstructed verbatim.
	inlineState := &deploymentModel{Spec: types.StringValue(wsSpecYAML("1.0.0")), SpecFile: types.StringNull()}
	prior := r.faithfulPriorSpec(context.Background(), inlineState)
	if prior == nil {
		t.Fatalf("inline prior must be reconstructed, got nil")
	}
	if prior.Artifact.Version != "1.0.0" {
		t.Fatalf("inline prior version = %q, want 1.0.0", prior.Artifact.Version)
	}
}
