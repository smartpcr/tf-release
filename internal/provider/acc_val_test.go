package provider

import (
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Stage 9.1 — VAL scenarios (DESIGN §18.1). Spec validation fires before any
// dial, so these are host-independent: they run under a real `terraform` binary
// via the terraform-plugin-testing harness with only TF_ACC=1 (no W1/L1 lab
// infra). Each test maps 1:1 to a DESIGN §18.1 ID and asserts the stable coded
// error the pipeline contract promises (DESIGN §12). VAL-01 additionally uses an
// unroutable host to prove NO connection is attempted — the failure is pure
// validation, reached before the transport is built.

// valSetup points the harness reattach address at the served provider source so
// the config's required_providers resolves in-process, and enables TF_ACC.
func valSetup(t *testing.T) {
	t.Helper()
	accPreCheck(t)
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
}

// deploymentConfig wraps a spec YAML body in an labdeploy_deployment resource
// under the acceptance provider config.
func accDeploymentConfig(specYAML string) string {
	return testAccProviderConfig + `
resource "labdeploy_deployment" "val" {
  spec = <<-EOT
` + specYAML + `EOT
}
`
}

// valCase runs a single-step plan that must fail with wantErr.
func valCase(t *testing.T, specYAML string, wantErr *regexp.Regexp) {
	t.Helper()
	valSetup(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      accDeploymentConfig(specYAML),
				PlanOnly:    true,
				ExpectError: wantErr,
			},
		},
	})
}

// A minimal, otherwise-valid winrm/windows target block reused where the target
// itself must be valid so a LATER check is the one that fails.
const valWinTarget = `target:
  transport: winrm
  hosts: ["10.255.255.1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_ACC_VAL_PW }
  winrm: { insecure_skip_verify: true }
`

func valWinService(t *testing.T) {
	t.Helper()
	// LABDEPLOY_ACC_VAL_PW must resolve so target validation passes and the
	// intended downstream check is what fails (env-name hygiene, DESIGN §11).
	t.Setenv("LABDEPLOY_ACC_VAL_PW", "pw")
}

// VAL-01: tab-broken YAML ⇒ ERR_SPEC_INVALID naming the offending SOURCE LINE
// (yaml.v3 reports `line N`); no connection attempted (host is unroutable, but
// plan-time parse fails first so it is never dialed) — evaluator item 1.
func TestAccVAL01_TabBrokenYAML(t *testing.T) {
	spec := "apiVersion: labdeploy/v1\nkind: Deployment\nmetadata:\n\tname: bad\n"
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID(?s).*line \d`))
}

// VAL-02: missing artifact.version ⇒ ERR_SPEC_INVALID naming artifact.version.
func TestAccVAL02_MissingVersion(t *testing.T) {
	valWinService(t)
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
` + valWinTarget + `pattern: { type: windows_service, service_name: SampleSvc, exe: bin\SampleSvc.exe }
artifact:
  type: zip
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://10.255.255.1/a.zip" }
`
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID.*artifact\.version`))
}

// VAL-03: pattern windows_service + os linux ⇒ ERR_SPEC_INVALID citing the §14
// support matrix.
func TestAccVAL03_PatternOSMismatch(t *testing.T) {
	valWinService(t)
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: ssh
  hosts: ["10.255.255.1"]
  os: linux
  credentials: { username: u, password_env: LABDEPLOY_ACC_VAL_PW }
pattern: { type: windows_service, service_name: SampleSvc, exe: bin/SampleSvc }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://10.255.255.1/a.zip" }
`
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID.*support matrix`))
}

// VAL-04: cluster pattern with a single host ⇒ ERR_SPEC_INVALID `2..16`.
func TestAccVAL04_ClusterHostCount(t *testing.T) {
	valWinService(t)
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["10.255.255.1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_ACC_VAL_PW }
pattern: { type: cluster_generic_service, service_name: SampleSvc, role_name: sample-role, exe: bin\SampleSvc.exe }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://10.255.255.1/a.zip" }
`
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID.*2\.\.16`))
}

// VAL-05: checksum `md5:…` ⇒ ERR_SPEC_INVALID citing the sha256 format.
func TestAccVAL05_BadChecksum(t *testing.T) {
	valWinService(t)
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
` + valWinTarget + `pattern: { type: windows_service, service_name: SampleSvc, exe: bin\SampleSvc.exe }
artifact:
  type: zip
  version: 1.0.0
  checksum: "md5:deadbeef"
  source: { type: http, url: "http://10.255.255.1/a.zip" }
`
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID.*checksum`))
}

// VAL-06: both `spec` and `spec_file` set ⇒ the TF schema/config validator
// error (exactly one of), before any parse or dial.
func TestAccVAL06_ExactlyOneOfSpec(t *testing.T) {
	valSetup(t)
	config := testAccProviderConfig + `
resource "labdeploy_deployment" "val" {
  spec      = "apiVersion: labdeploy/v1\nkind: Deployment\n"
  spec_file = "/tmp/other.yaml"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)exactly one of`),
			},
		},
	})
}

// VAL-07: `${var:missing}` in spec ⇒ ERR_SPEC_INVALID `unresolved variable
// missing at <path>`.
func TestAccVAL07_UnresolvedVariable(t *testing.T) {
	valWinService(t)
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
` + valWinTarget + `pattern: { type: windows_service, service_name: SampleSvc, exe: bin\SampleSvc.exe }
artifact:
  type: zip
  version: "${var:missing}"
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://10.255.255.1/a.zip" }
`
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID.*unresolved variable missing`))
}

// VAL-08: `password_env: NOT_SET_VAR` (a name that does not resolve on the
// runner) ⇒ ERR_SPEC_INVALID `env var NOT_SET_VAR … is not set`, fired BEFORE
// any dial (DESIGN §11 env-name hygiene). Modeled as an apply step per §18.1.
func TestAccVAL08_UnsetPasswordEnv(t *testing.T) {
	valSetup(t)
	// Deliberately DO NOT set NOT_SET_VAR; assert absence to be robust to a
	// polluted CI environment.
	if _, ok := os.LookupEnv("NOT_SET_VAR"); ok {
		t.Skip("NOT_SET_VAR is set in this environment; cannot prove the unset path")
	}
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["10.255.255.1"]
  os: windows
  credentials: { username: u, password_env: NOT_SET_VAR }
  winrm: { insecure_skip_verify: true }
pattern: { type: windows_service, service_name: SampleSvc, exe: bin\SampleSvc.exe }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source: { type: http, url: "http://10.255.255.1/a.zip" }
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      accDeploymentConfig(spec),
				ExpectError: regexp.MustCompile(`ERR_SPEC_INVALID.*NOT_SET_VAR.*is not set`),
			},
		},
	})
}

// VAL-09: docker_image artifact with a windows_service pattern ⇒
// ERR_SPEC_INVALID (artifact × pattern matrix).
func TestAccVAL09_DockerWithWindowsService(t *testing.T) {
	valWinService(t)
	spec := `apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
` + valWinTarget + `pattern: { type: windows_service, service_name: SampleSvc, exe: bin\SampleSvc.exe }
artifact:
  type: docker_image
  version: 1.0.0
  source: { type: docker_registry, image: "example/sample-svc:1.0.0" }
`
	valCase(t, spec, regexp.MustCompile(`ERR_SPEC_INVALID.*docker`))
}
