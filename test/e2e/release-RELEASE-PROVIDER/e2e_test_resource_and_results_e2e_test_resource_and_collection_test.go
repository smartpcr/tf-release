//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 7.2 (E2E Test
// Resource and Collection). Every scenario exercises the REAL impl:
//
//   - Collection before failure -> the REAL provider E2ETestResource.Create runs
//     against a fake transport (engine.NewTransport seam) that serves the
//     on-target results + logs archives; with a frozen clock the persisted
//     results_dir tree (results + logs + summary.json) is asserted BYTE-IDENTICAL
//     to the committed golden snapshot under internal/engine/testdata, and the
//     ERR_TEST_FAILED diagnostic is surfaced only AFTER that tree is fully
//     written (proof: golden; DESIGN §5.3, §7.4; E2E-05).
//   - Timeout kills tree -> the REAL engine.RunTest runs against a fake transport
//     whose scripted runner Result carries the watchdog timeout sentinel+marker;
//     it returns ERR_TIMEOUT and the runner script it issued force-kills the whole
//     process TREE (kill -9 -<pgid>) (proof: in-process; DESIGN §5.3; E2E-04).
//   - deployment_id wires a reference without replacement -> the REAL
//     labdeploy_e2e_test schema exposes deployment_id as a settable string
//     attribute with NO RequiresReplace plan modifier (proof: in-process).
//   - deployment_id ordering under apply -> the BUILT provider is served through
//     the terraform-plugin-testing harness and a real `terraform` binary runs a
//     genuine apply of a config where deployment_id references the deployment id;
//     the recorded Create order is exactly [deployment, e2e] (proof:
//     service:tf-plugin-server).
package e2e

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// Stable engine constants replicated for the out-of-package suite (values are
// pinned by internal/engine/testrun.go and asserted there): the watchdog
// timeout sentinel exit code and the stdout/stderr marker it emits, plus the
// on-target release-marker filename the cached fast-path probes.
const (
	collectTimeoutSentinel = 124
	collectTimeoutMarker   = "__LABDEPLOY_TIMEOUT__"
	collectReleaseMarker   = ".labdeploy-release.json"
)

// e2eCollectT is the top-level *testing.T threaded from the suite entrypoint so
// the ordering scenario can drive the terraform-plugin-testing harness (which
// requires a *testing.T). Set once before the suite runs; unique to this stage.
var e2eCollectT *testing.T

// collectGoldenClock is the deterministic instant injected into the engine for
// the byte-identical golden proof: with started==finished the summary.json's
// started_utc/finished_utc are fixed and duration_seconds is 0, so the WHOLE
// persisted tree (including summary.json) can be byte-compared with NO masking.
func collectGoldenClock() time.Time { return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) }

// collectModuleRoot walks up from this source file to the module root so the
// golden-fixture scenario can read internal/engine/testdata regardless of the
// working directory go test chooses.
func collectModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found above %s", file)
}

// collectFakeTransport drives the WHOLE RunTest flow in-process (DESIGN §17
// seam): it answers the staging/marker probes so the cached fast-path is taken,
// records the runner watchdog script, returns a SCRIPTED runner Result, and
// serves the on-target results/logs archives (real .tar.gz) back through
// Download so collection extracts a real tree. No network, no external service.
type collectFakeTransport struct {
	osKind        spec.OSKind
	host          string
	markerJSON    string // returned for the release-marker read ⇒ cached=true
	runnerRes     transport.Result
	archiveSrc    string   // local file whose bytes are served as the RESULTS archive
	logsSrc       string   // local file whose bytes are served as the extra-LOGS archive
	runnerScripts []string // recorded runner watchdog scripts (process-tree kill wrappers)
}

func (f *collectFakeTransport) Connect(context.Context) error { return nil }
func (f *collectFakeTransport) Close() error                  { return nil }
func (f *collectFakeTransport) OS() spec.OSKind               { return f.osKind }
func (f *collectFakeTransport) Host() string                  { return f.host }

