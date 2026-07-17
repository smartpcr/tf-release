package provider

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// testAccProtoV6ProviderFactories serves the provider to the
// terraform-plugin-testing harness over Terraform plugin protocol v6. The
// harness drives a real `terraform` binary that reattaches to this in-process
// protocol-v6 server, exercising the full gRPC handshake and Configure RPC.
//
// The factory key is the bare type name ("labdeploy"); the harness composes the
// full reattach source address from it plus the host/namespace overrides set in
// the test below, so Terraform resolves the provider at the exact served address
// (Address == registry.local/smartpcr/labdeploy).
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"labdeploy": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccProviderConfig declares the provider under its full served source so
// Terraform must resolve and configure it through the protocol-v6 handshake at
// registry.local/smartpcr/labdeploy.
const testAccProviderConfig = `
terraform {
  required_providers {
    labdeploy = {
      source = "registry.local/smartpcr/labdeploy"
    }
  }
}

provider "labdeploy" {}
`

// TestAccProviderServedProtocolV6 is the implementation-plan §1.1 harness proof:
// the provider is served through the terraform-plugin-testing gRPC/subprocess
// harness (a real `terraform` binary reattaches to the plugin), resolved by the
// registry.local/smartpcr/labdeploy source address that main.go advertises, and
// negotiated over Terraform plugin protocol v6. It self-skips unless TF_ACC=1
// and a terraform binary are available (resource.Test enforces this), so it
// never affects the plain `go test` gate.
func TestAccProviderServedProtocolV6(t *testing.T) {
	host, namespace, name := splitAddress(t, Address)
	if name != "labdeploy" {
		t.Fatalf("Address type name = %q, want %q", name, "labdeploy")
	}

	// Point the harness's reattach address at the exact served source so the
	// config's required_providers source resolves to this in-process server.
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A provider-only config yields a no-op plan; running it still
				// forces Terraform to complete the v6 handshake and call the
				// provider's Configure RPC at the advertised address.
				Config:   testAccProviderConfig,
				PlanOnly: true,
			},
		},
	})
}

// splitAddress splits "host/namespace/name" into its three parts.
func splitAddress(t *testing.T, addr string) (host, namespace, name string) {
	t.Helper()
	parts := strings.Split(addr, "/")
	if len(parts) != 3 {
		t.Fatalf("Address %q is not host/namespace/name", addr)
	}
	return parts[0], parts[1], parts[2]
}
