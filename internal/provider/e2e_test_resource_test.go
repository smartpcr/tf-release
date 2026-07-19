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
