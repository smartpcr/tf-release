//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 1.2 (Spec Types
// and YAML/JSON Parsing).
//
// Both scenarios exercise the REAL spec engine (internal/spec.ParseDeployment)
// against in-process fixtures — no external service is required (setup: inline;
// proof: in-process):
//
//   - YAML equals JSON -> authors the SAME Deployment spec as both YAML and JSON,
//     parses each through the real parser, and asserts the two in-memory
//     *spec.Deployment structs are deeply equal AND yield the identical canonical
//     spec hash. This proves the YAML->JSON->struct pipeline is format-agnostic.
//   - Pattern union decode -> parses a spec whose pattern.type is windows_service
//     and asserts the concrete WindowsService union member is populated while
//     every other union member (ConsoleApp, NodeWebApp, DotnetAPI, ClusterGeneric,
//     DockerContainer) is nil — the type-directed decode of DESIGN §6.4.
//
// Every Given/When/Then invokes the real parser and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// The SAME Deployment spec authored two ways. yamlSpec and jsonSpec must parse to
// byte-identical in-memory structs and the same canonical hash.
const specParsingYAML = `
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

const specParsingJSON = `{
  "apiVersion": "labdeploy/v1",
  "kind": "Deployment",
  "metadata": { "name": "sample-svc" },
  "target": {
    "transport": "winrm",
    "hosts": ["${var:HOST}"],
    "os": "windows",
    "credentials": { "username": "LAB\\deploy", "password_env": "LABDEPLOY_PASSWORD" }
  },
  "artifact": {
    "type": "zip",
    "version": "1.0.0",
    "checksum": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "source": { "type": "http", "url": "https://x/y.zip" }
  },
  "pattern": {
    "type": "windows_service",
    "service_name": "SampleSvc",
    "exe": "bin\\SampleSvc.exe"
  },
  "health_check": { "type": "http", "http": { "url": "http://localhost:8080/health" } }
}`

// specWorld carries state across the steps of a single spec-parsing scenario.
type specWorld struct {
	yamlSrc string
	jsonSrc string

	fromYAML *spec.Deployment
	fromJSON *spec.Deployment
	yamlHash string
	jsonHash string

	single *spec.Deployment
}

func specParseVars() map[string]string { return map[string]string{"HOST": "lab-01"} }

// --- YAML equals JSON -------------------------------------------------------

func (w *specWorld) sameSpecAuthoredYAMLAndJSON() error {
	w.yamlSrc = specParsingYAML
	w.jsonSrc = specParsingJSON
	return nil
}

func (w *specWorld) bothAreParsed() error {
	vars := specParseVars()
	dy, hy, err := spec.ParseDeployment(w.yamlSrc, vars, "")
	if err != nil {
		return fmt.Errorf("YAML parse failed: %w", err)
	}
	dj, hj, err := spec.ParseDeployment(w.jsonSrc, vars, "")
	if err != nil {
		return fmt.Errorf("JSON parse failed: %w", err)
	}
	w.fromYAML, w.yamlHash = dy, hy
	w.fromJSON, w.jsonHash = dj, hj
	return nil
}

func (w *specWorld) produceIdenticalStructs() error {
	if w.fromYAML == nil || w.fromJSON == nil {
		return fmt.Errorf("parse results missing; a When step did not run")
	}
	if !reflect.DeepEqual(w.fromYAML, w.fromJSON) {
		return fmt.Errorf("YAML and JSON produced different structs:\n yaml=%+v\n json=%+v", w.fromYAML, w.fromJSON)
	}
	return nil
}

func (w *specWorld) produceSameCanonicalHash() error {
	if w.yamlHash == "" || w.jsonHash == "" {
		return fmt.Errorf("canonical hashes missing; a When step did not run")
	}
	if w.yamlHash != w.jsonHash {
		return fmt.Errorf("canonical hash differs across formats: %s vs %s", w.yamlHash, w.jsonHash)
	}
	return nil
}

// --- Pattern union decode ---------------------------------------------------

func (w *specWorld) specWithPatternType(patternType string) error {
	if patternType != "windows_service" {
		return fmt.Errorf("unexpected pattern.type in scenario: %q", patternType)
	}
	w.yamlSrc = specParsingYAML
	return nil
}

func (w *specWorld) itIsParsed() error {
	d, _, err := spec.ParseDeployment(w.yamlSrc, specParseVars(), "")
	if err != nil {
		return fmt.Errorf("parse failed: %w", err)
	}
	w.single = d
	return nil
}

func (w *specWorld) windowsServiceMemberPopulated() error {
	if w.single == nil {
		return fmt.Errorf("no parsed spec; a When step did not run")
	}
	p := w.single.Pattern
	if p.Type != spec.PatternWindowsService {
		return fmt.Errorf("wrong pattern type: %q", p.Type)
	}
	if p.WindowsService == nil {
		return fmt.Errorf("windows_service concrete member is nil; type-directed decode failed")
	}
	if p.WindowsService.ServiceName != "SampleSvc" {
		return fmt.Errorf("windows_service member not populated: %+v", p.WindowsService)
	}
	if p.WindowsService.Exe != `bin\SampleSvc.exe` {
		return fmt.Errorf("windows_service.exe not populated: %q", p.WindowsService.Exe)
	}
	return nil
}

func (w *specWorld) everyOtherUnionMemberNil() error {
	p := w.single.Pattern
	if p.ConsoleApp != nil {
		return fmt.Errorf("console_app member should be nil, got %+v", p.ConsoleApp)
	}
	if p.NodeWebApp != nil {
		return fmt.Errorf("node_web_app member should be nil, got %+v", p.NodeWebApp)
	}
	if p.DotnetAPI != nil {
		return fmt.Errorf("dotnet_api member should be nil, got %+v", p.DotnetAPI)
	}
	if p.ClusterGeneric != nil {
		return fmt.Errorf("cluster_generic_service member should be nil, got %+v", p.ClusterGeneric)
	}
	if p.DockerContainer != nil {
		return fmt.Errorf("docker_container member should be nil, got %+v", p.DockerContainer)
	}
	return nil
}

// InitializeScenario_project_scaffold_and_spec_engine_spec_types_and_yaml_json_parsing
// registers the step definitions for the Stage 1.2 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_project_scaffold_and_spec_engine_spec_types_and_yaml_json_parsing(ctx *godog.ScenarioContext) {
	w := &specWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = specWorld{}
		// ValidateDeployment requires the env var named by
		// target.credentials.password_env to be set (validate.go).
		if err := os.Setenv("LABDEPLOY_PASSWORD", "x"); err != nil {
			return c, err
		}
		return c, nil
	})

	ctx.Step(`^the same Deployment spec authored in YAML and in JSON$`, w.sameSpecAuthoredYAMLAndJSON)
	ctx.Step(`^both are parsed$`, w.bothAreParsed)
	ctx.Step(`^they produce identical in-memory structs$`, w.produceIdenticalStructs)
	ctx.Step(`^they produce the same canonical spec hash$`, w.produceSameCanonicalHash)

	ctx.Step(`^a Deployment spec with "pattern\.type: windows_service"$`, func() error {
		return w.specWithPatternType("windows_service")
	})
	ctx.Step(`^it is parsed$`, w.itIsParsed)
	ctx.Step(`^the windows_service concrete struct is populated$`, w.windowsServiceMemberPopulated)
	ctx.Step(`^every other pattern union member is nil$`, w.everyOtherUnionMemberNil)
}

// TestE2E_project_scaffold_and_spec_engine_spec_types_and_yaml_json_parsing is the
// go test entrypoint for the Stage 1.2 godog suite.
func TestE2E_project_scaffold_and_spec_engine_spec_types_and_yaml_json_parsing(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_project_scaffold_and_spec_engine_spec_types_and_yaml_json_parsing,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"project_scaffold_and_spec_engine_spec_types_and_yaml_json_parsing.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
