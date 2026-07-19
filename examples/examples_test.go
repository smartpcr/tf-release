// Package examples holds golden tests that exercise the committed example
// artifacts (Stage 8.2). They read examples/specs/*.yaml and
// examples/pipelines/*.yml straight from disk and assert that:
//
//   - every deployment/test spec parses and validates through the real spec
//     validator (merge-then-validate, mirroring how the provider applies the
//     example provider `default_target` from main.tf); and
//   - both pipeline templates are well-formed YAML and retain the
//     always()/condition: always() publish steps that guarantee test results
//     and logs are uploaded even on a failed rollout.
//
// The tests keep the committed examples honest: if a future edit drifts a
// spec out of contract or breaks a pipeline template, `go test ./examples/...`
// fails.
package examples

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"gopkg.in/yaml.v3"
)

// validChecksum is a syntactically valid sha256:<64 hex> value used to resolve
// the ${var:CHECKSUM} token so checksum validation passes.
const validChecksum = "sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// exampleVars resolves every ${var:*} token referenced by the committed specs.
func exampleVars() map[string]string {
	return map[string]string{
		"HOST":     "lab-host-01.contoso.lab",
		"CHECKSUM": validChecksum,
		"FEED_URL": "https://pkgs.contoso.lab/_packaging/lab/nuget/v3/index.json",
		"URL":      "https://artifacts.contoso.lab/sample/1.0.0/app.zip",
		"VERSION":  "1.0.0",
	}
}

// defaultTarget mirrors the provider `default_target` block in examples/main.tf.
// The winrm specs deliberately omit credentials and inherit them here, so the
// golden test must reproduce the provider's merge-then-validate flow (DESIGN
// §6.2) rather than validate the raw file in isolation.
func defaultTarget() *spec.Target {
	return &spec.Target{
		Transport: spec.TransportWinRM,
		OS:        spec.OSWindows,
		Credentials: spec.Credentials{
			Username:    `LAB\deploy-svc`,
			PasswordEnv: "LABDEPLOY_PASSWORD",
		},
	}
}

// setExampleEnv sets every env var NAME the specs reference so requireEnv
// (VAL-08) passes without a live connection.
func setExampleEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"LABDEPLOY_PASSWORD",
		"LABDEPLOY_SSH_KEY",
		"NUGET_PAT",
		"REGISTRY_PASSWORD",
	} {
		t.Setenv(name, "example-secret")
	}
}

func readSpec(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("specs", name))
	if err != nil {
		t.Fatalf("read spec %s: %v", name, err)
	}
	return string(raw)
}

// TestDeploymentSpecsValidate proves every committed Deployment spec parses and
// validates (after merging the example provider default_target).
func TestDeploymentSpecsValidate(t *testing.T) {
	setExampleEnv(t)
	deployments := []string{
		"cluster-service.yaml",
		"console-app.yaml",
		"docker-container.yaml",
		"dotnet-api.yaml",
		"node-web-app.yaml",
		"windows-service.yaml",
	}
	for _, name := range deployments {
		name := name
		t.Run(name, func(t *testing.T) {
			raw := readSpec(t, name)
			d, hash, err := spec.ParseDeploymentLenient(raw, exampleVars(), "")
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			if hash == "" {
				t.Fatalf("%s: empty spec_hash", name)
			}
			spec.MergeTargetDefaults(&d.Target, defaultTarget())
			if err := spec.ValidateDeployment(d); err != nil {
				t.Fatalf("validate %s: %v", name, err)
			}
		})
	}
}

// TestTestRunSpecValidates proves the committed e2e TestRun spec parses and
// validates (after merging the example provider default_target).
func TestTestRunSpecValidates(t *testing.T) {
	setExampleEnv(t)
	raw := readSpec(t, "e2e-testrun.yaml")
	tr, hash, err := spec.ParseTestRunLenient(raw, exampleVars())
	if err != nil {
		t.Fatalf("parse e2e-testrun.yaml: %v", err)
	}
	if hash == "" {
		t.Fatal("e2e-testrun.yaml: empty spec_hash")
	}
	spec.MergeTargetDefaults(&tr.Target, defaultTarget())
	if err := spec.ValidateTestRun(tr); err != nil {
		t.Fatalf("validate e2e-testrun.yaml: %v", err)
	}
}

// TestPipelineTemplatesWellFormed proves both committed pipeline templates are
// well-formed YAML and keep the always()/condition: always() publish steps that
// guarantee results and logs upload even when the rollout fails.
func TestPipelineTemplatesWellFormed(t *testing.T) {
	cases := []struct {
		file       string
		mustContain string
	}{
		{filepath.Join("pipelines", "github-deploy.yml"), "if: always()"},
		{filepath.Join("pipelines", "azure-pipelines.yml"), "condition: always()"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.file, func(t *testing.T) {
			raw, err := os.ReadFile(c.file)
			if err != nil {
				t.Fatalf("read %s: %v", c.file, err)
			}
			var doc interface{}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("%s is not well-formed YAML: %v", c.file, err)
			}
			if doc == nil {
				t.Fatalf("%s decoded to an empty document", c.file)
			}
			if !strings.Contains(string(raw), c.mustContain) {
				t.Fatalf("%s: missing required publish guard %q", c.file, c.mustContain)
			}
		})
	}
}
