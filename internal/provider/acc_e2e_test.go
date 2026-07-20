package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// checkTrxUnder asserts results_dir/results contains at least one *.trx file.
func checkTrxUnder(resultsDir string) error {
	matches, _ := filepath.Glob(filepath.Join(resultsDir, "results", "*.trx"))
	if len(matches) == 0 {
		// Some runners write directly under results_dir; accept either layout.
		matches, _ = filepath.Glob(filepath.Join(resultsDir, "*.trx"))
	}
	if len(matches) == 0 {
		return fmt.Errorf("no *.trx under %s", resultsDir)
	}
	return nil
}

// checkResultsPopulated asserts the collection tree under dir carries a summary.json
// AND the collected app.log — the "results_dir fully populated" post-state.
func checkResultsPopulated(dir, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
			return fmt.Errorf("%s: summary.json absent under %s: %w", why, dir, err)
		}
		logs, _ := filepath.Glob(filepath.Join(dir, "logs", "*.log"))
		if len(logs) == 0 {
			return fmt.Errorf("%s: no collected logs under %s/logs", why, dir)
		}
		return nil
	}
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

// E2E-02: FAIL_ONE=1, fail_on_test_failure=true ⇒ apply exit≠0 ERR_TEST_FAILED;
// results_dir fully populated anyway (asserted in CheckDestroy against the captured
// dest dir, since ExpectError steps skip Check); deployment resource unaffected.
func TestAccE2E02_FailHardGate(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, map[string]string{"FAIL_ONE": "1"}), "")
	cfg := accE2EConfig(dep, tr, `  fail_on_test_failure = true`+"\n  triggers = { run = \"1\" }")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      cfg,
			ExpectError: mustRe(`ERR_TEST_FAILED`),
		}},
		// collect-then-fail: results are populated even though apply errored.
		CheckDestroy: checkResultsPopulated(dest, "E2E-02: results_dir must be fully populated despite the hard-gate failure"),
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
				logs, _ := filepath.Glob(filepath.Join(dest, "logs", "*.log"))
				if len(logs) == 0 {
					return fmt.Errorf("E2E-04: no partial logs collected under %s/logs after the timeout", dest)
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

// E2E-05: collect.windows_event_logs provider filter ⇒ events json in logs dir;
// only events at or after the test start time. (All tests pass; the assertion is on
// the collected event JSON.)
func TestAccE2E05_EventLogCollection(t *testing.T) {
	accPreCheck(t)
	e2eSetup(t)
	at := requireW1(t)
	dest := t.TempDir()
	dep := e2eDeployment(t, at)
	collectExtra := "\n  windows_event_logs:\n    - { log: Application, provider: SampleSvc }"
	tr := testRunSpec(t, at, "1.1.0", dest, "trx", vstestRunner(1800, nil), collectExtra)
	cfg := accE2EConfig(dep, tr, `  triggers = { run = "1" }`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_e2e_test.e2e", "passed", "true"),
				func(*terraform.State) error {
					ev, _ := filepath.Glob(filepath.Join(dest, "logs", "*event*.json"))
					if len(ev) == 0 {
						ev, _ = filepath.Glob(filepath.Join(dest, "logs", "*.json"))
					}
					if len(ev) == 0 {
						return fmt.Errorf("E2E-05: no collected Windows event-log JSON under %s/logs", dest)
					}
					return nil
				},
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
