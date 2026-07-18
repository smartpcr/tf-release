package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// nullTimeouts returns a null timeouts.Value of the deployment resource's
// timeouts block type (create/update/delete). Test models built as struct
// literals leave Timeouts as its zero value (an untyped, empty object) which
// fails tfsdk.Set against the schema's typed timeouts block; normalizing to a
// typed null keeps the create/update/delete defaults in force.
func nullTimeouts() timeouts.Value {
	return timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{
		"create": types.StringType,
		"update": types.StringType,
		"delete": types.StringType,
	})}
}

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
	if !strings.Contains(err.Error(), "ERR_SPEC_INVALID") {
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
	m.Timeouts = nullTimeouts()
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
// A nil state builds a create-shaped request (State.Raw null), so tests can exercise the
// post-`state rm` re-adopt path that flows through Create.
func runModifyPlan(t *testing.T, r *DeploymentResource, plan, state *deploymentModel) *resource.ModifyPlanResponse {
	t.Helper()
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	p := tfsdk.Plan{Schema: sr.Schema}
	plan.Timeouts = nullTimeouts()
	if d := p.Set(ctx, plan); d.HasError() {
		t.Fatalf("build plan: %v", d)
	}
	st := tfsdk.State{Schema: sr.Schema} // nil state ⇒ Raw stays null ⇒ create path
	if state != nil {
		state.Timeouts = nullTimeouts()
		if d := st.Set(ctx, state); d.HasError() {
			t.Fatalf("build state: %v", d)
		}
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
	detail := resp.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(detail, "terraform state rm") || !strings.Contains(detail, "terraform apply") {
		t.Fatalf("block must carry the executable state-rm + apply recovery, got %q", detail)
	}
}

// TestModifyPlanRecoveryCreateProceeds covers items 1+2 (iter-33 review): the documented
// recovery for unverifiable default-dependent state is `terraform state rm` + `terraform
// apply`. After state rm there is NO prior state, so the apply flows through Create. This
// test proves that create-path planning is NOT blocked (unlike the update path) and that it
// carries a resolved_spec snapshot for Create to persist — i.e. the recovery can actually
// proceed through planning rather than being rejected by the replacement/Delete guard.
func TestModifyPlanRecoveryCreateProceeds(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(wsSpecYAMLNoHost("1.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	// Same default-contributes-host setup that BLOCKS on update...
	r := &DeploymentResource{pd: &providerData{DefaultTarget: &spec.Target{Hosts: []string{"lab-99"}}}}
	plan := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	// ...but with NO prior state (post `terraform state rm`), planning is the create path.
	resp := runModifyPlan(t, r, plan, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("recovery create-path planning must NOT be blocked, got %v", resp.Diagnostics.Errors())
	}
	// The plan must carry a resolved_spec so Create persists a VERIFIED snapshot on apply,
	// reaching verified state without a replacement or the guarded Delete.
	var planned deploymentModel
	if d := resp.Plan.Get(context.Background(), &planned); d.HasError() {
		t.Fatalf("read planned model: %v", d)
	}
	if planned.ResolvedSpec.IsNull() || planned.ResolvedSpec.ValueString() == "" {
		t.Fatal("recovery create plan must carry a resolved_spec for Create to persist a verified snapshot")
	}
}

// fakeDeployEngine injects at the resource's engine boundary (DESIGN §17) so a
// full lifecycle path can be driven without a live transport. It records the spec
// it was asked to deploy (so a test can prove the RESOLVED, default-merged identity
// — not the pre-merge spec — is what gets deployed) and returns a canned success.
type fakeDeployEngine struct {
	gotSpec *spec.Deployment
	status  *engine.Status
}

func (f *fakeDeployEngine) Update(_ context.Context, s, _ *spec.Deployment) (*engine.Status, error) {
	f.gotSpec = s
	return f.status, nil
}
func (f *fakeDeployEngine) ReadStatus(_ context.Context, _ *spec.Deployment) (*engine.Status, error) {
	return f.status, nil
}
func (f *fakeDeployEngine) Destroy(_ context.Context, _ *spec.Deployment, _ string) error { return nil }
func (f *fakeDeployEngine) Warns() []string                                               { return nil }

// runCreate drives DeploymentResource.Create end-to-end with a real plan built from
// the resource schema and returns the response so tests can assert the persisted state.
func runCreate(t *testing.T, r *DeploymentResource, plan *deploymentModel) *resource.CreateResponse {
	t.Helper()
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	p := tfsdk.Plan{Schema: sr.Schema}
	plan.Timeouts = nullTimeouts()
	if d := p.Set(ctx, plan); d.HasError() {
		t.Fatalf("build plan: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sr.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: p}, resp)
	return resp
}

// TestCreateRecoveryPersistsResolvedSpec is the end-to-end continuation of the
// documented `terraform state rm` + `terraform apply` recovery for unverifiable
// default-dependent state (iter-34 review item 1). Where TestModifyPlanRecoveryCreateProceeds
// only proves create-path PLANNING is unblocked and carries a resolved_spec, this test
// drives DeploymentResource.Create itself through the recovery and proves:
//  1. the engine deploys the RESOLVED, default-merged identity (host lab-99) — the intended
//     deployed identity — not the pre-merge spec that omits the host, and
//  2. the resulting state persists a DECODABLE resolved_spec snapshot for that same identity,
//     i.e. Create actually reaches verified state (not merely assumed).
func TestCreateRecoveryPersistsResolvedSpec(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	// Raw spec_file omits target.hosts; the provider default_target supplies lab-99 — the
	// same default-contributes-host setup that BLOCKS on the update path.
	if err := os.WriteFile(specPath, []byte(wsSpecYAMLNoHost("1.0.0")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	fake := &fakeDeployEngine{status: &engine.Status{
		DeployedVersion: "1.0.0", ServiceStatus: "running", Hosts: []string{"lab-99"},
	}}
	r := &DeploymentResource{
		pd:        &providerData{DefaultTarget: &spec.Target{Hosts: []string{"lab-99"}}},
		newEngine: func() deployEngine { return fake },
	}
	// Post `terraform state rm`, the apply flows through Create with no prior state.
	plan := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runCreate(t, r, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("recovery Create must succeed, got %v", resp.Diagnostics.Errors())
	}
	// 1. The engine was asked to deploy the resolved default-merged identity, not the
	//    pre-merge spec (which has no host).
	if fake.gotSpec == nil {
		t.Fatal("Create must invoke the engine deploy")
	}
	if len(fake.gotSpec.Target.Hosts) != 1 || fake.gotSpec.Target.Hosts[0] != "lab-99" {
		t.Fatalf("Create must deploy the resolved identity lab-99, got hosts %v", fake.gotSpec.Target.Hosts)
	}
	// 2. The persisted state carries a DECODABLE resolved_spec snapshot for that identity —
	//    proving Create reaches VERIFIED state rather than merely assuming adoption.
	var persisted deploymentModel
	if d := resp.State.Get(context.Background(), &persisted); d.HasError() {
		t.Fatalf("read persisted state: %v", d)
	}
	rs := persisted.ResolvedSpec.ValueString()
	if rs == "" {
		t.Fatal("recovery Create must persist a resolved_spec snapshot")
	}
	var decoded spec.Deployment
	if err := json.Unmarshal([]byte(rs), &decoded); err != nil {
		t.Fatalf("persisted resolved_spec must be decodable, got error: %v (raw=%q)", err, rs)
	}
	if len(decoded.Target.Hosts) != 1 || decoded.Target.Hosts[0] != "lab-99" {
		t.Fatalf("persisted resolved_spec must capture the deployed identity lab-99, got %v", decoded.Target.Hosts)
	}
	// A verified snapshot is exactly what stateSpec treats as trustworthy, so a subsequent
	// Read/Delete would no longer hit the unverifiable-legacy migration guard.
	if _, _, verified, err := r.stateSpec(context.Background(), &persisted); err != nil || !verified {
		t.Fatalf("post-recovery state must be verified, got verified=%v err=%v", verified, err)
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
	if !strings.Contains(err.Error(), "ERR_SPEC_INVALID") {
		t.Fatalf("expected ERR_SPEC_INVALID, got %q", err.Error())
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
	legacy.Timeouts = nullTimeouts()
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

// clusterSpecYAMLHosts is a valid multi-host cluster_generic_service deployment,
// used to exercise target.hosts set semantics (reorder vs add/remove) at the
// plan layer. The single-host wsSpecYAML forms cannot carry 2 hosts (validation
// requires exactly one for windows_service), so the plan-level host tests use
// this cluster form.
func clusterSpecYAMLHosts(hosts ...string) string {
	hl := `["` + strings.Join(hosts, `", "`) + `"]`
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: %s
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: cluster_generic_service
  service_name: SampleSvc
  role_name: SampleRole
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, hl, 0)
}

// wsSpecServiceYAML is wsSpecYAML with a configurable pattern.service_name, for
// driving a service_name (immutable) change through ModifyPlan.
func wsSpecServiceYAML(service, host string) string {
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
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: windows_service
  service_name: %s
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, host, 0, service)
}

// dotnetSpecYAML is a valid dotnet_api single-host windows spec sharing the same
// metadata.name/service_name/host as wsSpecYAML, so switching between them isolates
// pattern.type as the changed immutable field.
func dotnetSpecYAML(host string) string {
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
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: dotnet_api
  service_name: SampleSvc
  launcher: exe
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, host, 0)
}

// consoleSpecYAML is a console_app spec parametrized by target.os (console_app is one
// of the few patterns valid on both windows and linux), for driving a target.os
// (immutable) change through ModifyPlan with no other field varying.
func consoleSpecYAML(os, host string) string {
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["%s"]
  os: %s
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:%064d"
  source: { type: http, url: "http://example.test/a.zip" }
pattern:
  type: console_app
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, host, os, 0)
}

func wsDeployment(ptype spec.PatternType, service, role, name, os string, hosts ...string) *spec.Deployment {
	d := &spec.Deployment{
		Metadata: spec.Metadata{Name: name},
		Target:   spec.Target{Hosts: hosts, OS: spec.OSKind(os)},
	}
	d.Pattern.Type = ptype
	d.Pattern.ServiceName = service
	d.Pattern.RoleName = role
	return d
}

// TestImmutableKeyHostReorderEqual proves the "host reorder is not a replace"
// scenario at the immutableKey layer: [a,b] and [b,a] compare equal because hosts
// are compared as a SET (lower-cased + sorted), per architecture.md line 350.
func TestImmutableKeyHostReorderEqual(t *testing.T) {
	ab := wsDeployment(spec.PatternClusterGeneric, "Svc", "Role", "app", "windows", "lab-a", "lab-b")
	ba := wsDeployment(spec.PatternClusterGeneric, "Svc", "Role", "app", "windows", "lab-b", "lab-a")
	if immutableKey(ab) != immutableKey(ba) {
		t.Fatalf("host reorder must NOT change immutableKey:\n [a,b]=%q\n [b,a]=%q", immutableKey(ab), immutableKey(ba))
	}
	// Case-insensitive set: LAB-A vs lab-a must not differ either.
	mixed := wsDeployment(spec.PatternClusterGeneric, "Svc", "Role", "app", "windows", "LAB-B", "LAB-A")
	if immutableKey(ab) != immutableKey(mixed) {
		t.Fatalf("host case/order must not change immutableKey: %q vs %q", immutableKey(ab), immutableKey(mixed))
	}
}

// TestImmutableKeyImmutablePathsDiffer proves the T9 scenario at the immutableKey
// layer: a change to pattern.type, service_name, target.hosts (set membership), or
// target.os each yields a different key (so ModifyPlan appends RequiresReplace).
func TestImmutableKeyImmutablePathsDiffer(t *testing.T) {
	base := wsDeployment(spec.PatternWindowsService, "Svc", "", "app", "windows", "lab-a")
	cases := map[string]*spec.Deployment{
		"pattern.type":  wsDeployment(spec.PatternClusterGeneric, "Svc", "Role", "app", "windows", "lab-a", "lab-b"),
		"service_name":  wsDeployment(spec.PatternWindowsService, "Other", "", "app", "windows", "lab-a"),
		"target.hosts":  wsDeployment(spec.PatternWindowsService, "Svc", "", "app", "windows", "lab-z"),
		"metadata.name": wsDeployment(spec.PatternWindowsService, "Svc", "", "other", "windows", "lab-a"),
		"target.os":     wsDeployment(spec.PatternWindowsService, "Svc", "", "app", "linux", "lab-a"),
	}
	for name, changed := range cases {
		if immutableKey(base) == immutableKey(changed) {
			t.Fatalf("changing %s must change immutableKey, but it stayed %q", name, immutableKey(base))
		}
	}
}

// TestDeploymentIDDeterministicSHA1 proves the "deterministic SHA-1 id" scenario:
// id == sha1(sorted(hosts)+"/"+name)[0:12] + ":" + name, and is byte-identical
// when the same hosts are supplied in a different order (architecture.md line 78).
func TestDeploymentIDDeterministicSHA1(t *testing.T) {
	d1 := wsDeployment(spec.PatternClusterGeneric, "Svc", "Role", "n", "windows", "b", "a")
	d2 := wsDeployment(spec.PatternClusterGeneric, "Svc", "Role", "n", "windows", "a", "b")
	got := deploymentID(d1)

	sum := sha1.Sum([]byte("a,b/n"))
	want := hex.EncodeToString(sum[:])[:12] + ":n"
	if got != want {
		t.Fatalf("deploymentID = %q, want authoritative formula %q", got, want)
	}
	if deploymentID(d1) != deploymentID(d2) {
		t.Fatalf("deploymentID must be host-order-independent: %q vs %q", deploymentID(d1), deploymentID(d2))
	}
	if len(strings.SplitN(got, ":", 2)[0]) != 12 {
		t.Fatalf("deploymentID hash prefix must be 12 chars, got %q", got)
	}
	if !strings.HasSuffix(got, ":n") {
		t.Fatalf("deploymentID must end with :<name>, got %q", got)
	}
}

// TestDiagSummaryCoded is the unit-level contract for the Summary formatter and the
// DESIGN §8.1-vs-§12 timeout taxonomy. Classification keys on the OPERATION CONTEXT:
//   - outer resource context expired            → ERR_TIMEOUT (TF/runner timeout)
//   - outer context LIVE + coded engine error   → the engine's taxonomy code
//   - outer context LIVE + nested transport      → ERR_CONNECT even though the error's
//     Unwrap chain carries context.DeadlineExceeded (a dial/WinRM child deadline)
//   - non-coded pre-flight error                → the operation fallback code
//
// Every result is the exact "[<CODE>] <short>" shape.
func TestDiagSummaryCoded(t *testing.T) {
	liveCtx := context.Background()
	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	if expiredCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("test setup: expiredCtx must be DeadlineExceeded, got %v", expiredCtx.Err())
	}
	cases := []struct {
		name     string
		ctx      context.Context
		fallback string
		err      error
		want     string
	}{
		{"coded", liveCtx, "ERR_SPEC_INVALID", &engine.CodedError{Code: "ERR_CONNECT", Err: fmt.Errorf("x")}, "[ERR_CONNECT] deploy failed"},
		{"fallback", liveCtx, "ERR_SPEC_INVALID", fmt.Errorf("plain"), "[ERR_SPEC_INVALID] deploy failed"},
		// Live outer context + nested transport/dial deadline: STAYS ERR_CONNECT.
		{"live-ctx-nested-transport-deadline", liveCtx, "ERR_CONNECT",
			&engine.CodedError{Code: "ERR_CONNECT", Err: fmt.Errorf("dial: %w", context.DeadlineExceeded)},
			"[ERR_CONNECT] deploy failed"},
		{"live-ctx-bare-deadline", liveCtx, "ERR_CONNECT", context.DeadlineExceeded, "[ERR_CONNECT] deploy failed"},
		// Expired outer resource context ⇒ ERR_TIMEOUT regardless of the wrapped code.
		{"expired-ctx-timeout", expiredCtx, "ERR_CONNECT",
			&engine.CodedError{Code: "ERR_CONNECT", Err: fmt.Errorf("d: %w", context.DeadlineExceeded)},
			"[ERR_TIMEOUT] deploy failed"},
		{"expired-ctx-plain-err", expiredCtx, "ERR_CONNECT", fmt.Errorf("plain"), "[ERR_TIMEOUT] deploy failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diagSummary(tc.ctx, tc.fallback, "deploy failed", tc.err); got != tc.want {
				t.Fatalf("diagSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTimeoutDefaults proves the "timeout defaults" scenario: the schema exposes a
// timeouts block, and with no explicit timeouts configured create/update default to
// 30m and delete to 15m (DESIGN §5.2).
func TestTimeoutDefaults(t *testing.T) {
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	(&DeploymentResource{}).Schema(ctx, resource.SchemaRequest{}, &sr)
	if _, ok := sr.Schema.Blocks["timeouts"]; !ok {
		t.Fatal("schema must declare a timeouts block")
	}
	if defaultCreateTimeout != 30*time.Minute || defaultUpdateTimeout != 30*time.Minute || defaultDeleteTimeout != 15*time.Minute {
		t.Fatalf("timeout defaults wrong: create=%s update=%s delete=%s", defaultCreateTimeout, defaultUpdateTimeout, defaultDeleteTimeout)
	}
	// A null (unconfigured) timeouts value resolves to the operation defaults.
	to := nullTimeouts()
	if got, d := to.Create(ctx, defaultCreateTimeout); d.HasError() || got != 30*time.Minute {
		t.Fatalf("create default = %s (diags %v), want 30m", got, d)
	}
	if got, d := to.Update(ctx, defaultUpdateTimeout); d.HasError() || got != 30*time.Minute {
		t.Fatalf("update default = %s (diags %v), want 30m", got, d)
	}
	if got, d := to.Delete(ctx, defaultDeleteTimeout); d.HasError() || got != 15*time.Minute {
		t.Fatalf("delete default = %s (diags %v), want 15m", got, d)
	}
}

// TestImportUnsupported proves the "import unsupported" scenario: ImportState emits
// the pinned message "import is not supported; adopt via apply" (DESIGN §5.2).
func TestImportUnsupported(t *testing.T) {
	r := &DeploymentResource{}
	resp := &resource.ImportStateResponse{}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "anything"}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("ImportState must surface an error")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); got != "import is not supported; adopt via apply" {
		t.Fatalf("import error message = %q, want the pinned %q", got, "import is not supported; adopt via apply")
	}
}

// TestModifyPlanHostReorderNoReplace proves the "host reorder is not a replace"
// scenario end-to-end through ModifyPlan: a prior state with hosts [lab-01,lab-02]
// and a new plan with [lab-02,lab-01] and no other change produces NO RequiresReplace.
func TestModifyPlanHostReorderNoReplace(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	// New plan: hosts reordered.
	if err := os.WriteFile(specPath, []byte(clusterSpecYAMLHosts("lab-02", "lab-01")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	// Prior persisted snapshot: hosts in the original order.
	priorSpec, priorHash, err := r.resolveSpec(context.Background(),
		&deploymentModel{Spec: types.StringValue(clusterSpecYAMLHosts("lab-01", "lab-02")), SpecFile: types.StringNull(),
			Variables: types.MapNull(types.StringType)})
	if err != nil {
		t.Fatalf("resolve prior: %v", err)
	}
	plan := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	state := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: marshalResolvedSpec(priorSpec), SpecHash: types.StringValue(priorHash),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runModifyPlan(t, r, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("reorder plan must not error: %v", resp.Diagnostics.Errors())
	}
	if len(resp.RequiresReplace) != 0 {
		t.Fatalf("host reorder must NOT force replacement, got RequiresReplace=%v", resp.RequiresReplace)
	}
}

// TestModifyPlanImmutableHostChangeReplaces proves the T9 scenario end-to-end: a
// plan changing target.hosts set membership (lab-02 → lab-03) forces RequiresReplace.
func TestModifyPlanImmutableHostChangeReplaces(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(specPath, []byte(clusterSpecYAMLHosts("lab-01", "lab-03")), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	r := &DeploymentResource{}
	priorSpec, priorHash, err := r.resolveSpec(context.Background(),
		&deploymentModel{Spec: types.StringValue(clusterSpecYAMLHosts("lab-01", "lab-02")), SpecFile: types.StringNull(),
			Variables: types.MapNull(types.StringType)})
	if err != nil {
		t.Fatalf("resolve prior: %v", err)
	}
	plan := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	state := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
		ResolvedSpec: marshalResolvedSpec(priorSpec), SpecHash: types.StringValue(priorHash),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runModifyPlan(t, r, plan, state)
	if resp.Diagnostics.HasError() {
		t.Fatalf("immutable-change plan must not error: %v", resp.Diagnostics.Errors())
	}
	if len(resp.RequiresReplace) == 0 {
		t.Fatal("changing target.hosts set membership must force RequiresReplace")
	}
}

// TestCreateSurfacesCodedDeployError proves the coded-diagnostic contract on the
// Create path itself: when the engine deploy fails with an ERR_CONNECT CodedError,
// the Create diagnostic Summary begins "[ERR_CONNECT] " (DESIGN §12).
func TestCreateSurfacesCodedDeployError(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	fake := &fakeErrEngine{err: &engine.CodedError{Code: "ERR_CONNECT", Host: "lab-01",
		Step: "PREFLIGHT", Err: fmt.Errorf("dial tcp: connection refused")}}
	r := &DeploymentResource{newEngine: func() deployEngine { return fake }}
	plan := &deploymentModel{Spec: types.StringValue(wsSpecYAML("1.0.0")), SpecFile: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runCreate(t, r, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create must surface the engine deploy failure")
	}
	sum := resp.Diagnostics.Errors()[0].Summary()
	if !strings.HasPrefix(sum, "[ERR_CONNECT] ") {
		t.Fatalf("deploy failure Summary must begin with the coded prefix, got %q", sum)
	}
}

// TestCreateNestedTransportDeadlineIsConnect proves the DESIGN §8.1 side of the
// timeout taxonomy end-to-end: when the OUTER resource operation context is still
// live but the engine surfaces a nested dial/transport child deadline (an ERR_CONNECT
// CodedError whose Unwrap chain carries context.DeadlineExceeded), Create must keep
// the ERR_CONNECT code — the child deadline is a connectivity failure, not a TF timeout.
func TestCreateNestedTransportDeadlineIsConnect(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	// Engine surfaces the deadline the way wrapTransportErr does: an ERR_CONNECT
	// CodedError whose Unwrap chain still carries context.DeadlineExceeded. runCreate's
	// outer context uses the default 30m create timeout, so it is LIVE here.
	fake := &fakeErrEngine{err: &engine.CodedError{Code: "ERR_CONNECT", Host: "lab-01",
		Step: "PREFLIGHT", Err: fmt.Errorf("dial: %w", context.DeadlineExceeded)}}
	r := &DeploymentResource{newEngine: func() deployEngine { return fake }}
	plan := &deploymentModel{Spec: types.StringValue(wsSpecYAML("1.0.0")), SpecFile: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runCreate(t, r, plan)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create must surface the deploy failure")
	}
	sum := resp.Diagnostics.Errors()[0].Summary()
	if !strings.HasPrefix(sum, "[ERR_CONNECT] ") {
		t.Fatalf("nested transport deadline under a live resource context must stay [ERR_CONNECT], got %q", sum)
	}
}

// TestCreateResourceDeadlineIsTimeout proves the DESIGN §12 side of the taxonomy
// end-to-end: when the OUTER Terraform resource-operation deadline (the create
// timeout block) actually expires, Create maps the failure to ERR_TIMEOUT. The
// engine blocks until the operation context is Done, so the classifier observes the
// expired resource context (not merely a DeadlineExceeded buried in the error).
func TestCreateResourceDeadlineIsTimeout(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	r := &DeploymentResource{newEngine: func() deployEngine { return &blockingEngine{} }}
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	p := tfsdk.Plan{Schema: sr.Schema}
	plan := &deploymentModel{Spec: types.StringValue(wsSpecYAML("1.0.0")), SpecFile: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType),
		Timeouts: shortTimeouts(t, "1ms")}
	if d := p.Set(ctx, plan); d.HasError() {
		t.Fatalf("build plan: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sr.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: p}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create must surface the resource-timeout failure")
	}
	sum := resp.Diagnostics.Errors()[0].Summary()
	if !strings.HasPrefix(sum, "[ERR_TIMEOUT] ") {
		t.Fatalf("expired resource context must map to [ERR_TIMEOUT], got %q", sum)
	}
}

// shortTimeouts builds a timeouts.Value whose create attribute is the given duration
// string (e.g. "1ms"), so a test can drive an operation context that expires promptly.
func shortTimeouts(t *testing.T, create string) timeouts.Value {
	t.Helper()
	obj, d := types.ObjectValue(
		map[string]attr.Type{"create": types.StringType, "update": types.StringType, "delete": types.StringType},
		map[string]attr.Value{"create": types.StringValue(create), "update": types.StringNull(), "delete": types.StringNull()},
	)
	if d.HasError() {
		t.Fatalf("build timeouts: %v", d)
	}
	return timeouts.Value{Object: obj}
}

// blockingEngine blocks each engine call until the operation context is Done, then
// returns that context's error — modelling a real transport whose in-flight operation
// is aborted when the outer resource deadline fires.
type blockingEngine struct{}

func (*blockingEngine) Update(ctx context.Context, _, _ *spec.Deployment) (*engine.Status, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("deploy aborted: %w", ctx.Err())
}
func (*blockingEngine) ReadStatus(ctx context.Context, _ *spec.Deployment) (*engine.Status, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("read aborted: %w", ctx.Err())
}
func (*blockingEngine) Destroy(ctx context.Context, _ *spec.Deployment, _ string) error {
	<-ctx.Done()
	return fmt.Errorf("destroy aborted: %w", ctx.Err())
}
func (*blockingEngine) Warns() []string { return nil }

// TestModifyPlanImmutablePathsRequireReplace is the T9 plan-level proof that a change
// to EACH immutable path — pattern.type, service_name, and target.os (target.hosts is
// covered by TestModifyPlanImmutableHostChangeReplaces) — forces RequiresReplace when
// driven end-to-end through ModifyPlan, not merely compared via immutableKey. Each case
// varies exactly one immutable field against an otherwise-identical prior snapshot.
func TestModifyPlanImmutablePathsRequireReplace(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	r := &DeploymentResource{}
	// resolvePrior builds a persisted resolved_spec snapshot the way an apply would.
	resolvePrior := func(t *testing.T, yaml string) types.String {
		t.Helper()
		ps, _, err := r.resolveSpec(context.Background(),
			&deploymentModel{Spec: types.StringValue(yaml), SpecFile: types.StringNull(),
				Variables: types.MapNull(types.StringType)})
		if err != nil {
			t.Fatalf("resolve prior: %v", err)
		}
		return marshalResolvedSpec(ps)
	}
	// console_app resolved specs cannot round-trip marshalResolvedSpec (the flat Pattern's
	// non-pointer Account struct always marshals as `"account":{}`, which the concrete
	// console_app schema rejects on strict re-decode) — a spec-package union limitation
	// unrelated to this stage. So the target.os case supplies a hand-built decodable prior
	// snapshot carrying only console_app-legal fields, at os=windows.
	consolePriorWindows := types.StringValue(
		`{"metadata":{"name":"sample-svc"},"target":{"transport":"winrm","hosts":["lab-01"],` +
			`"os":"windows","credentials":{"username":"u","password_env":"LABDEPLOY_PASSWORD"}},` +
			`"pattern":{"type":"console_app","exe":"bin\\SampleSvc.exe"}}`)
	cases := []struct {
		name    string
		prior   types.String
		newYAML string
	}{
		{"pattern.type", resolvePrior(t, wsSpecYAMLHost("1.0.0", "lab-01")), dotnetSpecYAML("lab-01")},
		{"service_name", resolvePrior(t, wsSpecServiceYAML("SampleSvc", "lab-01")), wsSpecServiceYAML("OtherSvc", "lab-01")},
		{"target.os", consolePriorWindows, consoleSpecYAML("linux", "lab-01")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			specPath := filepath.Join(dir, "spec.yaml")
			if err := os.WriteFile(specPath, []byte(tc.newYAML), 0o600); err != nil {
				t.Fatalf("write spec: %v", err)
			}
			plan := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
				ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
				Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
			state := &deploymentModel{SpecFile: types.StringValue(specPath), Spec: types.StringNull(),
				ResolvedSpec: tc.prior, SpecHash: types.StringValue("prior-hash"),
				Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
			resp := runModifyPlan(t, r, plan, state)
			if resp.Diagnostics.HasError() {
				t.Fatalf("%s change plan must not error: %v", tc.name, resp.Diagnostics.Errors())
			}
			if len(resp.RequiresReplace) == 0 {
				t.Fatalf("changing immutable path %s must force RequiresReplace", tc.name)
			}
		})
	}
}

// TestCreatePersistsAllComputedOutputs proves the Stage 5.2 computed-output contract:
// every computed attribute is populated in persisted state FROM the engine result —
// deployed_version, previous_version, hosts, release_path, service_status, spec_hash,
// name, and the deterministic id — asserted together off one fully-populated status.
func TestCreatePersistsAllComputedOutputs(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	fake := &fakeDeployEngine{status: &engine.Status{
		DeployedVersion: "2.1.0",
		PreviousVersion: "2.0.0",
		ReleasePath:     `C:\labdeploy\releases\2.1.0`,
		ServiceStatus:   "running",
		Hosts:           []string{"lab-01"},
	}}
	r := &DeploymentResource{newEngine: func() deployEngine { return fake }}
	plan := &deploymentModel{Spec: types.StringValue(wsSpecYAML("2.1.0")), SpecFile: types.StringNull(),
		ResolvedSpec: types.StringNull(), SpecHash: types.StringNull(),
		Hosts: types.ListNull(types.StringType), Variables: types.MapNull(types.StringType)}
	resp := runCreate(t, r, plan)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create must succeed, got %v", resp.Diagnostics.Errors())
	}
	var st deploymentModel
	if d := resp.State.Get(context.Background(), &st); d.HasError() {
		t.Fatalf("read persisted state: %v", d)
	}
	if got := st.DeployedVersion.ValueString(); got != "2.1.0" {
		t.Errorf("deployed_version = %q, want 2.1.0", got)
	}
	if got := st.PreviousVersion.ValueString(); got != "2.0.0" {
		t.Errorf("previous_version = %q, want 2.0.0", got)
	}
	if got := st.ReleasePath.ValueString(); got != `C:\labdeploy\releases\2.1.0` {
		t.Errorf("release_path = %q, want release dir", got)
	}
	if got := st.ServiceStatus.ValueString(); got != "running" {
		t.Errorf("service_status = %q, want running", got)
	}
	if st.SpecHash.ValueString() == "" {
		t.Error("spec_hash must be populated")
	}
	if got := st.Name.ValueString(); got != "sample-svc" {
		t.Errorf("name = %q, want sample-svc", got)
	}
	var hosts []string
	if d := st.Hosts.ElementsAs(context.Background(), &hosts, false); d.HasError() {
		t.Fatalf("read hosts: %v", d)
	}
	if len(hosts) != 1 || hosts[0] != "lab-01" {
		t.Errorf("hosts = %v, want [lab-01]", hosts)
	}
	// id must equal the authoritative deterministic formula sha1(sorted(hosts)+"/"+name)[0:12]+":"+name.
	wantID := deploymentID(&spec.Deployment{
		Metadata: spec.Metadata{Name: "sample-svc"},
		Target:   spec.Target{Hosts: []string{"lab-01"}},
	})
	if got := st.ID.ValueString(); got != wantID {
		t.Errorf("id = %q, want %q", got, wantID)
	}
}

// fakeErrEngine is a deployEngine whose Update fails with a fixed coded error, so
// the resource's coded-diagnostic mapping can be exercised without a live transport.
type fakeErrEngine struct{ err error }

func (f *fakeErrEngine) Update(_ context.Context, _, _ *spec.Deployment) (*engine.Status, error) {
	return nil, f.err
}
func (f *fakeErrEngine) ReadStatus(_ context.Context, _ *spec.Deployment) (*engine.Status, error) {
	return nil, f.err
}
func (f *fakeErrEngine) Destroy(_ context.Context, _ *spec.Deployment, _ string) error { return f.err }
func (f *fakeErrEngine) Warns() []string                                               { return nil }
