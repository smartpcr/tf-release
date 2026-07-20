package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Stage 9.2 — E2E (DESIGN §18.9) labdeploy_e2e_test acceptance against W1 (Windows
// Server 2022, WinRM https, .NET 8 + vstest). Each test maps 1:1 to a DESIGN §18.9
// ID. The suite deploys sample-svc via labdeploy_deployment and runs the
// `sample-tests` vstest package via labdeploy_e2e_test whose deployment_id references
// the deployment (ordering the test strictly after the deploy, DESIGN §5.3).
//
// Results are collected into a RUNNER-LOCAL destination_dir (a per-test temp dir),
// so the `results_dir/results/*.trx`, `summary.json` and collected `logs/app.log`
// post-state is asserted directly on the local filesystem.

// Test-package artifact env: base URL (may carry a {ver} token) + per-version sha.
const envTestsBaseURL = "LABDEPLOY_ACC_TESTS_BASE_URL"

// testsArtifact returns the sample-tests package URL + sha256 for version. Under
// TF_ACC=1 a missing URL or checksum is a FAILURE (a test cannot pass against an
// unverifiable package).
func testsArtifact(t *testing.T, version string) (url, sha string) {
	t.Helper()
	base := os.Getenv(envTestsBaseURL)
	if base == "" {
		t.Fatalf("TF_ACC=1 requires %s to serve the sample-tests %s package", envTestsBaseURL, version)
	}
	if strings.Contains(base, "{ver}") {
		url = strings.ReplaceAll(base, "{ver}", version)
	} else {
		url = strings.TrimRight(base, "/") + fmt.Sprintf("/sample-tests-%s.zip", version)
	}
	norm := strings.NewReplacer(".", "_", "-", "_")
	key := "LABDEPLOY_ACC_TESTS_SHA_" + norm.Replace(strings.ToUpper(version))
	sha = os.Getenv(key)
	if sha == "" {
		t.Fatalf("TF_ACC=1 requires %s (the sha256 of the sample-tests %s package)", key, version)
	}
	if !strings.HasPrefix(sha, "sha256:") {
		sha = "sha256:" + sha
	}
	return url, sha
}

// yamlPath forward-slashes a Windows path so it is safe inside a double-quoted YAML
// scalar (backslashes would be YAML escapes).
func yamlPath(p string) string { return strings.ReplaceAll(p, `\`, `/`) }

// testRunSpec builds a kind: TestRun document for W1. destDir is the runner-local
// collection root; format is trx|junit|none; runnerBlock is the full `runner:` body
// (indented two spaces, no leading key); collectExtra is appended into `collect:`.
func testRunSpec(t *testing.T, at accTarget, version, destDir, format, runnerBlock, collectExtra string) string {
	t.Helper()
	url, sha := testsArtifact(t, version)
	fmtLine := "  format: " + format
	if format == "trx" {
		fmtLine = "  format: trx\n  paths: [\"TestResults/*.trx\", \"results/*.trx\"]"
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: TestRun
metadata: { name: sample-svc-e2e }
target:
%s
artifact:
  type: zip
  version: %q
  checksum: %q
  source: { type: http, url: %q }
runner:
%s
results:
%s
pass_criteria:
  exit_codes: [0]
collect:
  logs: ['C:\deploy\sample-svc\shared\logs\*.log']
  destination_dir: %q%s
`, at.yaml, version, sha, url, runnerBlock, fmtLine, yamlPath(destDir), collectExtra)
}

// vstestRunner returns a vstest `runner:` block with optional env entries. env is a
// map rendered as YAML; timeoutSec sets runner.timeout_seconds.
func vstestRunner(timeoutSec int, env map[string]string) string {
	var b strings.Builder
	b.WriteString("  type: vstest\n  assemblies: [\"SampleSvc.E2E.dll\"]\n")
	fmt.Fprintf(&b, "  timeout_seconds: %d", timeoutSec)
	if len(env) > 0 {
		b.WriteString("\n  env:")
		for k, v := range env {
			fmt.Fprintf(&b, "\n    %s: %q", k, v)
		}
	}
	return b.String()
}

// accE2EConfig wires a deployment + an e2e_test whose deployment_id references it.
// e2eExtraHCL adds resource-level HCL attributes (fail_on_test_failure, triggers).
func accE2EConfig(deploymentSpec, e2eSpec, e2eExtraHCL string) string {
	return testAccProviderConfig + fmt.Sprintf(`
resource "labdeploy_deployment" "dep" {
  spec = <<-EOT
%sEOT
}

resource "labdeploy_e2e_test" "e2e" {
  deployment_id = labdeploy_deployment.dep.id
  spec = <<-EOT
%sEOT
%s
}
`, deploymentSpec, e2eSpec, e2eExtraHCL)
}

// e2eSetup env-gates and wires the reattach address (mirrors runScenario's prologue,
// but the e2e scenarios manage their own steps because they drive TWO resources).
func e2eSetup(t *testing.T) {
	t.Helper()
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
}

// e2eDeployment is the sample-svc 1.1.0 windows_service the E2E scenarios test
// against (health http /health; app.log written under shared for collection).
func e2eDeployment(t *testing.T, at accTarget) string {
	return winServiceSpec(t, at, "1.1.0", "zip", healthURL(8080), "")
}

// walkFiles recursively collects files under root whose lowercased base name
// satisfies match. A missing root yields an empty slice (a collection subtree can
// legitimately be absent), never an error, so callers decide what "empty" means.
// CollectFiles preserves the remote <host>/<preserved-path> layout, so every
// collection assertion MUST walk the tree rather than glob a single flat directory.
func walkFiles(root string, match func(lowerName string) bool) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && match(strings.ToLower(d.Name())) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// hasSuffix builds a walkFiles matcher for a lowercased filename suffix.
func hasSuffix(suffix string) func(string) bool {
	return func(n string) bool { return strings.HasSuffix(n, suffix) }
}

// checkTrxUnder asserts results_dir carries at least one *.trx anywhere under its
// results/ subtree. CollectFiles nests TRX files under results/<host>/<preserved
// -path>/*.trx, so the search is RECURSIVE (a flat glob misses host subdirs).
func checkTrxUnder(resultsDir string) error {
	matches := walkFiles(filepath.Join(resultsDir, "results"), hasSuffix(".trx"))
	if len(matches) == 0 {
		// Some runners write directly under results_dir; accept either layout.
		matches = walkFiles(resultsDir, hasSuffix(".trx"))
	}
	if len(matches) == 0 {
		return fmt.Errorf("no *.trx found anywhere under %s/results (walked recursively)", resultsDir)
	}
	return nil
}

// checkResultsPopulated asserts the collection tree under dir carries summary.json
// (written at the results_dir root) AND at least one collected *.log anywhere under
// logs/ (nested logs/<host>/<preserved-path>/*.log — the search is RECURSIVE).
func checkResultsPopulated(dir, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
			return fmt.Errorf("%s: summary.json absent under %s: %w", why, dir, err)
		}
		if logs := walkFiles(filepath.Join(dir, "logs"), hasSuffix(".log")); len(logs) == 0 {
			return fmt.Errorf("%s: no collected *.log anywhere under %s/logs (walked recursively)", why, dir)
		}
		return nil
	}
}

