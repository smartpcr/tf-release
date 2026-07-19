package provider

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// e2eFakeTransport drives E2ETestResource.Create end-to-end in-process: it answers
// the release-marker probe so RunTest takes the cached fast-path, returns a
// SCRIPTED failing runner Result, and serves an on-target results archive back
// through Download so collection materializes a real results tree — no network.
type e2eFakeTransport struct {
	archiveSrc string
	markerJSON string
}

func (f *e2eFakeTransport) Connect(context.Context) error { return nil }
func (f *e2eFakeTransport) Close() error                  { return nil }
func (f *e2eFakeTransport) OS() spec.OSKind               { return spec.OSLinux }
func (f *e2eFakeTransport) Host() string                  { return "lab-01" }

func (f *e2eFakeTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	switch {
	case strings.Contains(c.Script, ".labdeploy-release.json") && strings.Contains(c.Script, "base64"):
		return transport.Result{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(f.markerJSON))}, nil
	case strings.Contains(c.Script, "kill -9 -$__pgid"):
		return transport.Result{ExitCode: 1}, nil // tests failed
	case strings.Contains(c.Script, "labdeploy-logs-"):
		return transport.Result{ExitCode: 0, Stdout: "packed"}, nil
	default:
		return transport.Result{ExitCode: 0}, nil
	}
}

func (f *e2eFakeTransport) Upload(ctx context.Context, rd io.Reader, size int64, remote string) error {
	return nil
}

func (f *e2eFakeTransport) Download(ctx context.Context, remote, local string) error {
	if !strings.Contains(remote, "labdeploy-logs-") || f.archiveSrc == "" {
		return nil
	}
	in, err := os.Open(f.archiveSrc)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	out, err := os.Create(local)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

var _ transport.Transport = (*e2eFakeTransport)(nil)

func e2eMakeTarGz(t *testing.T, dst, name string, content []byte) {
	t.Helper()
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestE2ECreateCollectsBeforeFailErr is the PROVIDER-level acceptance proof for
// "Collection before failure" (implementation-plan.md:401): E2ETestResource.Create
// is exercised with fail_on_test_failure=true against a fake transport serving the
// on-target results archive. It asserts (a) Create returns an ERR_TEST_FAILED
// diagnostic, AND (b) the results tree + summary.json ARE fully persisted and the
// computed outputs are in state — i.e. the failure is surfaced only AFTER the
// artifacts exist, never instead of collecting them.
func TestE2ECreateCollectsBeforeFailErr(t *testing.T) {
	ctx := context.Background()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")

	dest := t.TempDir()
	checksum := "sha256:" + strings.Repeat("a", 64)
	golden := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="e2e-smoke" tests="4" failures="1" errors="0" skipped="1" time="1.50">
  <testcase name="a" classname="S" time="0.20"/>
  <testcase name="b" classname="S" time="0.30"/>
  <testcase name="c" classname="S" time="0.40"><failure message="boom">x</failure></testcase>
  <testcase name="d" classname="S" time="0.10"><skipped/></testcase>
</testsuite>
`)
	archive := filepath.Join(t.TempDir(), "results.tar.gz")
	e2eMakeTarGz(t, archive, "TestResults/results.xml", golden)

	marker, _ := json.Marshal(map[string]string{"version": "1.2.3", "sha256": checksum, "extracted_at": "2024-01-01T00:00:00Z"})

	specYAML := fmt.Sprintf(`apiVersion: labdeploy/v1
kind: TestRun
metadata: { name: smoke }
target:
  transport: ssh
  os: linux
  hosts: ["lab-01"]
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.2.3
  checksum: "%s"
  source: { type: http, url: "http://example.test/pkg.zip" }
runner:
  type: exec
  command: ./run-tests.sh
  timeout_seconds: 600
results:
  format: junit
  paths: ["TestResults/*.xml"]
collect:
  destination_dir: '%s'
pass_criteria: { exit_codes: [0] }
`, checksum, dest)

	r := &E2ETestResource{
		newEngine: func() *engine.Engine {
			e := engine.New()
			e.NewTransport = func(*spec.Target, string) (transport.Transport, error) {
				return &e2eFakeTransport{archiveSrc: archive, markerJSON: string(marker)}, nil
			}
			return e
		},
	}

	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	plan := &e2eModel{
		Spec:              types.StringValue(specYAML),
		SpecFile:          types.StringNull(),
		Variables:         types.MapNull(types.StringType),
		DeploymentID:      types.StringNull(),
		Triggers:          types.MapNull(types.StringType),
		FailOnTestFailure: types.BoolValue(true),
	}
	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		t.Fatalf("build plan: %v", d)
	}
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sr.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: p}, resp)

	// (a) The provider surfaces a coded test-failure error.
	if !resp.Diagnostics.HasError() {
		t.Fatalf("Create must fail with ERR_TEST_FAILED when tests fail and fail_on_test_failure=true; diags=%v", resp.Diagnostics)
	}
	var sawCoded bool
	for _, d := range resp.Diagnostics.Errors() {
		if strings.Contains(d.Summary(), "ERR_TEST_FAILED") {
			sawCoded = true
		}
	}
	if !sawCoded {
		t.Fatalf("expected an [ERR_TEST_FAILED] diagnostic summary, got %v", resp.Diagnostics.Errors())
	}

	// (b) Artifacts were fully persisted BEFORE the error: results tree + summary.
	if _, err := os.Stat(filepath.Join(dest, "summary.json")); err != nil {
		t.Fatalf("summary.json not persisted before failure surfaced: %v", err)
	}
	resultsXML := filepath.Join(dest, "results", "lab-01", "TestResults", "results.xml")
	got, err := os.ReadFile(resultsXML)
	if err != nil {
		t.Fatalf("results tree not persisted before failure surfaced: %v", err)
	}
	if string(got) != string(golden) {
		t.Fatalf("persisted results not byte-identical to the served archive")
	}

	// (b cont.) Computed outcomes are in state so a failed apply still records them.
	var state e2eModel
	if d := resp.State.Get(ctx, &state); d.HasError() {
		t.Fatalf("read state: %v", d)
	}
	if state.Passed.ValueBool() {
		t.Fatal("state passed = true, want false")
	}
	if state.TotalTests.ValueInt64() != 4 || state.FailedTests.ValueInt64() != 1 {
		t.Fatalf("state counters total=%d failed=%d, want 4/1", state.TotalTests.ValueInt64(), state.FailedTests.ValueInt64())
	}
	if state.ResultsDir.ValueString() != dest {
		t.Fatalf("state results_dir = %q, want %q", state.ResultsDir.ValueString(), dest)
	}
}
