package provider

import (
	"context"
	"strings"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// TestProviderMetadataTypeName asserts the provider advertises the type name
// that pairs with the registry address (registry.local/smartpcr/labdeploy).
func TestProviderMetadataTypeName(t *testing.T) {
	p := New("test")()
	resp := &fwprovider.MetadataResponse{}
	p.Metadata(context.Background(), fwprovider.MetadataRequest{}, resp)
	if resp.TypeName != "labdeploy" {
		t.Fatalf("Metadata TypeName = %q, want %q", resp.TypeName, "labdeploy")
	}
	if resp.Version != "test" {
		t.Fatalf("Metadata Version = %q, want %q", resp.Version, "test")
	}
}

// TestProviderAddress pins the registry source address advertised by main.go so
// the served address and the documented filesystem-mirror layout stay in sync
// (DESIGN §16.1).
func TestProviderAddress(t *testing.T) {
	if Address != "registry.local/smartpcr/labdeploy" {
		t.Fatalf("Address = %q, want %q", Address, "registry.local/smartpcr/labdeploy")
	}
}

// TestProviderServesProtocolV6 proves the provider is served over Terraform
// plugin protocol v6: providerserver.NewProtocol6WithError yields a
// tfprotov6.ProviderServer (the protocol-v6 gRPC surface) and a live
// GetProviderSchema RPC succeeds.
func TestProviderServesProtocolV6(t *testing.T) {
	factory := providerserver.NewProtocol6WithError(New("test")())
	server, err := factory()
	if err != nil {
		t.Fatalf("NewProtocol6WithError factory returned error: %v", err)
	}

	// Compile-time + runtime proof the server implements protocol v6: the
	// factory's return type is tfprotov6.ProviderServer, and GetProviderSchema
	// is a protocol-v6 RPC.
	assertProtocolV6(server)

	schemaResp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatalf("GetProviderSchema returned error: %v", err)
	}
	if schemaResp == nil || schemaResp.Provider == nil {
		t.Fatalf("GetProviderSchema returned no provider schema")
	}
	if _, ok := schemaResp.ResourceSchemas["labdeploy_deployment"]; !ok {
		t.Fatalf("provider schema missing labdeploy_deployment resource; got %v", keys(schemaResp.ResourceSchemas))
	}
}

// assertProtocolV6 accepts only a tfprotov6.ProviderServer, giving a
// compile-time proof that the served provider speaks Terraform plugin protocol v6.
func assertProtocolV6(_ tfprotov6.ProviderServer) {}

func keys(m map[string]*tfprotov6.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// deploymentConfig builds a tfsdk.Config from the DeploymentResource schema,
// setting spec/spec_file to the given values (nil ⇒ null) and every other
// attribute to null. This lets tests drive the framework config validators the
// same way Terraform does during plan/validate.
func deploymentConfig(t *testing.T, specVal, specFileVal *string) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	r := &DeploymentResource{}
	sresp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, sresp)
	sc := sresp.Schema

	objType := sc.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, ty := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(ty, nil) // null
	}
	if specVal != nil {
		vals["spec"] = tftypes.NewValue(tftypes.String, *specVal)
	}
	if specFileVal != nil {
		vals["spec_file"] = tftypes.NewValue(tftypes.String, *specFileVal)
	}
	return tfsdk.Config{Schema: sc, Raw: tftypes.NewValue(objType, vals)}
}

// TestVAL06ExactlyOneOfSpec covers DESIGN §14 VAL-06 at the Terraform SCHEMA
// layer: the resource's ConfigValidators reject configs where both `spec` and
// `spec_file` are set (or neither), and accept exactly one — before any parse
// or dial. Exercised through the framework's ValidateResource contract.
func TestVAL06ExactlyOneOfSpec(t *testing.T) {
	ctx := context.Background()
	r := &DeploymentResource{}
	validators := r.ConfigValidators(ctx)
	if len(validators) == 0 {
		t.Fatal("VAL-06: DeploymentResource declares no ConfigValidators")
	}

	run := func(cfg tfsdk.Config) *resource.ValidateConfigResponse {
		resp := &resource.ValidateConfigResponse{}
		for _, v := range validators {
			v.ValidateResource(ctx, resource.ValidateConfigRequest{Config: cfg}, resp)
		}
		return resp
	}

	inline := "apiVersion: labdeploy/v1"
	file := "/tmp/spec.yaml"

	// Both set ⇒ error naming ERR_SPEC_INVALID + exactly-one-of.
	resp := run(deploymentConfig(t, &inline, &file))
	if !resp.Diagnostics.HasError() {
		t.Fatal("VAL-06: both spec and spec_file set must produce a config error")
	}
	msg := resp.Diagnostics.Errors()[0].Summary() + " " + resp.Diagnostics.Errors()[0].Detail()
	for _, frag := range []string{"ERR_SPEC_INVALID", "exactly one of"} {
		if !strings.Contains(msg, frag) {
			t.Fatalf("VAL-06 diagnostic missing %q: got %q", frag, msg)
		}
	}

	// Neither set ⇒ error.
	if !run(deploymentConfig(t, nil, nil)).Diagnostics.HasError() {
		t.Fatal("VAL-06: neither spec nor spec_file set must produce a config error")
	}

	// Exactly one set ⇒ no error (both orderings).
	if run(deploymentConfig(t, &inline, nil)).Diagnostics.HasError() {
		t.Fatalf("VAL-06: inline spec alone must be valid: %v", run(deploymentConfig(t, &inline, nil)).Diagnostics)
	}
	if run(deploymentConfig(t, nil, &file)).Diagnostics.HasError() {
		t.Fatalf("VAL-06: spec_file alone must be valid: %v", run(deploymentConfig(t, nil, &file)).Diagnostics)
	}
}
