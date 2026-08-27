//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 8.2 (Examples and
// Pipeline Templates).
//
// Both scenarios read the COMMITTED example artifacts straight from disk and
// exercise the REAL code paths in-process — no external service is required
// (setup: inline; proof: golden fixtures on disk):
//
//   - Example specs validate -> discovers every committed examples/specs/*.yaml
//     (never a hard-coded list, so a newly added fixture cannot bypass the
//     gate), routes each to spec.ParseDeployment*/ParseTestRun* by its kind,
//     resolves the ${var:*} tokens and env-var NAMES the specs reference, merges
//     the example provider default_target (DESIGN §6.2 merge-then-validate), and
//     asserts each parses and validates without error. Both a Deployment and a
//     TestRun fixture must be represented.
//   - Pipeline YAML well-formed -> parses the two committed
//     examples/pipelines/*.yml templates with a real YAML parser into typed
//     models and asserts both are well-formed AND that the always()/condition:
//     always() guard sits on the ACTUAL upload-artifact / Publish* step (not
//     merely somewhere in the file text) so results/logs upload even on failure.
//
// Every Given/When/Then invokes the real spec engine / YAML parser and asserts
// on its result against the on-disk fixtures.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"gopkg.in/yaml.v3"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// exPipelineValidChecksum is a syntactically valid sha256:<64 hex> value used to
// resolve the ${var:CHECKSUM} token so checksum validation passes.
const exPipelineValidChecksum = "sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// exampleVars resolves every ${var:*} token referenced by the committed specs
// (mirrors examples/examples_test.go so the E2E gate uses the identical inputs
// the operator's copy-paste examples expect).
func examplePipelineVars() map[string]string {
	return map[string]string{
		"HOST":     "lab-host-01.contoso.lab",
		"CHECKSUM": exPipelineValidChecksum,
		"FEED_URL": "https://pkgs.contoso.lab/_packaging/lab/nuget/v3/index.json",
		"URL":      "https://artifacts.contoso.lab/sample/1.0.0/app.zip",
		"VERSION":  "1.0.0",
	}
}

// examplePipelineDefaultTarget mirrors the provider default_target block in
// examples/main.tf. The winrm specs deliberately omit credentials and inherit
// them here, so the golden gate must reproduce the provider's
// merge-then-validate flow (DESIGN §6.2) rather than validate the raw file in
// isolation.
func examplePipelineDefaultTarget() *spec.Target {
	return &spec.Target{
		Transport: spec.TransportWinRM,
		OS:        spec.OSWindows,
		Credentials: spec.Credentials{
			Username:    `LAB\deploy-svc`,
			PasswordEnv: "LABDEPLOY_PASSWORD",
		},
	}
}

// exampleEnvNames are the env-var NAMES the committed specs reference. They must
// be set (to any value) so requireEnv (VAL-08) passes without a live connection.
var exampleEnvNames = []string{
	"LABDEPLOY_PASSWORD",
	"LABDEPLOY_SSH_KEY",
	"NUGET_PAT",
	"REGISTRY_PASSWORD",
}

// examplePipelineRepoRoot walks up from the test package dir to the module root
// (the nearest go.mod) so the committed examples/ tree is located regardless of
// the working directory `go test` runs from.
func examplePipelineRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// --- typed pipeline models (so assertions target real nodes, not raw text) ---

type exGHWorkflow struct {
	Jobs map[string]exGHJob `yaml:"jobs"`
}

type exGHJob struct {
	Needs interface{} `yaml:"needs"`
	If    string      `yaml:"if"`
	Steps []exGHStep  `yaml:"steps"`
}

type exGHStep struct {
	Name string `yaml:"name"`
	Uses string `yaml:"uses"`
	If   string `yaml:"if"`
	Run  string `yaml:"run"`
}

type exADOPipeline struct {
	Stages []exADOStage `yaml:"stages"`
}

type exADOStage struct {
	Stage     string      `yaml:"stage"`
	DependsOn interface{} `yaml:"dependsOn"`
	Condition string      `yaml:"condition"`
	Jobs      []exADOJob  `yaml:"jobs"`
}

type exADOJob struct {
	Steps []exADOStep `yaml:"steps"`
}

type exADOStep struct {
	Task        string `yaml:"task"`
	Script      string `yaml:"script"`
	DisplayName string `yaml:"displayName"`
	Condition   string `yaml:"condition"`
}

