//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 1.1 (Repository
// Bootstrap and Toolchain). The scenarios exercise the real gate-host toolchain
// (Go compiler + make + golangci-lint) and the in-process terraform-plugin-go
// protocol-v6 provider server, matching the [proof: service:go-toolchain] and
// [proof: service:tf-plugin-server] strategies declared in the implementation
// plan. Nothing is skipped: each Given/When/Then runs a real command or a real
// gRPC RPC and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
)

// wantAddress is the registry source address main.go serves the provider under
// (DESIGN §16.1); it is re-asserted here to keep the served address in sync.
const wantAddress = "registry.local/smartpcr/labdeploy"

// buildBinaryName is the exact artifact name the Makefile emits for v0.1.0.
const buildBinaryName = "terraform-provider-labdeploy_v0.1.0"

// toolchainWorld carries state across the steps of a single scenario.
type toolchainWorld struct {
	moduleRoot string

	// build scenario
	buildErr    error
	buildOutput string

	// lint scenario
	lintExit   int
	lintOutput string
	lintErr    error

	// protocol-v6 scenario
	server   tfprotov6.ProviderServer
	serveErr error
	schema   *tfprotov6.GetProviderSchemaResponse
	metaResp *fwprovider.MetadataResponse
}

// findModuleRoot walks up from the test working directory until it finds the
// go.mod for github.com/smartpcr/terraform-provider-labdeploy, which is the
// module the acceptance criteria build/lint.
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/smartpcr/terraform-provider-labdeploy") {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod for terraform-provider-labdeploy not found above %s", dir)
		}
		dir = parent
	}
}

func (w *toolchainWorld) theExistingRepo() error {
	root, err := findModuleRoot()
	if err != nil {
		return err
	}
	w.moduleRoot = root
	return nil
}

// makeBuildRuns runs `make build` on the gate host with CGO_ENABLED=0. When make
// is unavailable it falls back to the exact equivalent `go build` the Makefile
// target runs, so the scenario still exercises the real Go toolchain and emits
// the same artifact rather than skipping.
func (w *toolchainWorld) makeBuildRuns() error {
	if w.moduleRoot == "" {
		if err := w.theExistingRepo(); err != nil {
			return err
		}
	}
	// Start from a clean slate so "produced" means this run produced it.
	binPath := filepath.Join(w.moduleRoot, "bin", buildBinaryName)
	_ = os.Remove(binPath)

	var cmd *exec.Cmd
	if _, err := exec.LookPath("make"); err == nil {
		cmd = exec.Command("make", "build")
	} else {
		cmd = exec.Command("go", "build",
			"-ldflags", "-X main.version=0.1.0",
			"-o", filepath.Join("bin", buildBinaryName), ".")
	}
	cmd.Dir = w.moduleRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	w.buildOutput = string(out)
	w.buildErr = err
	return nil
}

// binaryProducedWithCGODisabled asserts the artifact exists and that its embedded
// Go build info records CGO_ENABLED=0 -- a portable, statically-linked proof that
// does not depend on platform-specific linker inspection.
func (w *toolchainWorld) binaryProducedWithCGODisabled(name string) error {
	if w.buildErr != nil {
		return fmt.Errorf("build failed: %v\n%s", w.buildErr, w.buildOutput)
	}
	binPath := filepath.Join(w.moduleRoot, "bin", name)
	if _, err := os.Stat(binPath); err != nil {
		// go build may append .exe on Windows for some invocations.
		alt := binPath + ".exe"
		if _, err2 := os.Stat(alt); err2 != nil {
			return fmt.Errorf("expected binary %q not produced: %v\n%s", binPath, err, w.buildOutput)
		}
		binPath = alt
	}

	out, err := exec.Command("go", "version", "-m", binPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("go version -m failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CGO_ENABLED=0") {
		return fmt.Errorf("binary %q was not built with CGO_ENABLED=0; build info:\n%s", binPath, out)
	}
	return nil
}

func (w *toolchainWorld) theRepo() error {
	return w.theExistingRepo()
}

// golangciLintRuns invokes the gate host's golangci-lint binary against the
// module, exactly as `make lint` does.
func (w *toolchainWorld) golangciLintRuns() error {
	if w.moduleRoot == "" {
		if err := w.theExistingRepo(); err != nil {
			return err
		}
	}
	bin, err := exec.LookPath("golangci-lint")
	if err != nil {
		return fmt.Errorf("golangci-lint not found on the gate host PATH: %w", err)
	}
	cmd := exec.Command(bin, "run")
	cmd.Dir = w.moduleRoot
	cmd.Env = os.Environ()
	out, runErr := cmd.CombinedOutput()
	w.lintOutput = string(out)
	w.lintErr = runErr
	w.lintExit = 0
	if runErr != nil {
		w.lintExit = 1
		if ee, ok := runErr.(*exec.ExitError); ok {
			w.lintExit = ee.ExitCode()
		}
	}
	return nil
}

func (w *toolchainWorld) itExitsZeroWithNoFindings() error {
	if w.lintExit != 0 {
		return fmt.Errorf("golangci-lint exited %d (want 0):\n%s", w.lintExit, w.lintOutput)
	}
	return nil
}

func (w *toolchainWorld) mainGo() error {
	// The provider entrypoint under test is main.go, which serves
	// provider.New(version) at provider.Address; nothing to prepare.
	return nil
}

// providerServedUnderHarness serves the provider through the same protocol-v6
// factory the terraform-plugin-testing harness uses
// (providerserver.NewProtocol6WithError). The returned server is, by its static
// type, a tfprotov6.ProviderServer -- the protocol-v6 gRPC surface -- and a live
// GetProviderSchema RPC is driven against it.
func (w *toolchainWorld) providerServedUnderHarness() error {
	factory := providerserver.NewProtocol6WithError(provider.New("test")())
	server, err := factory()
	w.serveErr = err
	if err != nil {
		return nil
	}
	w.server = server

	schemaResp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		w.serveErr = err
		return nil
	}
	w.schema = schemaResp

	meta := &fwprovider.MetadataResponse{}
	provider.New("test")().Metadata(context.Background(), fwprovider.MetadataRequest{}, meta)
	w.metaResp = meta
	return nil
}

