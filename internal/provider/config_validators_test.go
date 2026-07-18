package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// configForModel builds a real tfsdk.Config from the resource's own schema so the
// exactly-one-of ConfigValidator can be exercised exactly as the framework invokes it
// during `terraform validate` — before any spec parse or transport dial.
func configForModel(t *testing.T, m *deploymentModel) tfsdk.Config {
	t.Helper()
	sr := resource.SchemaResponse{}
	(&DeploymentResource{}).Schema(context.Background(), resource.SchemaRequest{}, &sr)
	// Zero-value types.Map/types.List carry a dynamic element type the schema rejects;
	// give the collection attributes typed nulls so only spec/spec_file drive the case.
	m.Variables = types.MapNull(types.StringType)
	m.Hosts = types.ListNull(types.StringType)
	st := tfsdk.State{Schema: sr.Schema}
	if diags := st.Set(context.Background(), m); diags.HasError() {
		t.Fatalf("build config: %v", diags)
	}
	return tfsdk.Config{Schema: sr.Schema, Raw: st.Raw}
}

// TestExactlyOneOfSpecValidator proves Scenario "One-of validation" (T9): when both
// spec and spec_file are set — and separately when neither is set — the schema-level
// ConfigValidator reports an "exactly one of" error during validation, rather than
// deferring the check to the runtime resolveSpec fallback at apply time. Exactly one
// set is accepted, and an unknown (interpolated) value is deferred without error.
func TestExactlyOneOfSpecValidator(t *testing.T) {
	cases := []struct {
		name     string
		model    *deploymentModel
		wantErrs bool
	}{
		{
			name:     "both set",
			model:    &deploymentModel{Spec: types.StringValue("kind: Deployment"), SpecFile: types.StringValue("/x/spec.yaml")},
			wantErrs: true,
		},
		{
			name:     "neither set (both null)",
			model:    &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringNull()},
			wantErrs: true,
		},
		{
			name:     "neither set (empty strings)",
			model:    &deploymentModel{Spec: types.StringValue(""), SpecFile: types.StringValue("")},
			wantErrs: true,
		},
		{
			name:     "only spec",
			model:    &deploymentModel{Spec: types.StringValue("kind: Deployment"), SpecFile: types.StringNull()},
			wantErrs: false,
		},
		{
			name:     "only spec_file",
			model:    &deploymentModel{Spec: types.StringNull(), SpecFile: types.StringValue("/x/spec.yaml")},
			wantErrs: false,
		},
		{
			name:     "unknown spec deferred",
			model:    &deploymentModel{Spec: types.StringUnknown(), SpecFile: types.StringNull()},
			wantErrs: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := resource.ValidateConfigRequest{Config: configForModel(t, tc.model)}
			resp := &resource.ValidateConfigResponse{}
			exactlyOneOfSpecValidator{}.ValidateResource(context.Background(), req, resp)

			gotErrs := resp.Diagnostics.HasError()
			if gotErrs != tc.wantErrs {
				t.Fatalf("HasError()=%v want %v; diags=%v", gotErrs, tc.wantErrs, resp.Diagnostics)
			}
			if tc.wantErrs {
				summary := resp.Diagnostics.Errors()[0].Summary()
				if !strings.Contains(summary, "exactly one of") {
					t.Fatalf("error summary %q does not mention 'exactly one of'", summary)
				}
			}
		})
	}
}

// TestExactlyOneOfSpecValidatorWiredIntoResource guards the wiring: the resource must
// advertise the exactly-one-of validator through ConfigValidators so the framework runs
// it during validate. If a refactor drops it from the slice, config-time enforcement of
// VAL-06 silently regresses to the runtime-only resolveSpec check.
func TestExactlyOneOfSpecValidatorWiredIntoResource(t *testing.T) {
	vs := (&DeploymentResource{}).ConfigValidators(context.Background())
	found := false
	for _, v := range vs {
		if _, ok := v.(exactlyOneOfSpecValidator); ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("ConfigValidators() must include exactlyOneOfSpecValidator; got %#v", vs)
	}
}

// TestDeploymentSchemaRoundTrip proves Scenario "Schema round-trip": a fully-populated
// deployment model written into state through the resource schema reads back with every
// attribute intact — no arguments or computed attributes are dropped or mutated.
func TestDeploymentSchemaRoundTrip(t *testing.T) {
	ctx := context.Background()
	sr := resource.SchemaResponse{}
	(&DeploymentResource{}).Schema(ctx, resource.SchemaRequest{}, &sr)

	original := &deploymentModel{
		ID:              types.StringValue("dep-123"),
		Spec:            types.StringValue("kind: Deployment"),
		SpecFile:        types.StringNull(),
		Variables:       types.MapValueMust(types.StringType, map[string]attr.Value{"ENV": types.StringValue("prod")}),
		VersionOverride: types.StringValue("2.0.0"),
		DestroyMode:     types.StringValue("purge"),
		SpecHash:        types.StringValue("abc123"),
		ResolvedSpec:    types.StringValue("kind: Deployment\nresolved: true"),
		DeployedVersion: types.StringValue("2.0.0"),
		PreviousVersion: types.StringValue("1.0.0"),
		Hosts:           types.ListValueMust(types.StringType, []attr.Value{types.StringValue("lab-01"), types.StringValue("lab-02")}),
		ReleasePath:     types.StringValue(`C:\releases\dep-123`),
		ServiceStatus:   types.StringValue("running"),
	}

	st := tfsdk.State{Schema: sr.Schema}
	if diags := st.Set(ctx, original); diags.HasError() {
		t.Fatalf("write state: %v", diags)
	}

	var got deploymentModel
	if diags := st.Get(ctx, &got); diags.HasError() {
		t.Fatalf("read state: %v", diags)
	}

	if !reflect.DeepEqual(*original, got) {
		t.Fatalf("round-trip lost attributes:\n original=%#v\n got=%#v", *original, got)
	}
}
