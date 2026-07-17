//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 1.3 (Spec
// Validation and Variable Substitution).
//
// Every scenario exercises the REAL spec engine (internal/spec) and the REAL
// provider config layer (internal/provider) in-process — no external service is
// required (setup: inline; proof: in-process):
//
//   - Validation matrix -> replays the DESIGN §18.1 VAL-01..VAL-09 table. Eight
//     rows are validated through spec.ParseDeployment and must fail as
//     [ERR_SPEC_INVALID] naming the offending JSON path / message fragment.
//     VAL-06 (both spec and spec_file set) is a Terraform config-layer concern,
//     so it is driven through the exported DeploymentResource ConfigValidators
//     exactly as Terraform does during plan/validate.
//   - Variable substitution and escape -> proves spec.Substitute replaces
//     ${var:X}, preserves the $${var:Y} escape literally, and that an unknown
//     ${var:NAME} yields [ERR_SPEC_INVALID] naming its JSON path.
//   - files path traversal rejected -> proves spec.ValidateDeployment rejects
//     absolute and ".." paths naming files[i].path while a plain relative path
//     passes.
//
// Every Given/When/Then invokes the real code and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	tftypes "github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// --- fixtures (copied verbatim from internal/spec tests so the E2E suite
// exercises the exact contract shapes the engine promises) --------------------

const valWinSvcYAML = `
apiVersion: labdeploy/v1
kind: Deployment
metadata:
  name: sample-svc
target:
  transport: winrm
  hosts: ["${var:HOST}"]
  os: windows
  credentials:
    username: LAB\deploy
    password_env: LABDEPLOY_PASSWORD
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
`

const valClusterOneHostYAML = `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: clu1 }
target:
  transport: winrm
  hosts: ["only-node"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  source: { type: http, url: "https://x/y.zip" }
pattern:
  type: cluster_generic_service
  service_name: S
  role_name: R
  exe: s.exe
health_check: { type: none }
`

const valDockerWinSvcYAML = `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: dk }
target:
  transport: winrm
  hosts: ["h1"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: docker_image
  version: 1.0.0
  source: { type: docker_registry, image: "repo/img:tag" }
pattern:
  type: windows_service
  service_name: S
  exe: s.exe
health_check: { type: none }
`

const valSHA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// valCase describes one VAL-* row that is driven through spec.ParseDeployment.
type valCase struct {
	yaml      string
	vars      map[string]string
	env       map[string]string
	fragments []string
}

// valCases mirrors the DESIGN §18.1 VAL matrix (in-process rows). VAL-06 is
// handled separately via the provider config validator.
var valCases = map[string]valCase{
	"VAL-01": {
		yaml:      "apiVersion: labdeploy/v1\nkind: Deployment\nmetadata:\n\tname: x\n",
		fragments: []string{"[ERR_SPEC_INVALID]", "line"},
	},
	"VAL-02": {
		yaml:      strings.Replace(valWinSvcYAML, "  version: 1.0.0\n", "", 1),
		vars:      map[string]string{"HOST": "h"},
		env:       map[string]string{"LABDEPLOY_PASSWORD": "x"},
		fragments: []string{"[ERR_SPEC_INVALID]", "artifact.version"},
	},
	"VAL-03": {
		yaml:      strings.Replace(valWinSvcYAML, "os: windows", "os: linux", 1),
		vars:      map[string]string{"HOST": "h"},
		env:       map[string]string{"LABDEPLOY_PASSWORD": "x"},
		fragments: []string{"[ERR_SPEC_INVALID]", "pattern.type", "windows_service", "linux"},
	},
	"VAL-04": {
		yaml:      valClusterOneHostYAML,
		env:       map[string]string{"LABDEPLOY_PASSWORD": "x"},
		fragments: []string{"[ERR_SPEC_INVALID]", "target.hosts", "2..16"},
	},
	"VAL-05": {
		yaml:      strings.Replace(valWinSvcYAML, valSHA, "md5:deadbeef", 1),
		vars:      map[string]string{"HOST": "h"},
		env:       map[string]string{"LABDEPLOY_PASSWORD": "x"},
		fragments: []string{"[ERR_SPEC_INVALID]", "artifact.checksum"},
	},
	"VAL-07": {
		yaml:      valWinSvcYAML,
		vars:      nil,
		env:       map[string]string{"LABDEPLOY_PASSWORD": "x"},
		fragments: []string{"[ERR_SPEC_INVALID]", "unresolved variable HOST", "at target.hosts[0]"},
	},
	"VAL-08": {
		yaml:      strings.Replace(valWinSvcYAML, "LABDEPLOY_PASSWORD", "NEVER_SET_ENV_VAL08", 1),
		vars:      map[string]string{"HOST": "h"},
		fragments: []string{"[ERR_SPEC_INVALID]", "env var NEVER_SET_ENV_VAL08", "referenced by target.credentials.password_env", "is not set"},
	},
	"VAL-09": {
		yaml:      valDockerWinSvcYAML,
		env:       map[string]string{"LABDEPLOY_PASSWORD": "x"},
		fragments: []string{"[ERR_SPEC_INVALID]", "artifact.type", "docker_image", "docker_container"},
	},
}