// examplePipelineWorld carries state across the steps of a single scenario.
type examplePipelineWorld struct {
	root string

	specDir   string
	specPaths []string
	specErr   error
	sawDeploy bool
	sawTest   bool

	pipelineDir string
	gh          exGHWorkflow
	ghErr       error
	ado         exADOPipeline
	adoErr      error
}

// --- Scenario 1: example specs validate -------------------------------------

func (w *examplePipelineWorld) committedSpecsUnder(rel string) error {
	w.specDir = filepath.Join(w.root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(w.specDir)
	if err != nil {
		return fmt.Errorf("read specs dir %s: %w", w.specDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			w.specPaths = append(w.specPaths, filepath.Join(w.specDir, n))
		}
	}
	if len(w.specPaths) == 0 {
		return fmt.Errorf("no committed specs/*.yaml fixtures found under %s", w.specDir)
	}
	return nil
}

type exKindProbe struct {
	Kind string `yaml:"kind"`
}

func (w *examplePipelineWorld) eachSpecValidated() error {
	for _, name := range exampleEnvNames {
		if err := os.Setenv(name, "example-secret"); err != nil {
			return err
		}
	}
	vars := examplePipelineVars()
	def := examplePipelineDefaultTarget()
	for _, path := range w.specPaths {
		raw, err := os.ReadFile(path)
		if err != nil {
			w.specErr = fmt.Errorf("read %s: %w", path, err)
			return nil
		}
		var probe exKindProbe
		if err := yaml.Unmarshal(raw, &probe); err != nil {
			w.specErr = fmt.Errorf("%s: not well-formed YAML: %w", path, err)
			return nil
		}
		switch probe.Kind {
		case "Deployment":
			w.sawDeploy = true
			d, hash, err := spec.ParseDeploymentLenient(string(raw), vars, "")
			if err != nil {
				w.specErr = fmt.Errorf("parse %s: %w", path, err)
				return nil
			}
			if hash == "" {
				w.specErr = fmt.Errorf("%s: empty spec_hash", path)
				return nil
			}
			spec.MergeTargetDefaults(&d.Target, def)
			if err := spec.ValidateDeployment(d); err != nil {
				w.specErr = fmt.Errorf("validate %s: %w", path, err)
				return nil
			}
		case "TestRun":
			w.sawTest = true
			tr, hash, err := spec.ParseTestRunLenient(string(raw), vars)
			if err != nil {
				w.specErr = fmt.Errorf("parse %s: %w", path, err)
				return nil
			}
			if hash == "" {
				w.specErr = fmt.Errorf("%s: empty spec_hash", path)
				return nil
			}
			spec.MergeTargetDefaults(&tr.Target, def)
			if err := spec.ValidateTestRun(tr); err != nil {
				w.specErr = fmt.Errorf("validate %s: %w", path, err)
				return nil
			}
		default:
			w.specErr = fmt.Errorf("%s: unknown kind %q (want Deployment|TestRun)", path, probe.Kind)
			return nil
		}
	}
	return nil
}

func (w *examplePipelineWorld) everySpecValidatesWithoutError() error {
	if w.specErr != nil {
		return w.specErr
	}
	return nil
}

func (w *examplePipelineWorld) bothKindsCovered() error {
	if !w.sawDeploy {
		return fmt.Errorf("no Deployment spec fixture discovered under %s", w.specDir)
	}
	if !w.sawTest {
		return fmt.Errorf("no TestRun spec fixture discovered under %s", w.specDir)
	}
	return nil
}

// --- Scenario 2: pipeline templates well-formed with always() guards ---------

func (w *examplePipelineWorld) committedPipelinesUnder(rel string) error {
	w.pipelineDir = filepath.Join(w.root, filepath.FromSlash(rel))
	if _, err := os.Stat(w.pipelineDir); err != nil {
		return fmt.Errorf("pipelines dir %s: %w", w.pipelineDir, err)
	}
	return nil
}

func (w *examplePipelineWorld) eachPipelineParsed() error {
	ghRaw, err := os.ReadFile(filepath.Join(w.pipelineDir, "github-deploy.yml"))
	if err != nil {
		w.ghErr = fmt.Errorf("read github-deploy.yml: %w", err)
	} else {
		w.ghErr = yaml.Unmarshal(ghRaw, &w.gh)
	}
	adoRaw, err := os.ReadFile(filepath.Join(w.pipelineDir, "azure-pipelines.yml"))
	if err != nil {
		w.adoErr = fmt.Errorf("read azure-pipelines.yml: %w", err)
	} else {
		w.adoErr = yaml.Unmarshal(adoRaw, &w.ado)
	}
	return nil
}

func (w *examplePipelineWorld) bothPipelinesWellFormed() error {
	if w.ghErr != nil {
		return fmt.Errorf("github-deploy.yml not well-formed YAML: %w", w.ghErr)
	}
	if w.adoErr != nil {
		return fmt.Errorf("azure-pipelines.yml not well-formed YAML: %w", w.adoErr)
	}
	if len(w.gh.Jobs) == 0 {
		return fmt.Errorf("github-deploy.yml: parsed but has no jobs")
	}
	if len(w.ado.Stages) == 0 {
		return fmt.Errorf("azure-pipelines.yml: parsed but has no stages")
	}
	return nil
}

func (w *examplePipelineWorld) githubUploadsGuardedBy(guard string) error {
	deploy, ok := w.gh.Jobs["deploy"]
	if !ok {
		return fmt.Errorf("github-deploy.yml: missing required job \"deploy\"")
	}
	var sawUpload bool
	for _, s := range deploy.Steps {
		if strings.HasPrefix(s.Uses, "actions/upload-artifact") {
			sawUpload = true
			if strings.TrimSpace(s.If) != guard {
				return fmt.Errorf("deploy job upload-artifact step must carry `if: %s`, got %q", guard, s.If)
			}
		}
	}
	if !sawUpload {
		return fmt.Errorf("deploy job: missing actions/upload-artifact step for results/logs")
	}
	return nil
}

func (w *examplePipelineWorld) azurePublishesGuardedBy(guard string) error {
	var deploy *exADOStage
	for i := range w.ado.Stages {
		if w.ado.Stages[i].Stage == "Deploy" {
			deploy = &w.ado.Stages[i]
		}
	}
	if deploy == nil {
		return fmt.Errorf("azure-pipelines.yml: missing required stage \"Deploy\"")
	}
	var sawResults, sawArtifact bool
	for _, j := range deploy.Jobs {
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Task, "PublishTestResults") {
				sawResults = true
				if strings.TrimSpace(s.Condition) != guard {
					return fmt.Errorf("PublishTestResults task must carry `condition: %s`, got %q", guard, s.Condition)
				}
			}
			if strings.HasPrefix(s.Task, "PublishPipelineArtifact") {
				sawArtifact = true
				if strings.TrimSpace(s.Condition) != guard {
					return fmt.Errorf("PublishPipelineArtifact task must carry `condition: %s`, got %q", guard, s.Condition)
				}
			}
		}
	}
	if !sawResults {
		return fmt.Errorf("Deploy stage: missing PublishTestResults task")
	}
	if !sawArtifact {
		return fmt.Errorf("Deploy stage: missing PublishPipelineArtifact task")
	}
	return nil
}

