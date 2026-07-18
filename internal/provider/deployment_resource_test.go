package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
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

// wsSpecYAMLNoHost is wsSpecYAML with target.hosts OMITTED, so a provider
// default_target must supply the host — used to prove a changed default host cannot be
// silently adopted during an update of unverifiable state.
func wsSpecYAMLNoHost(version string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
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
	prior, perr := r.priorFromState(context.Background(), resolvedState)
	if perr != nil {
		t.Fatalf("priorFromState errored: %v", perr)
	}
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
	if prior, perr := r.priorFromState(context.Background(), inline); perr != nil || prior == nil || prior.Artifact.Version != "1.0.0" {
		t.Fatalf("inline back-compat prior must reconstruct 1.0.0, got %+v (err %v)", prior, perr)
	}
	fileState := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue("/nonexistent/spec.yaml"),
		ResolvedSpec: types.StringNull(), DeployedVersion: types.StringNull()}
	if prior, perr := r.priorFromState(context.Background(), fileState); perr != nil || prior != nil {
		t.Fatalf("spec_file back-compat prior with no deployed_version must decline (nil,nil), got %+v (err %v)", prior, perr)
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
	if prior, perr := r.priorFromState(context.Background(), state); perr != nil || prior != nil {
		t.Fatalf("legacy spec_file prior must decline (nil,nil) even with deployed_version, got %+v (err %v)", prior, perr)
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
	got, _, verified, err := r.stateSpec(context.Background(), state)
	if err != nil {
		t.Fatalf("stateSpec: %v", err)
	}
	if !verified {
		t.Fatalf("snapshot-backed stateSpec must be verified")
	}
	if len(got.Target.Hosts) != 1 || got.Target.Hosts[0] != "lab-01" {
		t.Fatalf("stateSpec must return DEPLOYED host lab-01 from snapshot, got %v", got.Target.Hosts)
	}
	// Legacy state with no snapshot falls back to the on-disk file (only source) but is
	// flagged UNVERIFIED so Read will not treat a missing manifest as absence.
	legacy := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path),
		ResolvedSpec: types.StringNull()}
	lg, _, lgVerified, err := r.stateSpec(context.Background(), legacy)
	if err != nil {
		t.Fatalf("stateSpec legacy: %v", err)
	}
	if lgVerified {
		t.Fatalf("legacy spec_file stateSpec must be UNVERIFIED")
	}
	if lg.Target.Hosts[0] != "lab-99" {
		t.Fatalf("legacy stateSpec must fall back to on-disk host lab-99, got %v", lg.Target.Hosts)
	}
}

// TestStateSpecCorruptSnapshotErrors covers item 2: a non-empty but undecodable
// resolved_spec must surface a state-corruption error, NOT silently fall back to the
// mutable spec_file (which could target the wrong identity).
func TestStateSpecCorruptSnapshotErrors(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("1.0.0", "lab-01")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	state := &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue(path),
		ResolvedSpec: types.StringValue("{ this is not valid json")}
	_, _, _, err := r.stateSpec(context.Background(), state)
	if err == nil {
		t.Fatal("corrupt resolved_spec must return an error, not fall back to spec_file")
	}
	if !strings.Contains(err.Error(), "ERR_STATE_CORRUPT") {
		t.Fatalf("error must be a state-corruption diagnostic, got %v", err)
	}
}

