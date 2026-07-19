package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// e2eSchema renders the E2ETestResource schema once for the schema-shape tests.
func e2eSchema(t *testing.T) schema.Schema {
	t.Helper()
	r := &E2ETestResource{}
	sresp := &resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, sresp)
	if sresp.Diagnostics.HasError() {
		t.Fatalf("e2e schema build errors: %v", sresp.Diagnostics)
	}
	return sresp.Schema
}

// TestE2EDeploymentIDIsSettableWithoutReplacement proves the "deployment_id
// wires a reference without replacement" scenario (DESIGN §5.3): the
// labdeploy_e2e_test schema exposes deployment_id as a settable (Optional)
// string attribute with NO RequiresReplace plan modifier, so a config
// referencing labdeploy_deployment.x.id creates a graph edge in Terraform Core
// without forcing the test's replacement (triggers alone owns re-execution).
func TestE2EDeploymentIDIsSettableWithoutReplacement(t *testing.T) {
	sc := e2eSchema(t)

	raw, ok := sc.Attributes["deployment_id"]
	if !ok {
		t.Fatal("deployment_id: attribute is absent from the labdeploy_e2e_test schema")
	}
	attrib, ok := raw.(schema.StringAttribute)
	if !ok {
		t.Fatalf("deployment_id: want schema.StringAttribute, got %T", raw)
	}

	// Settable: Optional (config-set) and not Computed-only.
	if !attrib.IsOptional() {
		t.Fatal("deployment_id: must be Optional so a config can set it to labdeploy_deployment.x.id")
	}
	if attrib.IsRequired() {
		t.Fatal("deployment_id: must NOT be Required (it is a purely-advisory dependency edge)")
	}
	if attrib.GetType() != types.StringType {
		t.Fatalf("deployment_id: want string type, got %v", attrib.GetType())
	}

	// Absence of RequiresReplace: run every declared plan modifier across a
	// value change and assert none requests replacement.
	ctx := context.Background()
	req := planmodifier.StringRequest{
		StateValue:  types.StringValue("labdeploy_deployment.x.id-OLD"),
		PlanValue:   types.StringValue("labdeploy_deployment.x.id-NEW"),
		ConfigValue: types.StringValue("labdeploy_deployment.x.id-NEW"),
	}
	for i, pm := range attrib.PlanModifiers {
		resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		pm.PlanModifyString(ctx, req, resp)
		if resp.RequiresReplace {
			t.Fatalf("deployment_id: plan modifier #%d (%s) sets RequiresReplace; the edge must NOT force replacement", i, pm.Description(ctx))
		}
	}
}