// val06ConfigError drives the DESIGN §18.1 VAL-06 rule (exactly one of
// spec/spec_file) through the REAL provider config validators — exactly as
// Terraform does during plan/validate — with BOTH spec and spec_file set. It
// returns the resulting diagnostic as an error so the matrix step can treat it
// uniformly with the spec-engine rows.
func val06ConfigError() error {
	ctx := context.Background()
	r := provider.NewDeploymentResource()

	sresp := &resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, sresp)
	sc := sresp.Schema

	objType := sc.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, ty := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(ty, nil) // null
	}
	vals["spec"] = tftypes.NewValue(tftypes.String, "apiVersion: labdeploy/v1")
	vals["spec_file"] = tftypes.NewValue(tftypes.String, "/tmp/spec.yaml")
	cfg := tfsdk.Config{Schema: sc, Raw: tftypes.NewValue(objType, vals)}

	v, ok := r.(resource.ResourceWithConfigValidators)
	if !ok {
		return fmt.Errorf("DeploymentResource does not declare ConfigValidators")
	}
	resp := &resource.ValidateConfigResponse{}
	for _, cv := range v.ConfigValidators(ctx) {
		cv.ValidateResource(ctx, resource.ValidateConfigRequest{Config: cfg}, resp)
	}
	if !resp.Diagnostics.HasError() {
		return nil
	}
	d := resp.Diagnostics.Errors()[0]
	return fmt.Errorf("%s %s", d.Summary(), d.Detail())
}

// valWorld carries state across the steps of a single scenario.
type valWorld struct {
	caseID string
	err    error

	subOut string
	subErr error

	base    *spec.Deployment
	pathErr error
	pathOK  bool
}

// --- Validation matrix ------------------------------------------------------

func (w *valWorld) invalidCase(id string) error {
	if id != "VAL-06" {
		if _, ok := valCases[id]; !ok {
			return fmt.Errorf("unknown VAL case %q", id)
		}
	}
	w.caseID = id
	return nil
}