// TestStateSpecInlineLegacyVerified covers the inline back-compat path: legacy state
// with an inline `spec` (no resolved_spec) is faithful verbatim, so it is VERIFIED and
// Read may treat a missing manifest as genuine absence.
func TestStateSpecInlineLegacyVerified(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	r := &DeploymentResource{}
	state := &deploymentModel{Spec: types.StringValue(wsSpecYAMLHost("1.0.0", "lab-01")),
		SpecFile: types.StringNull(), ResolvedSpec: types.StringNull()}
	got, _, verified, err := r.stateSpec(context.Background(), state)
	if err != nil {
		t.Fatalf("stateSpec inline: %v", err)
	}
	if !verified {
		t.Fatalf("inline legacy spec must be VERIFIED (faithful verbatim)")
	}
	if got.Target.Hosts[0] != "lab-01" {
		t.Fatalf("inline stateSpec host = %v, want lab-01", got.Target.Hosts)
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

// deleteReqForModel builds a real resource.DeleteRequest whose State carries the given
// model, using the resource's own schema, so Delete can be exercised end-to-end.
func deleteReqForModel(t *testing.T, r *DeploymentResource, m *deploymentModel) (resource.DeleteRequest, *resource.DeleteResponse) {
	t.Helper()
	sr := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	st := tfsdk.State{Schema: sr.Schema}
	if diags := st.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("build state: %v", diags)
	}
	return resource.DeleteRequest{State: st}, &resource.DeleteResponse{State: st}
}

// TestDeleteRejectsUnverifiedLegacyState covers item 2 (iter-25 review): Delete must
// refuse to destroy when the identity is unverifiable (legacy spec_file with no
// resolved_spec snapshot), since an immutable file edit could orphan the deployed
// service. It must surface a migration diagnostic instead of calling Destroy.
func TestDeleteRejectsUnverifiedLegacyState(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("1.0.0", "lab-01")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	// Legacy state: spec_file set, NO resolved_spec snapshot ⇒ unverifiable identity.
	m := &deploymentModel{SpecFile: types.StringValue(path), ResolvedSpec: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	req, resp := deleteReqForModel(t, r, m)
	r.Delete(context.Background(), req, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Delete on unverifiable legacy state must surface a migration error, not destroy")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Summary(), "migration required") {
		t.Fatalf("expected migration diagnostic, got %q", resp.Diagnostics.Errors()[0].Summary())
	}
}

// runModifyPlan drives ModifyPlan end-to-end with a real plan+state built from the
// resource schema, returning the response so tests can assert RequiresReplace / errors.
func runModifyPlan(t *testing.T, r *DeploymentResource, plan, state *deploymentModel) *resource.ModifyPlanResponse {
	t.Helper()
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		t.Fatalf("build plan: %v", d)
	}
	st := tfsdk.State{Schema: sr.Schema}
	if d := st.Set(ctx, state); d.HasError() {
		t.Fatalf("build state: %v", d)
	}
	resp := &resource.ModifyPlanResponse{Plan: p}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Plan:   p,
		State:  st,
		Config: tfsdk.Config{Schema: sr.Schema, Raw: p.Raw},
	}, resp)
	return resp
}