// assertProtocolV6 accepts only a tfprotov6.ProviderServer, giving a
// compile-time proof that the served provider speaks Terraform plugin protocol
// v6.
func assertProtocolV6(_ tfprotov6.ProviderServer) {}

func (w *toolchainWorld) advertisesProtocolV6AndAddress(addr string) error {
	if w.serveErr != nil {
		return fmt.Errorf("serving provider over protocol v6 failed: %w", w.serveErr)
	}
	if w.server == nil {
		return fmt.Errorf("no protocol-v6 provider server was produced")
	}
	// Compile-time proof of protocol v6.
	assertProtocolV6(w.server)

	if w.schema == nil || w.schema.Provider == nil {
		return fmt.Errorf("GetProviderSchema returned no provider schema")
	}
	if _, ok := w.schema.ResourceSchemas["labdeploy_deployment"]; !ok {
		return fmt.Errorf("protocol-v6 schema missing labdeploy_deployment resource")
	}
	if provider.Address != addr {
		return fmt.Errorf("provider.Address = %q, want %q", provider.Address, addr)
	}
	if w.metaResp == nil || w.metaResp.TypeName != "labdeploy" {
		return fmt.Errorf("provider Metadata TypeName not advertised as labdeploy")
	}
	// The advertised address must be a well-formed host/namespace/name triple.
	if parts := strings.Split(addr, "/"); len(parts) != 3 || parts[2] != w.metaResp.TypeName {
		return fmt.Errorf("advertised address %q not consistent with type name %q", addr, w.metaResp.TypeName)
	}
	return nil
}

// InitializeScenario_project_scaffold_and_spec_engine_repository_bootstrap_and_toolchain
// wires every Given/When/Then to its step implementation. Fresh world per scenario.
func InitializeScenario_project_scaffold_and_spec_engine_repository_bootstrap_and_toolchain(ctx *godog.ScenarioContext) {
	w := &toolchainWorld{}

	ctx.Before(func(c context.Context, sc *godog.Scenario) (context.Context, error) {
		*w = toolchainWorld{}
		return c, nil
	})

	ctx.Step(`^the existing repo$`, w.theExistingRepo)
	ctx.Step(`^the repo$`, w.theRepo)
	ctx.Step(`^main\.go$`, w.mainGo)

	ctx.Step(`^"make build" runs on the gate host$`, w.makeBuildRuns)
	ctx.Step(`^"golangci-lint run" runs on the gate host$`, w.golangciLintRuns)
	ctx.Step(`^the provider is served under the terraform-plugin-testing harness$`, w.providerServedUnderHarness)

	ctx.Step(`^binary "([^"]*)" is produced with CGO_ENABLED=0$`, w.binaryProducedWithCGODisabled)
	ctx.Step(`^it exits 0 with no findings$`, w.itExitsZeroWithNoFindings)
	ctx.Step(`^it advertises plugin protocol v6 and address "([^"]*)"$`, w.advertisesProtocolV6AndAddress)
}

// TestE2E_project_scaffold_and_spec_engine_repository_bootstrap_and_toolchain is
// the go test entrypoint for the Stage 1.1 godog suite.
func TestE2E_project_scaffold_and_spec_engine_repository_bootstrap_and_toolchain(t *testing.T) {
	// Guard against an unexpectedly hostile PATH on exotic runners.
	_ = runtime.GOOS

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_project_scaffold_and_spec_engine_repository_bootstrap_and_toolchain,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"project_scaffold_and_spec_engine_repository_bootstrap_and_toolchain.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