func (f *collectFakeTransport) Exec(_ context.Context, c transport.Cmd) (transport.Result, error) {
	switch {
	case strings.Contains(c.Script, collectReleaseMarker) &&
		(strings.Contains(c.Script, "base64") || strings.Contains(c.Script, "Base64")):
		// release-marker read ⇒ report the wanted version is already extracted.
		return transport.Result{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(f.markerJSON))}, nil
	case strings.Contains(c.Script, "kill -9 -$__pgid") || strings.Contains(c.Script, "taskkill /PID"):
		// The runner watchdog script — the process-tree kill wrapper.
		f.runnerScripts = append(f.runnerScripts, c.Script)
		return f.runnerRes, nil
	case strings.Contains(c.Script, "labdeploy-logs-"):
		// The collection archive probe: report files present so CollectFiles
		// proceeds to Download.
		return transport.Result{ExitCode: 0, Stdout: "packed"}, nil
	default:
		return transport.Result{ExitCode: 0}, nil
	}
}

func (f *collectFakeTransport) Upload(context.Context, io.Reader, int64, string) error { return nil }

func (f *collectFakeTransport) Download(_ context.Context, remote, local string) error {
	if !strings.Contains(remote, "labdeploy-logs-") {
		return nil
	}
	// CollectFiles downloads into <destDir>/<host>/logs.tar.gz where destDir is
	// dest/results for the results pull and dest/logs for the extra-logs pull.
	// The grandparent dir name ("results" | "logs") selects which archive to
	// serve, so results and logs snapshot DISTINCT trees.
	which := filepath.Base(filepath.Dir(filepath.Dir(local)))
	src := f.archiveSrc
	if which == "logs" && f.logsSrc != "" {
		src = f.logsSrc
	}
	if src == "" {
		return nil
	}
	in, err := os.Open(src)
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

var _ transport.Transport = (*collectFakeTransport)(nil)

// collectMakeTarGz writes a single-entry .tar.gz at dst carrying content at the
// given in-archive path, mimicking the on-target archive the runner produced.
func collectMakeTarGz(dst, arcName string, content []byte) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: arcName, Mode: 0o644, Size: int64(len(content))}); err != nil {
		return err
	}
	if _, err := tw.Write(content); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// collectRelFileSet returns the set of slash-normalized file paths (relative to
// root) for every regular file beneath root, so two trees can be compared for
// exact set-equality in both directions.
func collectRelFileSet(root string) (map[string]bool, error) {
	set := map[string]bool{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return werr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		set[filepath.ToSlash(rel)] = true
		return nil
	})
	return set, err
}

// e2ePlanModel mirrors the (unexported) provider e2eModel tfsdk shape so the
// out-of-package suite can build a Create plan against the REAL schema.
type e2ePlanModel struct {
	ID                types.String `tfsdk:"id"`
	Spec              types.String `tfsdk:"spec"`
	SpecFile          types.String `tfsdk:"spec_file"`
	Variables         types.Map    `tfsdk:"variables"`
	DeploymentID      types.String `tfsdk:"deployment_id"`
	Triggers          types.Map    `tfsdk:"triggers"`
	FailOnTestFailure types.Bool   `tfsdk:"fail_on_test_failure"`
	Passed            types.Bool   `tfsdk:"passed"`
	ExitCode          types.Int64  `tfsdk:"exit_code"`
	TotalTests        types.Int64  `tfsdk:"total_tests"`
	PassedTests       types.Int64  `tfsdk:"passed_tests"`
	FailedTests       types.Int64  `tfsdk:"failed_tests"`
	SkippedTests      types.Int64  `tfsdk:"skipped_tests"`
	ResultsDir        types.String `tfsdk:"results_dir"`
	DurationSeconds   types.Int64  `tfsdk:"duration_seconds"`
	Summary           types.String `tfsdk:"summary"`
}