// TestModifyPlanBlocksDefaultContributedHostAdoption covers items 1+2 (iter-32 review):
// when unverifiable state (no resolved_spec, matching pre-merge spec_hash) is planned for
// update and the provider default_target now contributes an immutable field (here: the
// host), ModifyPlan must BLOCK with an executable migration error instead of laundering the
// merged identity into the plan — otherwise Update would deploy to the newly-defaulted host
// and orphan the original deployment.
func TestModifyPlanBlocksDefaultContributedHostAdoption(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	// Raw spec_file omits target.hosts; the provider default supplies it.
	if err := os.WriteFile(specPath, []byte(wsSpecYAMLNoHost("1.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	// Provider default_target now points at lab-99 (a DIFFERENT host than was deployed).
	r := &DeploymentResource{pd: &providerData{DefaultTarget: &spec.Target{Hosts: []string{"lab-99"}}}}
	_, hash, contributed, err := r.resolveSpecDetail(context.Background(), &deploymentModel{SpecFile: types.StringValue(specPath)})
	if err != nil {
		t.Fatalf("resolve for baseline hash: %v", err)
	}
	if !contributed {
		t.Fatal("test precondition: provider default_target must contribute the host")
	}
	// Unverifiable state: empty resolved_spec (upgrader declined) but pre-merge hash matches.
	state := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringValue(hash),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	plan := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runModifyPlan(t, r, plan, state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("ModifyPlan must block adoption of a default-contributed identity on unverifiable state")
	}
	if !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "terraform apply") {
		t.Fatalf("block must carry an executable recovery, got %q", resp.Diagnostics.Errors()[0].Detail())
	}
}

// TestPriorFromStateCorruptSnapshotErrors covers item 3 (iter-32 review): a non-empty but
// undecodable resolved_spec must be treated as state corruption (error), consistent with
// stateSpec, rather than silently falling through to inline/spec_file reconstruction and
// feeding a fabricated prior to Update. It must also carry the executable recovery (item 4).
func TestPriorFromStateCorruptSnapshotErrors(t *testing.T) {
	r := &DeploymentResource{}
	state := &deploymentModel{ResolvedSpec: types.StringValue("{ this is not valid json"),
		Spec: types.StringNull(), SpecFile: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	prior, err := r.priorFromState(context.Background(), state)
	if err == nil {
		t.Fatal("corrupt resolved_spec must surface an error, not a fabricated/nil prior")
	}
	if prior != nil {
		t.Fatal("corrupt resolved_spec must not yield a prior deployment")
	}
	if !strings.Contains(err.Error(), "ERR_STATE_CORRUPT") {
		t.Fatalf("expected ERR_STATE_CORRUPT, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "terraform state rm") {
		t.Fatalf("corrupt-state recovery must be executable (terraform state rm), got %q", err.Error())
	}
}

// TestDeleteCorruptSnapshotExecutableRecovery covers item 4 (iter-32 review): Delete on a
// corrupt resolved_spec must not just recommend the dead-end `terraform apply -replace`
// (the destroy half is guarded); it must give the executable `terraform state rm` recovery.
func TestDeleteCorruptSnapshotExecutableRecovery(t *testing.T) {
	r := &DeploymentResource{}
	m := &deploymentModel{ResolvedSpec: types.StringValue("{ corrupt"),
		Spec: types.StringNull(), SpecFile: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	req, resp := deleteReqForModel(t, r, m)
	r.Delete(context.Background(), req, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Delete on corrupt state must surface an error")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(detail, "terraform state rm") {
		t.Fatalf("expected executable recovery (terraform state rm), got %q", detail)
	}
}

// runUpgrade drives the v0→v1 state upgrader against a legacy model and returns the
// upgraded model plus the response diagnostics.
func runUpgrade(t *testing.T, r *DeploymentResource, legacy *deploymentModel) (deploymentModel, *resource.UpgradeStateResponse) {
	t.Helper()
	up := r.UpgradeState(context.Background())
	u, ok := up[0]
	if !ok || u.PriorSchema == nil {
		t.Fatal("expected a v0 state upgrader with a PriorSchema")
	}
	prior := tfsdk.State{Schema: *u.PriorSchema}
	if diags := prior.Set(context.Background(), legacy); diags.HasError() {
		t.Fatalf("build prior state: %v", diags)
	}
	sr := resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	resp := &resource.UpgradeStateResponse{State: tfsdk.State{Schema: sr.Schema}}
	u.StateUpgrader(context.Background(), resource.UpgradeStateRequest{State: &prior}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("upgrade errored: %v", resp.Diagnostics)
	}
	var upgraded deploymentModel
	if diags := resp.State.Get(context.Background(), &upgraded); diags.HasError() {
		t.Fatalf("read upgraded state: %v", diags)
	}
	return upgraded, resp
}

// TestUpgradeStateBackfillsResolvedSpec covers item 1 (iter-28 review): the v0→v1 state
// upgrader reconstructs a resolved_spec snapshot for legacy spec_file state ONLY when the
// current file still resolves to the persisted spec_hash (the source is unchanged since
// deployment), converting an unverifiable identity into a verified one without any unsafe
// deletion — this is what breaks the legacy migration deadlock.
func TestUpgradeStateBackfillsResolvedSpec(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("1.0.0", "lab-01")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	// The persisted spec_hash matches the unchanged file → faithful backfill.
	_, hash, err := r.resolveSpec(context.Background(), &deploymentModel{SpecFile: types.StringValue(path)})
	if err != nil {
		t.Fatalf("resolve for baseline hash: %v", err)
	}
	legacy := &deploymentModel{SpecFile: types.StringValue(path), ResolvedSpec: types.StringNull(),
		SpecHash: types.StringValue(hash),
		Hosts:    types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	upgraded, _ := runUpgrade(t, r, legacy)
	if upgraded.ResolvedSpec.IsNull() || upgraded.ResolvedSpec.ValueString() == "" {
		t.Fatal("upgrader must backfill resolved_spec when the file matches the persisted spec_hash")
	}
	// The backfilled snapshot must now make stateSpec VERIFIED so Read/Delete no longer
	// treat this state as unverifiable — the deadlock is broken.
	_, _, verified, err := r.stateSpec(context.Background(), &upgraded)
	if err != nil {
		t.Fatalf("stateSpec after upgrade: %v", err)
	}
	if !verified {
		t.Fatal("state must be VERIFIED after resolved_spec backfill")
	}
}

// TestUpgradeStateDeclinesOnHashMismatch covers item 2 (iter-28 review): when the current
// spec_file no longer resolves to the persisted spec_hash — i.e. the operator upgraded the
// provider AND edited the spec — the upgrader must NOT snapshot the edited (desired) file
// as the deployed prior. It leaves resolved_spec unset, warns, and keeps the legacy
// replacement safeguard (legacyReplaceRequired) active.
func TestUpgradeStateDeclinesOnHashMismatch(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	// Current on-disk file is the EDITED (desired) config at host lab-99.
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("2.0.0", "lab-99")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	// Persisted spec_hash is from the ORIGINAL deployed config (host lab-01, v1) — differs
	// from the current file's hash.
	stalePersisted := fmt.Sprintf("sha256:%064d", 1)
	legacy := &deploymentModel{SpecFile: types.StringValue(path), ResolvedSpec: types.StringNull(),
		SpecHash: types.StringValue(stalePersisted),
		Hosts:    types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	upgraded, resp := runUpgrade(t, r, legacy)
	if !upgraded.ResolvedSpec.IsNull() && upgraded.ResolvedSpec.ValueString() != "" {
		t.Fatal("hash mismatch must NOT backfill resolved_spec (would fabricate a desired-config prior)")
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("hash mismatch must warn that the snapshot was declined")
	}
	// Safeguard still active: no snapshot ⇒ priorFromState nil ⇒ legacyReplaceRequired fires
	// on real drift, and stateSpec reports UNVERIFIED so Read/Delete stay fail-safe.
	_, _, verified, err := r.stateSpec(context.Background(), &upgraded)
	if err != nil {
		t.Fatalf("stateSpec after declined upgrade: %v", err)
	}
	if verified {
		t.Fatal("state must remain UNVERIFIED when the snapshot was declined")
	}
	declinedPrior, perr := r.priorFromState(context.Background(), &upgraded)
	if perr != nil {
		t.Fatalf("priorFromState errored: %v", perr)
	}
	if !legacyReplaceRequired(declinedPrior, "sha256:different", &upgraded) {
		t.Fatal("legacy replacement safeguard must remain active after a declined backfill")
	}
}

// TestUpgradeStateDeclinesWhenProviderDefaultsContribute covers item 1/2 (iter-30 review):
// spec_hash is computed over the RAW spec, BEFORE provider default_target is merged. If the
// raw spec_file is unchanged (so spec_hash still matches) but the provider's default_target
// now contributes a target field (here: port), the resolved_spec would capture merged fields
// that may never have been deployed. The upgrader must DECLINE the backfill, leave
// resolved_spec unset, warn, and keep the state UNVERIFIED so lifecycle stays truthful.
func TestUpgradeStateDeclinesWhenProviderDefaultsContribute(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	// Unchanged raw spec_file — leaves target.port unset so a provider default can fill it.
	if err := os.WriteFile(path, []byte(wsSpecYAMLHost("1.0.0", "lab-01")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	// Provider now supplies a default_target that contributes port 5986 during the merge.
	r := &DeploymentResource{pd: &providerData{DefaultTarget: &spec.Target{Port: 5986}}}
	// spec_hash is pre-merge, so it matches the unchanged raw file even with the new default.
	_, hash, err := r.resolveSpec(context.Background(), &deploymentModel{SpecFile: types.StringValue(path)})
	if err != nil {
		t.Fatalf("resolve for baseline hash: %v", err)
	}
	legacy := &deploymentModel{SpecFile: types.StringValue(path), ResolvedSpec: types.StringNull(),
		SpecHash: types.StringValue(hash),
		Hosts:    types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	upgraded, resp := runUpgrade(t, r, legacy)
	if !upgraded.ResolvedSpec.IsNull() && upgraded.ResolvedSpec.ValueString() != "" {
		t.Fatal("changed provider default_target must NOT backfill resolved_spec (fields may never have been deployed)")
	}
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("declined default_target backfill must emit a migration warning")
	}
	_, _, verified, err := r.stateSpec(context.Background(), &upgraded)
	if err != nil {
		t.Fatalf("stateSpec after declined upgrade: %v", err)
	}
	if verified {
		t.Fatal("state must remain UNVERIFIED when provider defaults contributed unverifiable fields")
	}
}

// TestUpgradeStateUnresolvableStaysFailSafe covers the edge case where the legacy
// spec_file can no longer be resolved (e.g. removed): the upgrader must NOT error the
// whole refresh; it leaves resolved_spec empty (still fail-safe) and emits a warning.
func TestUpgradeStateUnresolvableStaysFailSafe(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	r := &DeploymentResource{}
	legacy := &deploymentModel{SpecFile: types.StringValue("/nonexistent/spec.yaml"),
		ResolvedSpec: types.StringNull(), Hosts: types.ListNull(types.StringType),
		Variables: types.MapNull(types.StringType)}
	upgraded, resp := runUpgrade(t, r, legacy)
	if resp.Diagnostics.WarningsCount() == 0 {
		t.Fatal("upgrade must warn when it cannot reconstruct the snapshot")
	}
	if !upgraded.ResolvedSpec.IsNull() && upgraded.ResolvedSpec.ValueString() != "" {
		t.Fatal("unresolvable legacy state must keep resolved_spec empty (fail-safe)")
	}
}
