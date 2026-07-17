//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 1.4 (Canonical
// JSON and Spec Hashing).
//
// Both scenarios exercise the REAL spec engine (internal/spec) in-process — no
// external service is required:
//
//   - Hash stable across key order (proof: golden) -> reads the two COMMITTED
//     canonical-JSON fixtures under internal/spec/testdata (canonical_order_a.json
//     and canonical_order_b.json) which encode the SAME document with keys in
//     different orders, hashes each through spec.CanonicalHash, and asserts the
//     two spec_hash values are byte-identical. This is the T2 preservation proof:
//     the hash is derived from canonical (sorted-key) JSON, so author key order
//     cannot move it.
//   - Override changes hash (proof: in-process) -> parses a base Deployment spec
//     to obtain (base version, base spec_hash), then re-parses the SAME source
//     with a version_override and asserts artifact.version now equals the override
//     AND the spec_hash differs from the base.
//
// Every Given/When/Then invokes the real engine and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// The two committed fixtures live in the spec package's testdata, three levels
// above this package's working directory (test/e2e/release-RELEASE-PROVIDER).
const (
	canonicalTestdataDir = "../../../internal/spec/testdata"
	canonicalOrderA      = "canonical_order_a.json"
	canonicalOrderB      = "canonical_order_b.json"
)

// canonicalBaseSpec is the base Deployment used by the override scenario. It is
// authored in JSON with a concrete artifact.version so the override is provably
// a change.
const canonicalBaseSpec = `{
  "apiVersion": "labdeploy/v1",
  "kind": "Deployment",
  "metadata": { "name": "sample-svc" },
  "target": {
    "transport": "winrm",
    "hosts": ["lab-01"],
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

const canonicalBaseVersion = "1.0.0"
const canonicalVersionOverride = "2.5.0"

// hashWorld carries state across the steps of a single scenario.
type hashWorld struct {
	// key-order scenario
	hashA string
	hashB string

	// override scenario
	baseVersion  string
	baseHash     string
	overrideVer  string
	overrideHash string
}

// --- Hash stable across key order (golden fixtures) -------------------------

func (w *hashWorld) committedFixtureInDifferentOrders() error {
	for _, f := range []string{canonicalOrderA, canonicalOrderB} {
		p := filepath.Join(canonicalTestdataDir, f)
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("committed canonical-JSON fixture %s missing: %w", p, err)
		}
	}
	return nil
}

func (w *hashWorld) eachOrderingIsHashed() error {
	ha, err := hashFixture(canonicalOrderA)
	if err != nil {
		return err
	}
	hb, err := hashFixture(canonicalOrderB)
	if err != nil {
		return err
	}
	w.hashA, w.hashB = ha, hb
	return nil
}

func hashFixture(name string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(canonicalTestdataDir, name))
	if err != nil {
		return "", fmt.Errorf("read fixture %s: %w", name, err)
	}
	h, err := spec.CanonicalHash(raw)
	if err != nil {
		return "", fmt.Errorf("hash fixture %s: %w", name, err)
	}
	if h == "" {
		return "", fmt.Errorf("empty hash for fixture %s", name)
	}
	return h, nil
}

func (w *hashWorld) hashByteIdenticalAcrossOrderings() error {
	if w.hashA == "" || w.hashB == "" {
		return fmt.Errorf("hashes missing; the When step did not run")
	}
	if w.hashA != w.hashB {
		return fmt.Errorf("spec hash NOT stable across key order: order_a=%s order_b=%s", w.hashA, w.hashB)
	}
	return nil
}

// --- Override changes hash (in-process) -------------------------------------

func (w *hashWorld) aBaseDeploymentSpec() error {
	d, h, err := spec.ParseDeployment(canonicalBaseSpec, nil, "")
	if err != nil {
		return fmt.Errorf("base spec parse failed: %w", err)
	}
	if d.Artifact.Version != canonicalBaseVersion {
		return fmt.Errorf("base artifact.version = %q, want %q", d.Artifact.Version, canonicalBaseVersion)
	}
	w.baseVersion = d.Artifact.Version
	w.baseHash = h
	return nil
}

func (w *hashWorld) aVersionOverrideIsApplied() error {
	d, h, err := spec.ParseDeployment(canonicalBaseSpec, nil, canonicalVersionOverride)
	if err != nil {
		return fmt.Errorf("override spec parse failed: %w", err)
	}
	w.overrideVer = d.Artifact.Version
	w.overrideHash = h
	return nil
}

func (w *hashWorld) artifactVersionReflectsOverride() error {
	if w.overrideVer != canonicalVersionOverride {
		return fmt.Errorf("artifact.version = %q, want override %q", w.overrideVer, canonicalVersionOverride)
	}
	return nil
}

func (w *hashWorld) specHashDiffersFromBase() error {
	if w.baseHash == "" || w.overrideHash == "" {
		return fmt.Errorf("hashes missing; a prior step did not run")
	}
	if w.baseHash == w.overrideHash {
		return fmt.Errorf("override did NOT change spec hash: base=%s override=%s", w.baseHash, w.overrideHash)
	}
	return nil
}

// InitializeScenario_project_scaffold_and_spec_engine_canonical_json_and_spec_hashing
// registers the step definitions for the Stage 1.4 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_project_scaffold_and_spec_engine_canonical_json_and_spec_hashing(ctx *godog.ScenarioContext) {
	w := &hashWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = hashWorld{}
		// ValidateDeployment requires the env var named by
		// target.credentials.password_env to be set (validate.go).
		if err := os.Setenv("LABDEPLOY_PASSWORD", "x"); err != nil {
			return c, err
		}
		return c, nil
	})

	ctx.Step(`^a committed canonical-JSON fixture rendered with keys in different orders$`, w.committedFixtureInDifferentOrders)
	ctx.Step(`^each ordering is hashed$`, w.eachOrderingIsHashed)
	ctx.Step(`^the spec hash is byte-identical across orderings$`, w.hashByteIdenticalAcrossOrderings)

	ctx.Step(`^a base Deployment spec$`, w.aBaseDeploymentSpec)
	ctx.Step(`^a version_override is applied$`, w.aVersionOverrideIsApplied)
	ctx.Step(`^artifact\.version reflects the override$`, w.artifactVersionReflectsOverride)
	ctx.Step(`^the spec hash differs from the base spec hash$`, w.specHashDiffersFromBase)
}

// TestE2E_project_scaffold_and_spec_engine_canonical_json_and_spec_hashing is the
// go test entrypoint for the Stage 1.4 godog suite.
func TestE2E_project_scaffold_and_spec_engine_canonical_json_and_spec_hashing(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_project_scaffold_and_spec_engine_canonical_json_and_spec_hashing,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"project_scaffold_and_spec_engine_canonical_json_and_spec_hashing.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