// collectWorld carries state across the steps of a single scenario.
type collectWorld struct {
	// scenario 1 (collection before failure)
	goldenRoot   string
	dest         string
	resultsArc   string
	logsArc      string
	specYAML     string
	markerJSON   string
	createDiags  fwresource.CreateResponse

	// scenario 2 (timeout kills tree)
	timeoutTR      *spec.TestRun
	timeoutFake    *collectFakeTransport
	runErr         error
	runnerScripts  []string

	// scenario 3 (schema)
	e2eSchema rschema.Schema

	// scenario 4 (ordering under apply)
	orderSnapshot []string
	orderDest     string
}

// --- Scenario 1: Collection before failure -----------------------------------

func (w *collectWorld) givenFailingCreateFixture() error {
	root, err := collectModuleRoot()
	if err != nil {
		return err
	}
	w.goldenRoot = filepath.Join(root, "internal", "engine", "testdata", "e2e_golden_tree")

	resultsXML, err := os.ReadFile(filepath.Join(w.goldenRoot, "results", "lab-01", "TestResults", "results.xml"))
	if err != nil {
		return fmt.Errorf("read golden results: %w", err)
	}
	appLog, err := os.ReadFile(filepath.Join(w.goldenRoot, "logs", "lab-01", "app.log"))
	if err != nil {
		return fmt.Errorf("read golden log: %w", err)
	}

	w.dest, err = os.MkdirTemp("", "e2e-collect-dest")
	if err != nil {
		return err
	}
	arcDir, err := os.MkdirTemp("", "e2e-collect-arc")
	if err != nil {
		return err
	}
	w.resultsArc = filepath.Join(arcDir, "results.tar.gz")
	w.logsArc = filepath.Join(arcDir, "logs.tar.gz")
	if err := collectMakeTarGz(w.resultsArc, "TestResults/results.xml", resultsXML); err != nil {
		return err
	}
	if err := collectMakeTarGz(w.logsArc, "app.log", appLog); err != nil {
		return err
	}

	// checksum is a valid sha256:<64hex> so the YAML passes validation; the fake
	// transport's marker carries the SAME checksum+version ⇒ cached fast-path.
	checksum := "sha256:" + strings.Repeat("a", 64)
	marker, _ := json.Marshal(map[string]string{"version": "1.2.3", "sha256": checksum, "extracted_at": "2024-01-01T00:00:00Z"})
	w.markerJSON = string(marker)

	// LABDEPLOY_PASSWORD must resolve when the spec references it via password_env.
	_ = os.Setenv("LABDEPLOY_PASSWORD", "pw")

	// The spec reproduces the committed golden's TestRun (name/version/host,
	// junit results, /var/log/app.log logs) so the persisted tree matches.
	w.specYAML = fmt.Sprintf(`apiVersion: labdeploy/v1
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
  logs: ["/var/log/app.log"]
pass_criteria: { exit_codes: [0] }
`, checksum, strings.ReplaceAll(w.dest, `\`, `/`))
	return nil
}

func (w *collectWorld) whenCreateRunsFailing() error {
	ctx := context.Background()
	fake := &collectFakeTransport{
		osKind: spec.OSLinux, host: "lab-01", markerJSON: w.markerJSON,
		runnerRes:  transport.Result{ExitCode: 1}, // tests failed, empty stdout/stderr
		archiveSrc: w.resultsArc, logsSrc: w.logsArc,
	}
	r := provider.NewE2ETestResourceWithEngine(func() *engine.Engine {
		e := engine.New()
		e.NewTransport = func(*spec.Target, string) (transport.Transport, error) { return fake, nil }
		e.Now = collectGoldenClock // freeze the clock ⇒ summary.json is deterministic
		return e
	})

	sr := fwresource.SchemaResponse{}
	r.Schema(ctx, fwresource.SchemaRequest{}, &sr)
	if sr.Diagnostics.HasError() {
		return fmt.Errorf("build schema: %v", sr.Diagnostics)
	}
	plan := &e2ePlanModel{
		Spec:              types.StringValue(w.specYAML),
		SpecFile:          types.StringNull(),
		Variables:         types.MapNull(types.StringType),
		DeploymentID:      types.StringNull(),
		Triggers:          types.MapNull(types.StringType),
		FailOnTestFailure: types.BoolValue(true),
	}
	p := tfsdk.Plan{Schema: sr.Schema}
	if d := p.Set(ctx, plan); d.HasError() {
		return fmt.Errorf("build plan: %v", d)
	}
	resp := fwresource.CreateResponse{State: tfsdk.State{Schema: sr.Schema}}
	r.Create(ctx, fwresource.CreateRequest{Plan: p}, &resp)
	w.createDiags = resp
	return nil
}

func (w *collectWorld) thenTreeByteIdenticalToGolden() error {
	goldenSet, err := collectRelFileSet(w.goldenRoot)
	if err != nil {
		return err
	}
	destSet, err := collectRelFileSet(w.dest)
	if err != nil {
		return err
	}
	for rel := range goldenSet {
		gotBytes, err := os.ReadFile(filepath.Join(w.dest, rel))
		if err != nil {
			return fmt.Errorf("expected artifact %q missing from persisted tree: %w", rel, err)
		}
		wantBytes, err := os.ReadFile(filepath.Join(w.goldenRoot, rel))
		if err != nil {
			return fmt.Errorf("read golden %q: %w", rel, err)
		}
		if string(gotBytes) != string(wantBytes) {
			return fmt.Errorf("tree file %q is NOT byte-identical to golden\n--- got ---\n%s\n--- want ---\n%s", rel, gotBytes, wantBytes)
		}
	}
	for rel := range destSet {
		if !goldenSet[rel] {
			return fmt.Errorf("persisted tree has UNEXPECTED file %q not present in the golden snapshot", rel)
		}
	}
	if len(goldenSet) != len(destSet) {
		return fmt.Errorf("tree file-set size mismatch: golden=%d dest=%d", len(goldenSet), len(destSet))
	}
	if len(goldenSet) < 5 {
		return fmt.Errorf("expected the full results+logs+summary tree, only have %d files", len(goldenSet))
	}
	return nil
}

func (w *collectWorld) thenErrTestFailedAfterWrite() error {
	if !w.createDiags.Diagnostics.HasError() {
		return fmt.Errorf("Create must fail with ERR_TEST_FAILED when tests fail and fail_on_test_failure=true; diags=%v", w.createDiags.Diagnostics)
	}
	saw := false
	for _, d := range w.createDiags.Diagnostics.Errors() {
		if strings.Contains(d.Summary(), "ERR_TEST_FAILED") {
			saw = true
		}
	}
	if !saw {
		return fmt.Errorf("expected an [ERR_TEST_FAILED] diagnostic summary, got %v", w.createDiags.Diagnostics.Errors())
	}
	// summary.json is the LAST artifact written by a successful collection, so its
	// presence proves the tree was fully written before the error was surfaced.
	if _, err := os.Stat(filepath.Join(w.dest, "summary.json")); err != nil {
		return fmt.Errorf("summary.json not persisted before failure surfaced: %w", err)
	}
	return nil
}

// --- Scenario 2: Timeout kills tree -------------------------------------------

func (w *collectWorld) givenTimeoutRun() error {
	var err error
	w.dest, err = os.MkdirTemp("", "e2e-timeout-dest")
	if err != nil {
		return err
	}
	marker, _ := json.Marshal(map[string]string{"version": "1.2.3", "sha256": "sha256:abc", "extracted_at": "2024-01-01T00:00:00Z"})
	w.timeoutFake = &collectFakeTransport{
		osKind: spec.OSLinux, host: "lab-01", markerJSON: string(marker),
		// Scripted timeout: watchdog sentinel exit code + marker, no transport error.
		runnerRes: transport.Result{ExitCode: collectTimeoutSentinel, Stdout: collectTimeoutMarker},
	}
	w.timeoutTR = &spec.TestRun{
		APIVersion: "labdeploy/v1", Kind: "TestRun",
		Metadata: spec.Metadata{Name: "smoke"},
		Target: spec.Target{OS: spec.OSLinux, Hosts: []string{"lab-01"},
			Transport: spec.TransportSSH},
		Artifact: spec.Artifact{Version: "1.2.3", Checksum: "sha256:abc"},
		Runner:   spec.Runner{Type: "exec", Command: "./run-tests.sh", TimeoutSeconds: 30},
		Results:  spec.Results{Format: "junit", Paths: []string{"TestResults/*.xml"}},
		Collect:  spec.Collect{DestinationDir: w.dest},
	}
	return nil
}

func (w *collectWorld) whenEngineRunsTest() error {
	e := engine.New()
	e.NewTransport = func(*spec.Target, string) (transport.Transport, error) { return w.timeoutFake, nil }
	var out *engine.TestOutcome
	out, w.runErr = e.RunTest(context.Background(), w.timeoutTR)
	if out != nil {
		return fmt.Errorf("timeout must yield a nil outcome, got %+v", out)
	}
	w.runnerScripts = w.timeoutFake.runnerScripts
	return nil
}

func (w *collectWorld) thenTimeoutAndKillScript() error {
	if w.runErr == nil || !strings.Contains(w.runErr.Error(), "ERR_TIMEOUT") {
		return fmt.Errorf("want ERR_TIMEOUT, got %v", w.runErr)
	}
	if len(w.runnerScripts) != 1 {
		return fmt.Errorf("expected exactly one runner script issued, got %d", len(w.runnerScripts))
	}
	script := w.runnerScripts[0]
	if !strings.Contains(script, "kill -9 -$__pgid") {
		return fmt.Errorf("runner script does not issue a process-tree kill:\n%s", script)
	}
	if !strings.Contains(script, "setsid") {
		return fmt.Errorf("runner script does not create a new process group (setsid):\n%s", script)
	}
	return nil
}

// --- Scenario 3: deployment_id wires a reference without replacement ----------

func (w *collectWorld) givenE2ESchema() error {
	r := provider.NewE2ETestResource()
	sr := &fwresource.SchemaResponse{}
	r.Schema(context.Background(), fwresource.SchemaRequest{}, sr)
	if sr.Diagnostics.HasError() {
		return fmt.Errorf("e2e schema build errors: %v", sr.Diagnostics)
	}
	w.e2eSchema = sr.Schema
	return nil
}

func (w *collectWorld) whenInspectDeploymentID() error {
	if _, ok := w.e2eSchema.Attributes["deployment_id"]; !ok {
		return fmt.Errorf("deployment_id: attribute is absent from the labdeploy_e2e_test schema")
	}
	return nil
}

func (w *collectWorld) thenSettableNoRequiresReplace() error {
	raw := w.e2eSchema.Attributes["deployment_id"]
	attrib, ok := raw.(rschema.StringAttribute)
	if !ok {
		return fmt.Errorf("deployment_id: want schema.StringAttribute, got %T", raw)
	}
	if !attrib.IsOptional() {
		return fmt.Errorf("deployment_id: must be Optional so config can set it to labdeploy_deployment.x.id")
	}
	if attrib.IsRequired() {
		return fmt.Errorf("deployment_id: must NOT be Required (it is a purely-advisory dependency edge)")
	}
	if attrib.GetType() != types.StringType {
		return fmt.Errorf("deployment_id: want string type, got %v", attrib.GetType())
	}
	ctx := context.Background()
	req := planmodifier.StringRequest{
		StateValue:  types.StringValue("labdeploy_deployment.x.id-OLD"),
		PlanValue:   types.StringValue("labdeploy_deployment.x.id-NEW"),
		ConfigValue: types.StringValue("labdeploy_deployment.x.id-NEW"),
	}
	for i, pm := range attrib.PlanModifiers {
		resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		pm.PlanModifyString(ctx, req, resp)
		if resp.RequiresReplace {
			return fmt.Errorf("deployment_id: plan modifier #%d (%s) sets RequiresReplace; the edge must NOT force replacement", i, pm.Description(ctx))
		}
	}
	return nil
}

// --- Scenario 4: deployment_id ordering under apply ---------------------------

func (w *collectWorld) givenOrderingConfig() error {
	if e2eCollectT == nil {
		return fmt.Errorf("no *testing.T threaded from the suite entrypoint")
	}
	return nil
}

func (w *collectWorld) whenApplyInHarness() error {
	t := e2eCollectT
	p, snapshot := provider.NewE2EOrderingRecorder()

	// The plugin-testing harness auto-installs the Terraform CLI via hc-install,
	// but its bundled release-signing key can be expired in a CI image; provision
	// the CLI directly (a plain HTTPS zip, no GPG) and point the harness at it via
	// TF_ACC_TERRAFORM_PATH so the exact-path source is used with no download.
	tfPath, err := ensureTerraformCLI()
	if err != nil {
		return fmt.Errorf("provision terraform CLI: %w", err)
	}

	// Drive the BUILT provider under a real `terraform` binary via the
	// terraform-plugin-testing harness (proto v6 reattach). TF_ACC=1 activates
	// the harness.
	t.Setenv("TF_ACC", "1")
	t.Setenv("TF_ACC_TERRAFORM_PATH", tfPath)
	t.Setenv("TF_ACC_PROVIDER_HOST", "registry.local")
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "smartpcr")
	t.Setenv("LABDEPLOY_PASSWORD", "pw")

	dest, err := os.MkdirTemp("", "e2e-order-dest")
	if err != nil {
		return err
	}
	w.orderDest = dest

	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"labdeploy": providerserver.NewProtocol6WithError(p),
	}

	tfresource.Test(t, tfresource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []tfresource.TestStep{
			{
				Config: provider.E2EOrderingConfig(dest),
				Check: tfresource.ComposeAggregateTestCheckFunc(
					func(_ *terraform.State) error {
						got := snapshot()
						if len(got) != 2 || got[0] != "deployment" || got[1] != "e2e" {
							return fmt.Errorf("apply Create order = %v, want [deployment e2e] "+
								"(deployment must be created strictly before the e2e_test)", got)
						}
						// Capture the apply-time order; the harness auto-destroy that
						// follows would otherwise re-dial the e2e transport (Delete) and
						// append a third record.
						w.orderSnapshot = got
						return nil
					},
					tfresource.TestCheckResourceAttrPair(
						"labdeploy_e2e_test.smoke", "deployment_id",
						"labdeploy_deployment.dep", "id"),
				),
			},
		},
	})

	return nil
}

func (w *collectWorld) thenCoreOrdersE2EAfterDeployment() error {
	got := w.orderSnapshot
	if len(got) != 2 || got[0] != "deployment" || got[1] != "e2e" {
		return fmt.Errorf("recorded Create order = %v, want [deployment e2e]", got)
	}
	return nil
}

// ensureTerraformCLI returns a path to a usable `terraform` binary. It honors an
// already-provisioned CLI (TF_ACC_TERRAFORM_PATH or one on PATH) and otherwise
// downloads a pinned release zip directly over HTTPS (no GPG signature step, so
// it is immune to the hc-install expired-key failure) and extracts it into a
// per-version cache dir. This is the ephemeral, in-process provisioning the
// required-scenario rule endorses for a `service:tf-plugin-server` proof.
func ensureTerraformCLI() (string, error) {
	if p := os.Getenv("TF_ACC_TERRAFORM_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("terraform"); err == nil {
		return p, nil
	}

	const ver = "1.9.8"
	binName := "terraform"
	if runtime.GOOS == "windows" {
		binName = "terraform.exe"
	}
	cacheDir := filepath.Join(os.TempDir(), "e2e-tfcli-"+ver+"-"+runtime.GOOS+"-"+runtime.GOARCH)
	binPath := filepath.Join(cacheDir, binName)
	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://releases.hashicorp.com/terraform/%s/terraform_%s_%s_%s.zip",
		ver, ver, runtime.GOOS, runtime.GOARCH)
	zipPath := filepath.Join(cacheDir, "terraform.zip")
	if err := downloadFile(url, zipPath); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := extractZipEntry(zipPath, binName, binPath); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(binPath, 0o755); err != nil {
			return "", err
		}
	}
	return binPath, nil
}

func downloadFile(url, dst string) error {
	resp, err := http.Get(url) //nolint:gosec // pinned HashiCorp release URL
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func extractZipEntry(zipPath, wantBase, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != wantBase {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, rc); err != nil { //nolint:gosec // trusted release archive
			return err
		}
		return nil
	}
	return fmt.Errorf("%q not found in %s", wantBase, zipPath)
}

// InitializeScenario_e2e_test_resource_and_results_e2e_test_resource_and_collection
// registers the step definitions for the Stage 7.2 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_e2e_test_resource_and_results_e2e_test_resource_and_collection(ctx *godog.ScenarioContext) {
	w := &collectWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = collectWorld{}
		return c, nil
	})
	ctx.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		for _, d := range []string{w.dest, w.orderDest} {
			if d != "" {
				_ = os.RemoveAll(d)
			}
		}
		return c, nil
	})

	// Scenario 1
	ctx.Step(`^an e2e_test resource with fail_on_test_failure true and a fake transport serving the on-target results zip$`, w.givenFailingCreateFixture)
	ctx.Step(`^Create runs against the fake transport for a failing run$`, w.whenCreateRunsFailing)
	ctx.Step(`^the persisted results_dir tree is byte-identical to the committed golden snapshot$`, w.thenTreeByteIdenticalToGolden)
	ctx.Step(`^ERR_TEST_FAILED is returned only after the tree is fully written$`, w.thenErrTestFailedAfterWrite)

	// Scenario 2
	ctx.Step(`^a TestRun whose runner timeout is exceeded on a fake transport$`, w.givenTimeoutRun)
	ctx.Step(`^the engine runs the test$`, w.whenEngineRunsTest)
	ctx.Step(`^it returns ERR_TIMEOUT and issues the process-tree kill script$`, w.thenTimeoutAndKillScript)

	// Scenario 3
	ctx.Step(`^the labdeploy_e2e_test resource schema$`, w.givenE2ESchema)
	ctx.Step(`^the deployment_id attribute is inspected$`, w.whenInspectDeploymentID)
	ctx.Step(`^it is a settable string attribute with no RequiresReplace plan modifier$`, w.thenSettableNoRequiresReplace)

	// Scenario 4
	ctx.Step(`^a config where labdeploy_e2e_test deployment_id references the deployment id$`, w.givenOrderingConfig)
	ctx.Step(`^terraform apply runs in the plugin-testing harness$`, w.whenApplyInHarness)
	ctx.Step(`^Terraform Core orders the e2e_test create strictly after the deployment create$`, w.thenCoreOrdersE2EAfterDeployment)
}

// TestE2E_e2e_test_resource_and_results_e2e_test_resource_and_collection is the
// go test entrypoint for the Stage 7.2 godog suite.
func TestE2E_e2e_test_resource_and_results_e2e_test_resource_and_collection(t *testing.T) {
	e2eCollectT = t
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_e2e_test_resource_and_results_e2e_test_resource_and_collection,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"e2e_test_resource_and_results_e2e_test_resource_and_collection.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
