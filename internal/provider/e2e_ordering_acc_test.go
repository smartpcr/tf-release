package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// e2eOrderingConfig wires an labdeploy_e2e_test whose deployment_id references
// labdeploy_deployment.dep.id. That reference makes Terraform Core build a
// dependency edge that orders the e2e_test create strictly AFTER the deployment
// create (DESIGN §5.3; implementation-plan.md:404 scenario). e2eDestDir is where
// the faked e2e run writes its (empty) results tree so the apply is hermetic.
func e2eOrderingConfig(e2eDestDir string) string {
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
`, strings.ReplaceAll(e2eDestDir, `\`, `/`))

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

// createRecorder records, in Terraform-Core execution order, which resource's
// engine actually ran during apply. It is the observable proof of apply ordering:
// the deployment engine records "deployment" the instant Core invokes the
// deployment's Create, and the e2e transport records "e2e" the instant Core
// invokes the e2e_test's Create (which dials the target). If — and only if — the
// deployment_id reference made Core order the two, the recorded sequence is
// exactly ["deployment", "e2e"].
type createRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *createRecorder) record(who string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, who)
}

func (r *createRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// recordingDeployEngine is a fake deployEngine (DESIGN §17 seam) that records
// "deployment" when Core invokes the deployment resource's Create → engine.Update,
// then returns a benign, self-consistent Status so apply proceeds AND the
// post-apply refresh (ReadStatus) yields an identical state (empty plan). Only
// Update records, so the recorder captures the CREATE ordering, not refresh reads.
type recordingDeployEngine struct{ rec *createRecorder }

func okStatus(d *spec.Deployment) *engine.Status {
	return &engine.Status{
		DeployedVersion: d.Artifact.Version,
		ReleasePath:     "C:/labdeploy/releases/" + d.Artifact.Version,
		ServiceStatus:   "running",
		Hosts:           d.Target.Hosts,
	}
}

func (e recordingDeployEngine) Update(_ context.Context, d *spec.Deployment, _ *spec.Deployment) (*engine.Status, error) {
	e.rec.record("deployment")
	return okStatus(d), nil
}
func (e recordingDeployEngine) ReadStatus(_ context.Context, d *spec.Deployment) (*engine.Status, error) {
	return okStatus(d), nil
}
func (e recordingDeployEngine) Destroy(context.Context, *spec.Deployment, string) error { return nil }
func (e recordingDeployEngine) Warns() []string                                         { return nil }

// recordingE2ETransport is a fake transport.Transport that records "e2e" the
// instant the e2e resource's Create → engine.RunTest dials the target (Connect),
// then answers every probe so a `results: { format: none }` run passes (exit 0):
// the cached-release marker probe returns a marker matching the spec's
// version+checksum (skips fetch/extract), and every other command exits 0. No
// network, no lab infra — the real engine + provider Create paths run in-process.
type recordingE2ETransport struct {
	rec        *createRecorder
	markerJSON string
	once       sync.Once
}

func (t *recordingE2ETransport) Connect(context.Context) error {
	t.once.Do(func() { t.rec.record("e2e") })
	return nil
}
func (t *recordingE2ETransport) Close() error    { return nil }
func (t *recordingE2ETransport) OS() spec.OSKind { return spec.OSLinux }
func (t *recordingE2ETransport) Host() string    { return "lab-01" }
func (t *recordingE2ETransport) Exec(_ context.Context, c transport.Cmd) (transport.Result, error) {
	if strings.Contains(c.Script, releaseMarkerFile) && strings.Contains(c.Script, "base64") {
		return transport.Result{ExitCode: 0, Stdout: base64Std(t.markerJSON)}, nil
	}
	return transport.Result{ExitCode: 0}, nil
}
func (t *recordingE2ETransport) Upload(context.Context, io.Reader, int64, string) error { return nil }
func (t *recordingE2ETransport) Download(context.Context, string, string) error         { return nil }

var _ transport.Transport = (*recordingE2ETransport)(nil)

const releaseMarkerFile = ".labdeploy-release.json"

// orderingRecordProvider wraps the REAL provider (delegating Metadata/Schema/
// Configure/DataSources) but overrides Resources() so both resources are built
// with the recording engine seams above. Served through the terraform-plugin-
// testing harness, a real `terraform` binary drives a genuine apply against it.
type orderingRecordProvider struct {
	inner  provider.Provider
	rec    *createRecorder
	marker string
}

func (p *orderingRecordProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	p.inner.Metadata(ctx, req, resp)
}
func (p *orderingRecordProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	p.inner.Schema(ctx, req, resp)
}
func (p *orderingRecordProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	p.inner.Configure(ctx, req, resp)
}
func (p *orderingRecordProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return p.inner.DataSources(ctx)
}
func (p *orderingRecordProvider) Resources(_ context.Context) []func() fwresource.Resource {
	return []func() fwresource.Resource{
		func() fwresource.Resource {
			return &DeploymentResource{newEngine: func() deployEngine { return recordingDeployEngine{rec: p.rec} }}
		},
		func() fwresource.Resource {
			return &E2ETestResource{newEngine: func() *engine.Engine {
				e := engine.New()
				e.NewTransport = func(*spec.Target, string) (transport.Transport, error) {
					return &recordingE2ETransport{rec: p.rec, markerJSON: p.marker}, nil
				}
				return e
			}}
		},
	}
}

// TestAccE2EDeploymentIDOrdersAfterDeployment is the implementation-plan.md:404
// "deployment_id ordering under apply" proof [proof: service:tf-plugin-server].
// It runs the BUILT provider under a real `terraform` binary via the
// terraform-plugin-testing harness (proto v6 reattach) and performs a genuine
// `terraform apply` of a config where
// `labdeploy_e2e_test.smoke.deployment_id = labdeploy_deployment.dep.id`.
//
// Both resources' engines are faked to RECORD the moment Terraform Core invokes
// each resource's Create (deployment engine.Update ⇒ "deployment"; e2e transport
// Connect ⇒ "e2e") and to otherwise SUCCEED with no lab infra. After the apply
// completes, the check asserts the recorded Create sequence is exactly
// ["deployment", "e2e"] — i.e. Core ran the deployment Create strictly BEFORE the
// e2e_test Create, which is only possible because the deployment_id reference
// created the dependency edge. This observes BOTH Create calls and compares their
// order (the previously-missing assertion), not merely plan-time unknown-value
// propagation.
//
// It self-skips unless TF_ACC=1 and a terraform binary are available
// (resource.Test enforces this), so it never affects the plain `go test` gate.
func TestAccE2EDeploymentIDOrdersAfterDeployment(t *testing.T) {
	host, namespace, name := splitAddress(t, Address)
	if name != "labdeploy" {
		t.Fatalf("Address type name = %q, want %q", name, "labdeploy")
	}
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	t.Setenv("LABDEPLOY_PASSWORD", "pw")

	rec := &createRecorder{}
	markerBytes, _ := json.Marshal(map[string]string{
		"version":      "1.0.0",
		"sha256":       "sha256:" + strings.Repeat("0", 64),
		"extracted_at": "2024-01-01T00:00:00Z",
	})
	destDir := t.TempDir()

	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"labdeploy": providerserver.NewProtocol6WithError(&orderingRecordProvider{
			inner:  New("test")(),
			rec:    rec,
			marker: string(markerBytes),
		}),
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: e2eOrderingConfig(destDir),
				Check: resource.ComposeAggregateTestCheckFunc(
					func(_ *terraform.State) error {
						got := rec.snapshot()
						if len(got) != 2 || got[0] != "deployment" || got[1] != "e2e" {
							return fmt.Errorf("apply Create order = %v, want [deployment e2e] "+
								"(deployment must be created strictly before the e2e_test)", got)
						}
						return nil
					},
					// Corroborate that both resources really landed in state with the
					// wired dependency (deployment_id == the deployment's id).
					resource.TestCheckResourceAttrPair(
						"labdeploy_e2e_test.smoke", "deployment_id",
						"labdeploy_deployment.dep", "id"),
				),
			},
		},
	})
}

// base64Std mirrors the linux `base64` encoding readSmallFile decodes when it
// reads the on-target release marker through this fake transport.
func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