func (w *valWorld) theSpecIsValidated() error {
	// A referenced-but-unset env var must stay unset for VAL-08.
	_ = os.Unsetenv("NEVER_SET_ENV_VAL08")

	if w.caseID == "VAL-06" {
		w.err = val06ConfigError()
		return nil
	}
	c := valCases[w.caseID]
	for k, v := range c.env {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	_, _, w.err = spec.ParseDeployment(c.yaml, c.vars, "")
	return nil
}

func (w *valWorld) validationFailsNaming(fragment string) error {
	if w.err == nil {
		return fmt.Errorf("%s: expected ERR_SPEC_INVALID, got nil", w.caseID)
	}
	msg := w.err.Error()
	if !strings.Contains(msg, "ERR_SPEC_INVALID") {
		return fmt.Errorf("%s: error not ERR_SPEC_INVALID-coded: %v", w.caseID, w.err)
	}
	if !strings.Contains(msg, fragment) {
		return fmt.Errorf("%s: error missing expected fragment %q: got %v", w.caseID, fragment, w.err)
	}
	// Assert the full contract fragment list for the spec-engine rows.
	if c, ok := valCases[w.caseID]; ok {
		for _, f := range c.fragments {
			if !strings.Contains(msg, f) {
				return fmt.Errorf("%s: error missing contract fragment %q: got %v", w.caseID, f, w.err)
			}
		}
	}
	return nil
}

// --- Variable substitution and escape ---------------------------------------

func (w *valWorld) aSpecFragmentWithVarAndEscape(_, _ string) error {
	// Fragment holds a resolvable ${var:X} and an escaped $${var:Y}.
	w.subOut = "host=${var:X} keep=$${var:Y}"
	return nil
}

func (w *valWorld) itIsSubstitutedWith(value string) error {
	out, err := spec.Substitute("host=${var:X} keep=$${var:Y}", map[string]string{"X": value})
	w.subOut, w.subErr = out, err
	return nil
}

func (w *valWorld) varReplacedWith(value string) error {
	if w.subErr != nil {
		return fmt.Errorf("substitution errored: %w", w.subErr)
	}
	if !strings.Contains(w.subOut, "host="+value) {
		return fmt.Errorf("${var:X} not replaced with %q: got %q", value, w.subOut)
	}
	return nil
}

func (w *valWorld) literalPreserved() error {
	if !strings.Contains(w.subOut, "keep=${var:Y}") {
		return fmt.Errorf("escaped $${var:Y} not preserved as literal ${var:Y}: got %q", w.subOut)
	}
	if strings.Contains(w.subOut, "$${var:Y}") {
		return fmt.Errorf("escape was not collapsed to a single literal: got %q", w.subOut)
	}
	return nil
}

func (w *valWorld) unknownNameYieldsErr() error {
	// Parse a spec that references ${var:HOST} with NO variables supplied.
	if err := os.Setenv("LABDEPLOY_PASSWORD", "x"); err != nil {
		return err
	}
	_, _, err := spec.ParseDeployment(valWinSvcYAML, nil, "")
	if err == nil {
		return fmt.Errorf("unknown variable name must yield ERR_SPEC_INVALID, got nil")
	}
	for _, f := range []string{"[ERR_SPEC_INVALID]", "unresolved variable HOST", "at target.hosts[0]"} {
		if !strings.Contains(err.Error(), f) {
			return fmt.Errorf("unknown-var error missing %q: got %v", f, err)
		}
	}
	return nil
}

// --- files path traversal ---------------------------------------------------

func (w *valWorld) aValidBaseSpec() error {
	if err := os.Setenv("LABDEPLOY_PASSWORD", "x"); err != nil {
		return err
	}
	d, _, err := spec.ParseDeployment(valWinSvcYAML, map[string]string{"HOST": "lab-01"}, "")
	if err != nil {
		return fmt.Errorf("base parse: %w", err)
	}
	w.base = d
	return nil
}

func (w *valWorld) filePathSetTo(path string) error {
	if w.base == nil {
		return fmt.Errorf("no base spec; the Given step did not run")
	}
	w.base.Files = []spec.RenderedFile{{Path: path, Content: "x"}}
	err := spec.ValidateDeployment(w.base)
	w.pathErr = err
	w.pathOK = err == nil
	return nil
}

func (w *valWorld) theSpecValidation(outcome string) error {
	switch outcome {
	case "passes":
		if !w.pathOK {
			return fmt.Errorf("relative path should pass validation, got %v", w.pathErr)
		}
		return nil
	case "rejected":
		if w.pathErr == nil {
			return fmt.Errorf("path must be rejected, got nil error")
		}
		msg := w.pathErr.Error()
		if !strings.Contains(msg, "ERR_SPEC_INVALID") {
			return fmt.Errorf("rejection not ERR_SPEC_INVALID-coded: %v", w.pathErr)
		}
		if !strings.Contains(msg, "files[0].path") {
			return fmt.Errorf("rejection must name files[0].path: got %v", w.pathErr)
		}
		return nil
	default:
		return fmt.Errorf("unknown outcome %q", outcome)
	}
}

// InitializeScenario_project_scaffold_and_spec_engine_spec_validation_and_variable_substitution
// registers the step definitions for the Stage 1.3 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_project_scaffold_and_spec_engine_spec_validation_and_variable_substitution(ctx *godog.ScenarioContext) {
	w := &valWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = valWorld{}
		// Baseline for scenarios that parse the valid fixture.
		if err := os.Setenv("LABDEPLOY_PASSWORD", "x"); err != nil {
			return c, err
		}
		return c, nil
	})

	// Validation matrix.
	ctx.Step(`^the invalid Deployment spec case "([^"]*)"$`, w.invalidCase)
	ctx.Step(`^the spec is validated$`, w.theSpecIsValidated)
	ctx.Step(`^validation fails with ERR_SPEC_INVALID naming "([^"]*)"$`, w.validationFailsNaming)

	// Variable substitution and escape.
	ctx.Step(`^a spec fragment containing "([^"]*)" and an escaped "([^"]*)"$`, w.aSpecFragmentWithVarAndEscape)
	ctx.Step(`^it is substituted with variable X set to "([^"]*)"$`, w.itIsSubstitutedWith)
	ctx.Step(`^"([^"]*)" is replaced with "([^"]*)"$`, func(_ /*token*/, value string) error { return w.varReplacedWith(value) })
	ctx.Step(`^the literal "([^"]*)" is preserved$`, func(_ string) error { return w.literalPreserved() })
	ctx.Step(`^substituting a spec with an unknown variable name yields ERR_SPEC_INVALID$`, w.unknownNameYieldsErr)

	// files path traversal.
	ctx.Step(`^a valid base Deployment spec$`, w.aValidBaseSpec)
	ctx.Step(`^files\[0\]\.path is set to "([^"]*)"$`, w.filePathSetTo)
	ctx.Step(`^the spec validation "([^"]*)"$`, w.theSpecValidation)
}

// TestE2E_project_scaffold_and_spec_engine_spec_validation_and_variable_substitution
// is the go test entrypoint for the Stage 1.3 godog suite.
func TestE2E_project_scaffold_and_spec_engine_spec_validation_and_variable_substitution(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_project_scaffold_and_spec_engine_spec_validation_and_variable_substitution,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"project_scaffold_and_spec_engine_spec_validation_and_variable_substitution.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
