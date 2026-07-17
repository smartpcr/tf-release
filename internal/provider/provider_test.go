package provider

import (
	"context"
	"strings"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
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

// TestVAL06ExactlyOneOfSpec covers DESIGN §14 VAL-06: setting both `spec` and
// `spec_file` is rejected with an ERR_SPEC_INVALID "exactly one of" error,
// before any spec parse or dial.
func TestVAL06ExactlyOneOfSpec(t *testing.T) {
	r := &DeploymentResource{}
	m := &deploymentModel{
		Spec:     types.StringValue("apiVersion: labdeploy/v1"),
		SpecFile: types.StringValue("/tmp/spec.yaml"),
	}
	_, _, err := r.resolveSpec(context.Background(), m)
	if err == nil {
		t.Fatal("VAL-06: expected error when both spec and spec_file are set")
	}
	for _, frag := range []string{"ERR_SPEC_INVALID", "exactly one of"} {
		if !strings.Contains(err.Error(), frag) {
			t.Fatalf("VAL-06 error missing %q: got %v", frag, err)
		}
	}
}
