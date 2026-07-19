// Package examples holds golden tests that exercise the committed example
// artifacts (Stage 8.2). They read examples/specs/*.yaml and
// examples/pipelines/*.yml straight from disk and assert that:
//
//   - EVERY committed deployment/test spec (discovered from disk, not a
//     hard-coded list) parses and validates through the real spec validator
//     (merge-then-validate, mirroring how the provider applies the example
//     provider `default_target` from main.tf); and
//   - both pipeline templates are well-formed YAML, carry the required
//     job/stage topology, and place the always()/condition: always() guards on
//     the actual upload-artifact / publish steps (not merely somewhere in the
//     file text) so results and logs upload even when the rollout fails.
//
// The tests keep the committed examples honest: a newly added spec fixture is
// automatically covered, and renaming/removing a pipeline job/stage or dropping
// a publish guard fails `go test ./examples/...`.
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

// discoverSpecs returns every committed examples/specs/*.yaml path. Discovery
// (rather than a hard-coded list) guarantees a newly added fixture cannot
// bypass the "each committed spec validates" acceptance gate.
func discoverSpecs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("specs")
	if err != nil {
		t.Fatalf("read specs dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			files = append(files, filepath.Join("specs", n))
		}
	}
	if len(files) == 0 {
		t.Fatal("no committed specs/*.yaml fixtures found")
	}
	return files
}

// kindProbe extracts the discriminator so each discovered spec is routed to the
// matching validator.
type kindProbe struct {
	Kind string `yaml:"kind"`
}

// TestExampleSpecsValidate proves EVERY committed spec fixture (discovered from
// disk) parses and validates after merging the example provider default_target.
// It routes each file to the Deployment or TestRun validator by its kind, and
// asserts both kinds are represented so the e2e TestRun stays covered.
func TestExampleSpecsValidate(t *testing.T) {
	setExampleEnv(t)
	var sawDeployment, sawTestRun bool
	for _, path := range discoverSpecs(t) {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			var probe kindProbe
			if err := yaml.Unmarshal(raw, &probe); err != nil {
				t.Fatalf("%s: not well-formed YAML: %v", path, err)
			}
			switch probe.Kind {
			case "Deployment":
				sawDeployment = true
				d, hash, err := spec.ParseDeploymentLenient(string(raw), exampleVars(), "")
				if err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				if hash == "" {
					t.Fatalf("%s: empty spec_hash", path)
				}
				spec.MergeTargetDefaults(&d.Target, defaultTarget())
				if err := spec.ValidateDeployment(d); err != nil {
					t.Fatalf("validate %s: %v", path, err)
				}
			case "TestRun":
				sawTestRun = true
				tr, hash, err := spec.ParseTestRunLenient(string(raw), exampleVars())
				if err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				if hash == "" {
					t.Fatalf("%s: empty spec_hash", path)
				}
				spec.MergeTargetDefaults(&tr.Target, defaultTarget())
				if err := spec.ValidateTestRun(tr); err != nil {
					t.Fatalf("validate %s: %v", path, err)
				}
			default:
				t.Fatalf("%s: unknown kind %q (want Deployment|TestRun)", path, probe.Kind)
			}
		})
	}
	if !sawDeployment {
		t.Error("no Deployment spec fixture discovered")
	}
	if !sawTestRun {
		t.Error("no TestRun spec fixture discovered")
	}
}

// --- pipeline template models (typed so assertions target real nodes) --------

type ghWorkflow struct {
	Jobs map[string]ghJob `yaml:"jobs"`
}

type ghJob struct {
	Needs interface{} `yaml:"needs"`
	If    string      `yaml:"if"`
	Steps []ghStep    `yaml:"steps"`
}

type ghStep struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	If   string `yaml:"if"`
	Run  string `yaml:"run"`
}

type adoPipeline struct {
	Stages []adoStage `yaml:"stages"`
}

type adoStage struct {
	Stage     string      `yaml:"stage"`
	DependsOn interface{} `yaml:"dependsOn"`
	Condition string      `yaml:"condition"`
	Jobs      []adoJob    `yaml:"jobs"`
}

type adoJob struct {
	Job   string    `yaml:"job"`
	Steps []adoStep `yaml:"steps"`
}

type adoStep struct {
	Task        string `yaml:"task"`
	Script      string `yaml:"script"`
	DisplayName string `yaml:"displayName"`
	Condition   string `yaml:"condition"`
}