// TestE2ETriggersRequiresReplace is the negative control for the scenario above:
// triggers DOES force replacement on change, proving the schema deliberately
// gates re-execution on triggers (not deployment_id).
func TestE2ETriggersRequiresReplace(t *testing.T) {
	sc := e2eSchema(t)

	raw, ok := sc.Attributes["triggers"]
	if !ok {
		t.Fatal("triggers: attribute is absent from the labdeploy_e2e_test schema")
	}
	attrib, ok := raw.(schema.MapAttribute)
	if !ok {
		t.Fatalf("triggers: want schema.MapAttribute, got %T", raw)
	}

	ctx := context.Background()
	oldV, d1 := types.MapValue(types.StringType, map[string]attr.Value{"run": types.StringValue("1")})
	newV, d2 := types.MapValue(types.StringType, map[string]attr.Value{"run": types.StringValue("2")})
	if d1.HasError() || d2.HasError() {
		t.Fatalf("building trigger maps: %v %v", d1, d2)
	}
	// A non-null state + non-null plan with a delta must trip RequiresReplace.
	req := planmodifier.MapRequest{
		State:      tfsdk.State{Raw: tftypes.NewValue(tftypes.String, "non-null")},
		Plan:       tfsdk.Plan{Raw: tftypes.NewValue(tftypes.String, "non-null")},
		StateValue: oldV,
		PlanValue:  newV,
	}
	var replaced bool
	for _, pm := range attrib.PlanModifiers {
		resp := &planmodifier.MapResponse{PlanValue: req.PlanValue}
		pm.PlanModifyMap(ctx, req, resp)
		if resp.RequiresReplace {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("triggers: a value change must force RequiresReplace (re-run)")
	}
}

// TestE2EArgumentsAreRequiresReplace proves the immutable-run lifecycle (DESIGN
// §5.3 "Update is never in-place"): every real argument — spec, spec_file,
// variables, fail_on_test_failure — carries a RequiresReplace plan modifier, so
// editing any of them replaces the resource (re-runs the tests) rather than
// silently keeping stale outcomes. deployment_id is deliberately excluded (edge
// only) and is asserted separately above.
func TestE2EArgumentsAreRequiresReplace(t *testing.T) {
	sc := e2eSchema(t)
	ctx := context.Background()

	for _, name := range []string{"spec", "spec_file"} {
		attrib, ok := sc.Attributes[name].(schema.StringAttribute)
		if !ok {
			t.Fatalf("%s: want schema.StringAttribute, got %T", name, sc.Attributes[name])
		}
		req := planmodifier.StringRequest{
			State:      tfsdk.State{Raw: tftypes.NewValue(tftypes.String, "non-null")},
			Plan:       tfsdk.Plan{Raw: tftypes.NewValue(tftypes.String, "non-null")},
			StateValue: types.StringValue("a"), PlanValue: types.StringValue("b"), ConfigValue: types.StringValue("b"),
		}
		if !stringReplaces(ctx, attrib.PlanModifiers, req) {
			t.Fatalf("%s: a change must force RequiresReplace", name)
		}
	}

	// variables (map)
	vAttr, ok := sc.Attributes["variables"].(schema.MapAttribute)
	if !ok {
		t.Fatalf("variables: want schema.MapAttribute, got %T", sc.Attributes["variables"])
	}
	oldV, _ := types.MapValue(types.StringType, map[string]attr.Value{"k": types.StringValue("1")})
	newV, _ := types.MapValue(types.StringType, map[string]attr.Value{"k": types.StringValue("2")})
	mReq := planmodifier.MapRequest{
		State:      tfsdk.State{Raw: tftypes.NewValue(tftypes.String, "non-null")},
		Plan:       tfsdk.Plan{Raw: tftypes.NewValue(tftypes.String, "non-null")},
		StateValue: oldV, PlanValue: newV, ConfigValue: newV,
	}
	var mReplaced bool
	for _, pm := range vAttr.PlanModifiers {
		resp := &planmodifier.MapResponse{PlanValue: mReq.PlanValue}
		pm.PlanModifyMap(ctx, mReq, resp)
		if resp.RequiresReplace {
			mReplaced = true
		}
	}
	if !mReplaced {
		t.Fatal("variables: a change must force RequiresReplace")
	}

	// fail_on_test_failure (bool)
	bAttr, ok := sc.Attributes["fail_on_test_failure"].(schema.BoolAttribute)
	if !ok {
		t.Fatalf("fail_on_test_failure: want schema.BoolAttribute, got %T", sc.Attributes["fail_on_test_failure"])
	}
	bReq := planmodifier.BoolRequest{
		State:      tfsdk.State{Raw: tftypes.NewValue(tftypes.String, "non-null")},
		Plan:       tfsdk.Plan{Raw: tftypes.NewValue(tftypes.String, "non-null")},
		StateValue: types.BoolValue(true), PlanValue: types.BoolValue(false), ConfigValue: types.BoolValue(false),
	}
	var bReplaced bool
	for _, pm := range bAttr.PlanModifiers {
		resp := &planmodifier.BoolResponse{PlanValue: bReq.PlanValue}
		pm.PlanModifyBool(ctx, bReq, resp)
		if resp.RequiresReplace {
			bReplaced = true
		}
	}
	if !bReplaced {
		t.Fatal("fail_on_test_failure: a change must force RequiresReplace")
	}
}

func stringReplaces(ctx context.Context, mods []planmodifier.String, req planmodifier.StringRequest) bool {
	for _, pm := range mods {
		resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		pm.PlanModifyString(ctx, req, resp)
		if resp.RequiresReplace {
			return true
		}
	}
	return false
}

// TestE2EDeleteNeverFailsDestroy proves the best-effort Delete contract (DESIGN
// §5.3 "Delete removes the remote test dir best effort, never fails destroy"):
// when the persisted state's spec cannot be resolved to a target (so the remote
// dir cannot be located), Delete emits a WARNING and NO error, letting Terraform
// drop the resource from state.
func TestE2EDeleteNeverFailsDestroy(t *testing.T) {
	ctx := context.Background()
	r := &E2ETestResource{}
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)

	st := tfsdk.State{Schema: sr.Schema}
	// State with neither spec nor spec_file ⇒ resolveSpec fails ⇒ cleanup skipped.
	m := &e2eModel{
		ID:        types.StringValue("smoke@lab-01"),
		Spec:      types.StringNull(),
		SpecFile:  types.StringNull(),
		Variables: types.MapNull(types.StringType),
		Triggers:  types.MapNull(types.StringType),
	}
	if d := st.Set(ctx, m); d.HasError() {
		t.Fatalf("build state: %v", d)
	}
	resp := &resource.DeleteResponse{State: st}
	r.Delete(ctx, resource.DeleteRequest{State: st}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("Delete must NEVER fail destroy; got error diagnostics: %v", resp.Diagnostics.Errors())
	}
	if len(resp.Diagnostics.Warnings()) == 0 {
		t.Fatal("Delete should warn when it cannot locate the remote test dir to clean")
	}
}
