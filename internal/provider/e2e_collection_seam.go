//go:build e2e

package provider

// This file is compiled ONLY under `-tags e2e`. It exposes narrow, behaviour-
// preserving seams around the (unexported) E2ETestResource / DeploymentResource
// engine boundaries so the out-of-package godog acceptance suite under
// test/e2e can drive the REAL impl for Stage 7.2 (E2E Test Resource and
// Collection) without duplicating it. Production builds never see these
// symbols. Identifiers are prefixed `ordSeam*` so they never collide with the
// same-package `_test.go` fakes that also compile under `go test -tags e2e`.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// NewE2ETestResourceWithEngine returns a REAL E2ETestResource whose engine
// factory is the caller-supplied newEng. Driving Create/Delete against it runs
// the GENUINE provider + engine.RunTest collection/pass-gating paths (DESIGN
// §5.3, §7.4) against a scripted fake transport, so the persisted results_dir
// tree and the ERR_TEST_FAILED diagnostic are produced by the impl — nothing is
// injected past the engine's public NewTransport/Now seam (engine.go §17).
func NewE2ETestResourceWithEngine(newEng func() *engine.Engine) *E2ETestResource {
	return &E2ETestResource{newEngine: newEng}
}

// --- deployment_id ordering-under-apply seam ---------------------------------

// ordSeamRecorder records, in Terraform-Core execution order, which resource's
// engine actually ran during apply — the observable proof of apply ordering.
type ordSeamRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *ordSeamRecorder) record(who string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, who)
}

func (r *ordSeamRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// ordSeamDeployEngine is a fake deployEngine that records "deployment" the
// instant Core invokes the deployment resource's Create → engine.Update, then
// returns a benign, self-consistent Status so apply proceeds AND the post-apply
// refresh yields an identical (empty-diff) state.
type ordSeamDeployEngine struct{ rec *ordSeamRecorder }

func ordSeamOKStatus(d *spec.Deployment) *engine.Status {
	return &engine.Status{
		DeployedVersion: d.Artifact.Version,
		ReleasePath:     "C:/labdeploy/releases/" + d.Artifact.Version,
		ServiceStatus:   "running",
		Hosts:           d.Target.Hosts,
	}
}

func (e ordSeamDeployEngine) Update(_ context.Context, d *spec.Deployment, _ *spec.Deployment) (*engine.Status, error) {
	e.rec.record("deployment")
	return ordSeamOKStatus(d), nil
}
func (e ordSeamDeployEngine) ReadStatus(_ context.Context, d *spec.Deployment) (*engine.Status, error) {
	return ordSeamOKStatus(d), nil
}
func (e ordSeamDeployEngine) Destroy(context.Context, *spec.Deployment, string) error { return nil }
func (e ordSeamDeployEngine) Warns() []string                                         { return nil }

// ordSeamE2ETransport records "e2e" the instant the e2e resource's Create →
// engine.RunTest dials the target (Connect), then answers every probe so a
// `results: { format: none }` run passes (exit 0): the release-marker probe
// returns a marker matching the spec's version+checksum (cached fast-path skips
// fetch/extract), every other command exits 0. No network, no lab infra.
type ordSeamE2ETransport struct {
	rec        *ordSeamRecorder
	markerJSON string
	once       sync.Once
}

func (t *ordSeamE2ETransport) Connect(context.Context) error {
	t.once.Do(func() { t.rec.record("e2e") })
	return nil
}
func (t *ordSeamE2ETransport) Close() error    { return nil }
func (t *ordSeamE2ETransport) OS() spec.OSKind { return spec.OSLinux }
func (t *ordSeamE2ETransport) Host() string    { return "lab-01" }
func (t *ordSeamE2ETransport) Exec(_ context.Context, c transport.Cmd) (transport.Result, error) {
	if strings.Contains(c.Script, ".labdeploy-release.json") && strings.Contains(c.Script, "base64") {
		return transport.Result{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(t.markerJSON))}, nil
	}
	return transport.Result{ExitCode: 0}, nil
}
func (t *ordSeamE2ETransport) Upload(context.Context, io.Reader, int64, string) error { return nil }
func (t *ordSeamE2ETransport) Download(context.Context, string, string) error         { return nil }

var _ transport.Transport = (*ordSeamE2ETransport)(nil)

// ordSeamProvider wraps the REAL provider (delegating Metadata/Schema/Configure/
// DataSources) but overrides Resources() so both resources are built with the
// recording seams above. Served through the plugin-testing harness, a real
// `terraform` binary drives a genuine apply against it.
type ordSeamProvider struct {
	inner  provider.Provider
	rec    *ordSeamRecorder
	marker string
}

func (p *ordSeamProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	p.inner.Metadata(ctx, req, resp)
}
func (p *ordSeamProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	p.inner.Schema(ctx, req, resp)
}
func (p *ordSeamProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	p.inner.Configure(ctx, req, resp)
}
func (p *ordSeamProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return p.inner.DataSources(ctx)
}
func (p *ordSeamProvider) Resources(_ context.Context) []func() fwresource.Resource {
	return []func() fwresource.Resource{
		func() fwresource.Resource {
			return &DeploymentResource{newEngine: func() deployEngine { return ordSeamDeployEngine{rec: p.rec} }}
		},
		func() fwresource.Resource {
			return &E2ETestResource{newEngine: func() *engine.Engine {
				e := engine.New()
				e.NewTransport = func(*spec.Target, string) (transport.Transport, error) {
					return &ordSeamE2ETransport{rec: p.rec, markerJSON: p.marker}, nil
				}
				return e
			}}
		},
	}
}

// NewE2EOrderingRecorder returns a provider.Provider whose two resources RECORD
// the moment Terraform Core invokes each resource's Create (deployment
// engine.Update ⇒ "deployment"; e2e transport Connect ⇒ "e2e") and otherwise
// SUCCEED with no lab infra, plus a snapshot func returning the recorded order.
// The godog suite serves this through the terraform-plugin-testing harness and a
// real `terraform` binary to prove `deployment_id` orders the e2e create after
// the deployment create (DESIGN §5.3; implementation-plan.md:404).
func NewE2EOrderingRecorder() (provider.Provider, func() []string) {
	rec := &ordSeamRecorder{}
	markerBytes, _ := json.Marshal(map[string]string{
		"version":      "1.0.0",
		"sha256":       "sha256:" + strings.Repeat("0", 64),
		"extracted_at": "2024-01-01T00:00:00Z",
	})
	p := &ordSeamProvider{inner: New("test")(), rec: rec, marker: string(markerBytes)}
	return p, rec.snapshot
}

// E2EOrderingConfig wires an labdeploy_e2e_test whose deployment_id references
// labdeploy_deployment.dep.id. That reference makes Terraform Core build a
// dependency edge that orders the e2e_test create strictly AFTER the deployment
// create. destDir is where the faked e2e run writes its (empty) results tree so
// the apply is hermetic.
func E2EOrderingConfig(destDir string) string {
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

	e2eSpec := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: TestRun
metadata: { name: smoke }
target:
  transport: ssh
  hosts: ["lab-01"]
  os: linux
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
collect:
  destination_dir: '%s'
pass_criteria: { exit_codes: [0] }
`, strings.ReplaceAll(destDir, `\`, `/`))

	providerCfg := `
terraform {
  required_providers {
    labdeploy = {
      source = "registry.local/smartpcr/labdeploy"
    }
  }
}

provider "labdeploy" {}
`

	return providerCfg + fmt.Sprintf(`
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