// checkStateResource asserts the presence/absence of addr in the Terraform state
// and, when present, that the given attributes hold the wanted values. E2E-02 uses
// it to prove a failed hard-gate left the deployment committed+healthy while the
// test resource is ABSENT from state.
func checkStateResource(addr string, wantPresent bool, attrs map[string]string) func(*terraform.State) error {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !wantPresent {
			if ok {
				return fmt.Errorf("resource %s must be ABSENT from state after the failed apply, but it is present", addr)
			}
			return nil
		}
		if !ok {
			return fmt.Errorf("resource %s must be present in state, but it is absent", addr)
		}
		for k, want := range attrs {
			if got := rs.Primary.Attributes[k]; got != want {
				return fmt.Errorf("resource %s attribute %s = %q, want %q", addr, k, got, want)
			}
		}
		return nil
	}
}

// winEvent mirrors the ConvertTo-Json shape CollectEventLogs emits per event
// (TimeCreated/Id/LevelDisplayName/ProviderName/Message). TimeCreated is a
// RawMessage because PowerShell 5.1 serializes it as \/Date(ms)\/ while PS7 emits
// an ISO-8601 string.
type winEvent struct {
	TimeCreated  json.RawMessage `json:"TimeCreated"`
	ProviderName string          `json:"ProviderName"`
	ID           int             `json:"Id"`
}

