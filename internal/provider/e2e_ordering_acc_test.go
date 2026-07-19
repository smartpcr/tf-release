package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// e2eOrderingConfig wires an labdeploy_e2e_test whose deployment_id references
// labdeploy_deployment.dep.id. Because dep is created in the same apply, its id is
// "known after apply" (unknown) at plan time; a reference to it therefore makes
// the test's deployment_id unknown too — the observable fingerprint of the
// Terraform Core dependency edge that orders the e2e_test create strictly AFTER
// the deployment create (DESIGN §5.3; implementation-plan.md:404 scenario).
func e2eOrderingConfig() string {
	deploymentSpec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
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
`, 0)

	e2eSpec := `apiVersion: labdeploy/v1
kind: TestRun
metadata: { name: smoke }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://example.test/a.zip" }
runner:
  type: exec
  command: ./run-tests.sh
  timeout_seconds: 600
results: { format: none }
pass_criteria: { exit_codes: [0] }
`

	return testAccProviderConfig + fmt.Sprintf(`
resource "labdeploy_deployment" "dep" {
  spec = <<-EOT
%sEOT
}

resource "labdeploy_e2e_test" "smoke" {
  deployment_id = labdeploy_deployment.dep.id
  spec = <<-EOT
%sEOT
}
`, deploymentSpec, e2eSpec)
}

// TestAccE2EDeploymentIDOrdersAfterDeployment is the plan-harness portion of the
// implementation-plan.md:404 "deployment_id ordering under apply" proof. It runs
// the BUILT provider under a real `terraform` binary via the terraform-plugin-
// testing harness (proto v6 reattach) and plans a config where
// `labdeploy_e2e_test.smoke.deployment_id = labdeploy_deployment.dep.id`.
//
// A PLAN is sufficient (and infra-free) to prove the Terraform Core dependency
// EDGE: if — and only if — Core built the edge from the reference, the
// deployment's not-yet-known id propagates into the test's deployment_id, making
// it unknown ("known after apply"). The plan check asserts exactly that, which
// proves Core will order the e2e_test create after the deployment create.
//
// DEFERRED (operator decision, iter-4): observing the real Create SIDE-EFFECT
// sequence under a *completed* apply is NOT asserted here because the provider's
// Create dials the target host (engine.New() at the plugin-server boundary has no
// transport seam), so a green apply needs a live lab target. That full apply-
// ordering proof is deferred to a dedicated integration workstream with real lab
// infra. The apply attempt below therefore fails infra-free at the transport and
// is tolerated via ExpectError; the EDGE (not the side-effect sequence) is the
// claim this test proves. It self-skips unless TF_ACC=1 and a terraform binary
// are available, so it never affects the plain `go test` gate.
func TestAccE2EDeploymentIDOrdersAfterDeployment(t *testing.T) {
	host, namespace, name := splitAddress(t, Address)
	if name != "labdeploy" {
		t.Fatalf("Address type name = %q, want %q", name, "labdeploy")
	}
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: e2eOrderingConfig(),
				// The dependency edge is proven by PreApply plan checks, which run
				// against the real Terraform plan BEFORE any apply. The apply then
				// runs the deployment create FIRST (the proven ordering) and fails
				// at the transport layer because no lab target exists in CI — an
				// expected, infra-free outcome we tolerate via ExpectError so the
				// edge proof does not depend on a live host.
				ExpectError: regexp.MustCompile(`(?s)ERR_|connect|dial|lookup|no such host|timed out|target`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// The dependency edge's fingerprint: the test's
						// deployment_id is unknown because it references the
						// deployment's known-after-apply id.
						plancheck.ExpectUnknownValue(
							"labdeploy_e2e_test.smoke",
							tfjsonpath.New("deployment_id"),
						),
						// Sanity: both objects are scheduled for creation.
						plancheck.ExpectResourceAction("labdeploy_deployment.dep", plancheck.ResourceActionCreate),
						plancheck.ExpectResourceAction("labdeploy_e2e_test.smoke", plancheck.ResourceActionCreate),
					},
				},
			},
		},
	})
}