// InitializeScenario_docker_examples_and_packaging_examples_and_pipeline_templates
// registers the step definitions for the Stage 8.2 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_docker_examples_and_packaging_examples_and_pipeline_templates(ctx *godog.ScenarioContext) {
	w := &examplePipelineWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		root, err := examplePipelineRepoRoot()
		if err != nil {
			return c, err
		}
		*w = examplePipelineWorld{root: root}
		return c, nil
	})

	// Scenario 1: example specs validate.
	ctx.Step(`^the committed example specs under "([^"]*)"$`, w.committedSpecsUnder)
	ctx.Step(`^each committed spec is run through the spec validator$`, w.eachSpecValidated)
	ctx.Step(`^every committed spec parses and validates without error$`, w.everySpecValidatesWithoutError)
	ctx.Step(`^both a Deployment and a TestRun fixture are covered$`, w.bothKindsCovered)

	// Scenario 2: pipeline templates well-formed with always() guards.
	ctx.Step(`^the committed pipeline templates under "([^"]*)"$`, w.committedPipelinesUnder)
	ctx.Step(`^each committed pipeline file is parsed with a YAML parser$`, w.eachPipelineParsed)
	ctx.Step(`^both pipeline templates are well-formed YAML$`, w.bothPipelinesWellFormed)
	ctx.Step(`^the GitHub workflow uploads results guarded by "([^"]*)"$`, w.githubUploadsGuardedBy)
	ctx.Step(`^the Azure pipeline publishes results guarded by "([^"]*)"$`, w.azurePublishesGuardedBy)
}

// TestE2E_docker_examples_and_packaging_examples_and_pipeline_templates is the
// go test entrypoint for the Stage 8.2 godog suite.
func TestE2E_docker_examples_and_packaging_examples_and_pipeline_templates(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_docker_examples_and_packaging_examples_and_pipeline_templates,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"docker_examples_and_packaging_examples_and_pipeline_templates.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