// parseWinEvents decodes a collected event-log JSON file, tolerating both the
// single-object (one event) and array (many events) shapes ConvertTo-Json produces.
func parseWinEvents(raw []byte) ([]winEvent, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	switch raw[0] {
	case '[':
		var evs []winEvent
		if err := json.Unmarshal(raw, &evs); err != nil {
			return nil, err
		}
		return evs, nil
	case '{':
		var one winEvent
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		return []winEvent{one}, nil
	default:
		return nil, fmt.Errorf("not a JSON object or array")
	}
}

// eventTime parses a ConvertTo-Json TimeCreated value, handling both the Windows
// PowerShell 5.1 \/Date(ms)\/ form and the PowerShell 7 ISO-8601 string form.
func eventTime(rawTC json.RawMessage) (time.Time, error) {
	s := strings.TrimSpace(string(rawTC))
	s = strings.Trim(s, `"`)
	s = strings.ReplaceAll(s, `\/`, "/")
	if i := strings.Index(s, "Date("); i >= 0 {
		rest := s[i+len("Date("):]
		if j := strings.IndexByte(rest, ')'); j > 0 {
			num := rest[:j]
			// strip an optional trailing timezone offset, e.g. 1595280000000+0000
			if k := strings.IndexAny(num[1:], "+-"); k >= 0 {
				num = num[:k+1]
			}
			if ms, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64); err == nil {
				return time.UnixMilli(ms).UTC(), nil
			}
		}
	}
	return parseFlexibleTime(s)
}

// parseFlexibleTime parses an ISO-8601 timestamp (RFC3339, optional sub-seconds)
// from the target's `Get-Date -Format o` output or an event's ISO TimeCreated.
func parseFlexibleTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.9999999Z07:00", "2006-01-02T15:04:05"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

// checkEventLogsCollected walks the collected event tree, asserts every event was
// emitted by wantProvider (the provider filter was honored) and that every event's
// TimeCreated is at or after the test start captured on the target's own clock (no
// stale events). Requires at least one parsed event.
func checkEventLogsCollected(dest, wantProvider string, start time.Time) error {
	files := walkFiles(filepath.Join(dest, "logs", "events"), hasSuffix(".json"))
	if len(files) == 0 {
		files = walkFiles(filepath.Join(dest, "logs"), func(n string) bool {
			return strings.Contains(n, "event") && strings.HasSuffix(n, ".json")
		})
	}
	if len(files) == 0 {
		return fmt.Errorf("E2E-05: no collected Windows event-log JSON under %s/logs/events (walked recursively)", dest)
	}
	total := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("E2E-05: read %s: %w", f, err)
		}
		evs, err := parseWinEvents(raw)
		if err != nil {
			return fmt.Errorf("E2E-05: parse %s: %w", f, err)
		}
		for _, e := range evs {
			total++
			if !strings.EqualFold(strings.TrimSpace(e.ProviderName), wantProvider) {
				return fmt.Errorf("E2E-05: event in %s has ProviderName %q, want %q (provider filter not applied)", f, e.ProviderName, wantProvider)
			}
			ts, err := eventTime(e.TimeCreated)
			if err != nil {
				return fmt.Errorf("E2E-05: %s: %w", f, err)
			}
			// E2E-05 requires only events AT OR AFTER the captured test start. The
			// start is captured on the target's OWN clock (in PreConfig, before the
			// triggering apply) and Windows events carry that same host clock at
			// 100 ns precision, so a strict compare is warranted: any event that
			// precedes the captured start — even within the same second — is a
			// pre-existing/stale event and must be rejected.
			if ts.Before(start) {
				return fmt.Errorf("E2E-05: event at %s precedes the test start %s (stale event collected; only events at or after start are permitted)",
					ts.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano))
			}
		}
	}
	if total == 0 {
		return fmt.Errorf("E2E-05: collected event JSON parsed to ZERO events; cannot verify provider identity or timestamps")
	}
	return nil
}

// ---------------------------------------------------------------------------
// E2E-01..07
// ---------------------------------------------------------------------------