// asStringSet normalises a YAML scalar-or-sequence (needs:/dependsOn:) to a set.
func asStringSet(v interface{}) map[string]bool {
	out := map[string]bool{}
	switch t := v.(type) {
	case string:
		out[t] = true
	case []interface{}:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func containsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// TestGitHubPipelineTopologyAndGuards parses github-deploy.yml structurally and
// asserts the deploy/e2e/rollback topology plus the always() upload guard on
// the actual upload-artifact step.
func TestGitHubPipelineTopologyAndGuards(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("pipelines", "github-deploy.yml"))
	if err != nil {
		t.Fatalf("read github-deploy.yml: %v", err)
	}
	var wf ghWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("github-deploy.yml not well-formed YAML: %v", err)
	}
	deploy, ok := wf.Jobs["deploy"]
	if !ok {
		t.Fatal("github-deploy.yml: missing required job \"deploy\"")
	}
	rollback, ok := wf.Jobs["rollback"]
	if !ok {
		t.Fatal("github-deploy.yml: missing required job \"rollback\"")
	}

	// deploy job must run e2e (in the same apply) and upload results always().
	var sawE2E, sawUpload bool
	for _, s := range deploy.Steps {
		if containsFold(s.Name, "e2e") || containsFold(s.Run, "e2e") {
			sawE2E = true
		}
		if strings.HasPrefix(s.Uses, "actions/upload-artifact") {
			sawUpload = true
			if strings.TrimSpace(s.If) != "always()" {
				t.Errorf("deploy job upload-artifact step must carry `if: always()`, got %q", s.If)
			}
		}
	}
	if !sawE2E {
		t.Error("deploy job: no step references e2e (deploy + e2e run in one apply)")
	}
	if !sawUpload {
		t.Error("deploy job: missing actions/upload-artifact step for results/logs")
	}

	// rollback job must depend on deploy and only run on failure.
	if !asStringSet(rollback.Needs)["deploy"] {
		t.Errorf("rollback job must `needs: deploy`, got %v", rollback.Needs)
	}
	if strings.TrimSpace(rollback.If) != "failure()" {
		t.Errorf("rollback job must carry `if: failure()`, got %q", rollback.If)
	}
}

// TestAzurePipelineTopologyAndGuards parses azure-pipelines.yml structurally and
// asserts the Deploy/publish/Rollback topology plus condition: always() on the
// actual Publish* tasks.
func TestAzurePipelineTopologyAndGuards(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("pipelines", "azure-pipelines.yml"))
	if err != nil {
		t.Fatalf("read azure-pipelines.yml: %v", err)
	}
	var p adoPipeline
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatalf("azure-pipelines.yml not well-formed YAML: %v", err)
	}
	stages := map[string]adoStage{}
	for _, s := range p.Stages {
		stages[s.Stage] = s
	}
	deploy, ok := stages["Deploy"]
	if !ok {
		t.Fatal("azure-pipelines.yml: missing required stage \"Deploy\"")
	}
	rollback, ok := stages["Rollback"]
	if !ok {
		t.Fatal("azure-pipelines.yml: missing required stage \"Rollback\"")
	}

	// Deploy stage must run e2e and publish results + artifacts with always().
	var sawE2E, sawPublishResults, sawPublishArtifact bool
	for _, j := range deploy.Jobs {
		for _, s := range j.Steps {
			if containsFold(s.DisplayName, "e2e") || containsFold(s.Script, "e2e") {
				sawE2E = true
			}
			if strings.HasPrefix(s.Task, "PublishTestResults") {
				sawPublishResults = true
				if strings.TrimSpace(s.Condition) != "always()" {
					t.Errorf("PublishTestResults task must carry `condition: always()`, got %q", s.Condition)
				}
			}
			if strings.HasPrefix(s.Task, "PublishPipelineArtifact") {
				sawPublishArtifact = true
				if strings.TrimSpace(s.Condition) != "always()" {
					t.Errorf("PublishPipelineArtifact task must carry `condition: always()`, got %q", s.Condition)
				}
			}
		}
	}
	if !sawE2E {
		t.Error("Deploy stage: no step references e2e (deploy + e2e run in one apply)")
	}
	if !sawPublishResults {
		t.Error("Deploy stage: missing PublishTestResults task")
	}
	if !sawPublishArtifact {
		t.Error("Deploy stage: missing PublishPipelineArtifact task")
	}

	// Rollback stage must depend on Deploy and only run on failure.
	if !asStringSet(rollback.DependsOn)["Deploy"] {
		t.Errorf("Rollback stage must `dependsOn: Deploy`, got %v", rollback.DependsOn)
	}
	if strings.TrimSpace(rollback.Condition) != "failed()" {
		t.Errorf("Rollback stage must carry `condition: failed()`, got %q", rollback.Condition)
	}
}