// E2E-01: deploy 1.1.0 + TestRun vstest (all pass), triggers.run=1 ⇒ apply ok;
// total=3 passed=3 failed=0 passed=true; results_dir/results/*.trx exists;
// summary.json matches; logs dir contains app.log copied from shared.
func TestAccE2E01_AllPass(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, nil), "")
	cfg := accE2EConfig(dep, tr, `  triggers = { run = "1" }`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "total_tests", "3"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed_tests", "3"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "failed_tests", "0"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "true"),
				resource.TestCheckResourceAttrWith("labdeploy_e2e_test.e2e", "results_dir", checkTrxUnder),
				checkResultsPopulated(dest, "E2E-01: results_dir must carry summary.json + collected app.log"),
			),
		}},
	})
}

// E2E-02: FAIL_ONE=1, fail_on_test_failure=true ⇒ apply exit≠0 ERR_TEST_FAILED.
// Beyond the coded error this proves, at the moment of the hard-gate failure: (a)
// the TRX results were still collected and the results_dir fully populated
// (summary.json + logs); (b) the labdeploy_e2e_test resource is ABSENT from state
// (a failed create is never committed); and (c) the deployment it depends on stays
// committed and — proven by a LIVE health GET, not just cached state — still serves
// v1.1.0. Step 1 drives the failing gate (ExpectError skips its Check); step 2
// re-applies the deployment-only config (a no-op that keeps the same
// labdeploy_deployment.dep) so a normal Check can probe the still-running service
// immediately after the failed test run, before the framework destroys it.
func TestAccE2E02_FailHardGate(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, map[string]string{"FAIL_ONE": "1"}), "")
	cfg := accE2EConfig(dep, tr, `  fail_on_test_failure = true`+"\n  triggers = { run = \"1\" }")
	// Deployment-only config with the SAME resource address as cfg's deployment
	// block, so applying it after the failed step is a no-op that leaves the live
	// service untouched (never a destroy/replace).
	depOnly := testAccProviderConfig + fmt.Sprintf("\nresource \"labdeploy_deployment\" \"dep\" {\n  spec = <<-EOT\n%sEOT\n}\n", dep)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      cfg,
				ExpectError: mustRe(`ERR_TEST_FAILED`),
			},
			{
				// No-op re-apply of the deployment only; Check runs against the live
				// post-failure system.
				Config: depOnly,
				Check: resource.ComposeAggregateTestCheckFunc(
					// (a) collect-then-fail: results populated + TRX collected despite the error.
					checkResultsPopulated(dest, "E2E-02: results_dir must be fully populated despite the hard-gate failure"),
					func(*terraform.State) error { return checkTrxUnder(dest) },
					// (b) the failed test-resource create is never committed to state.
					checkStateResource("labdeploy_e2e_test.e2e", false, nil),
					// (c) the deployment remains committed and, proven LIVE, still serves v1.1.0.
					checkStateResource("labdeploy_deployment.dep", true, map[string]string{
						"service_status":   "running",
						"deployed_version": "1.1.0",
					}),
					checkHealthBody(at, healthURL(8080), "v=1.1.0"),
				),
			},
		},
		CheckDestroy: checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
	})
}

// E2E-03: same failing run, fail_on_test_failure=false ⇒ apply ok; passed=false
// failed=1 (the output a pipeline gate consumes).
func TestAccE2E03_FailSoftGate(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, map[string]string{"FAIL_ONE": "1"}), "")
	cfg := accE2EConfig(dep, tr, `  fail_on_test_failure = false`+"\n  triggers = { run = \"1\" }")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "false"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "failed_tests", "1"),
				resource.TestCheckResourceAttrWith("labdeploy_e2e_test.e2e", "results_dir", checkTrxUnder),
			),
		}},
	})
}

// E2E-04: runner.timeout_seconds=10 against a sleep-forever test ⇒ ERR_TIMEOUT; on
// the target the test process tree is killed; partial logs collected. The hang is
// injected via the sample-tests HANG=1 env; partial-collection is asserted in
// CheckDestroy (ExpectError skips Check).
func TestAccE2E04_TimeoutKillsTree(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(10, map[string]string{"HANG": "1"}), "")
	cfg := accE2EConfig(dep, tr, `  triggers = { run = "1" }`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: mustRe(`ERR_TIMEOUT`),
		}},
		// Best-effort collection still runs on timeout: partial logs must land.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkNoLingeringTestProcess(at, "E2E-04: the timed-out test process tree must be dead on the target"),
			func(*terraform.State) error {
				if logs := walkFiles(filepath.Join(dest, "logs"), hasSuffix(".log")); len(logs) == 0 {
					return fmt.Errorf("E2E-04: no partial logs collected anywhere under %s/logs after the timeout (walked recursively)", dest)
				}
				return nil
			},
		),
	})
}

// checkNoLingeringTestProcess asserts the vstest runner process is not still running
// on the target after an E2E-04 timeout (the watchdog kills the whole tree).
func checkNoLingeringTestProcess(at accTarget, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			"if (Get-Process -Name vstest.console -ErrorAction SilentlyContinue) { 'ALIVE' } else { 'DEAD' }",
			"echo DEAD")
		if err != nil {
			return fmt.Errorf("%s: process probe: %w", why, err)
		}
		if strings.Contains(out, "ALIVE") {
			return fmt.Errorf("%s: a vstest.console process is still running", why)
		}
		return nil
	}
}

// E2E-05: collect.windows_event_logs provider filter ⇒ event JSON collected under
// results_dir/logs/events. The collected JSON must PARSE, every event must carry
// ProviderName SampleSvc (the provider filter was applied), and every event's
// TimeCreated must be at or after the test start captured on the TARGET's own clock
// (no stale events). All tests pass; the substantive assertion is on the events.
func TestAccE2E05_EventLogCollection(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	collectExtra := "\n  windows_event_logs:\n    - { log: Application, provider: SampleSvc }"
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, nil), collectExtra)
	cfg := accE2EConfig(dep, tr, `  triggers = { run = "1" }`)
	var eventStart time.Time
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		Steps: []resource.TestStep{{
			// Capture the start time on the TARGET's clock so the "events ≥ start"
			// assertion is immune to runner/host skew (events carry the host clock).
			PreConfig: preConfig(t, func() error {
				out, err := probeHost(at,
					"(Get-Date).ToUniversalTime().ToString('o')",
					"date -u +%Y-%m-%dT%H:%M:%S.0000000Z")
				if err != nil {
					return fmt.Errorf("capture target start time: %w", err)
				}
				ts, perr := parseFlexibleTime(strings.TrimSpace(out))
				if perr != nil {
					return fmt.Errorf("parse target start time %q: %w", strings.TrimSpace(out), perr)
				}
				eventStart = ts
				return nil
			}),
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "true"),
				func(*terraform.State) error { return checkEventLogsCollected(dest, "SampleSvc", eventStart) },
			),
		}},
	})
}

// E2E-06: triggers.run bumped 1→2 (same everything) ⇒ plan = replace; tests re-run.
// The intervening PlanOnly step proves the bump yields a non-empty (replace) plan;
// the final apply proves the re-run succeeds.
func TestAccE2E06_TriggerReRun(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, nil), "")
	run1 := accE2EConfig(dep, tr, `  triggers = { run = "1" }`)
	run2 := accE2EConfig(dep, tr, `  triggers = { run = "2" }`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		Steps: []resource.TestStep{
			{
				Config: run1,
				Check:  resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "true"),
			},
			// Bumping triggers.run must yield a REPLACE (non-empty) plan.
			{
				Config:             run2,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			// Applying the bump re-runs the tests (3 pass again).
			{
				Config: run2,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "total_tests", "3"),
					resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "true"),
				),
			},
		},
	})
}

// E2E-07: results.format none, exit-code-only exec runner ⇒ passed reflects the exit
// code list; counters = -1 ("not measured" sentinel).
func TestAccE2E07_ExitCodeOnly(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	runner := "  type: exec\n  command: sample-tests.cmd\n  timeout_seconds: 600"
	tr := testRunSpec(t, at, "1.1.0", dest, "none", runner, "")
	cfg := accE2EConfig(dep, tr, `  triggers = { run = "1" }`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "true"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "total_tests", "-1"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed_tests", "-1"),
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "failed_tests", "-1"),
			),
		}},
	})
}
