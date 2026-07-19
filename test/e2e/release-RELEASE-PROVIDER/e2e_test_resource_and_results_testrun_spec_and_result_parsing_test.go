//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 7.1 (TestRun Spec
// and Result Parsing).
//
// Both scenarios exercise the REAL in-process engine code — no external service
// is required (setup: inline), so both are proved against committed goldens /
// in-process assertions:
//
//   - Runner expansion golden -> for each runner.type (exec, vstest, dotnet_test,
//     npm — with and without the typed convenience fields) renders the REAL
//     engine.RunnerCommand and asserts the expanded command line is byte-identical
//     to the committed golden under internal/engine/testdata/runner, and that the
//     results-dir logger flags (/Logger:trx /ResultsDirectory:... for vstest,
//     --logger trx --results-directory ... --no-build for dotnet_test) are present
//     (proof: golden; DESIGN §7.2).
//   - Pass criteria evaluation -> drives the REAL engine.EvaluatePass over a
//     matrix of exit codes, formats and counters and asserts passed reflects
//     exit_codes AND min_pass_rate over (total-skipped), and that a results.format
//     of none rides purely on exit codes while engine.NoResultCounters yields -1
//     for every counter (proof: in-process; DESIGN §7.3, §5.3; E2E-07).
//
// Every Given/When/Then invokes the real engine code and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/logs"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// testrunModuleRoot walks up from this source file to the module root so the
// golden-fixture scenario can read internal/engine/testdata/runner regardless of
// the working directory go test chooses.
func testrunModuleRoot() (string, error) {
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

func ptrF64(v float64) *float64 { return &v }

// runnerCase mirrors one committed runner-command golden: the golden basename
// under internal/engine/testdata/runner plus the TestRun the REAL
// engine.RunnerCommand must reproduce. requireFlag, when set, is a substring the
// expanded command MUST contain (the results-dir logger flags per DESIGN §7).
type runnerCase struct {
	name        string
	tr          *spec.TestRun
	requireFlag string
}

func runnerCases() []runnerCase {
	return []runnerCase{
		{"exec", &spec.TestRun{Runner: spec.Runner{
			Type: "exec", Command: `bin\smoke.exe`, Args: []string{"--fast", "--ci"}}}, ""},
		{"exec_no_args", &spec.TestRun{Runner: spec.Runner{
			Type: "exec", Command: "./run-tests.sh"}}, ""},
		{"vstest_args", &spec.TestRun{Runner: spec.Runner{
			Type: "vstest", Args: []string{`tests\Unit.dll`, `tests\Integration.dll`, "/Parallel"}}},
			`/Logger:trx /ResultsDirectory:`},
		{"vstest_assemblies", &spec.TestRun{Runner: spec.Runner{
			Type: "vstest", Assemblies: []string{`tests\Unit.dll`}, Args: []string{"/InIsolation"}}},
			`/Logger:trx /ResultsDirectory:`},
		{"dotnet_test_args", &spec.TestRun{Runner: spec.Runner{
			Type: "dotnet_test", Args: []string{`tests\Suite.csproj`, "--filter", "Category=Smoke"}}},
			`--logger trx --results-directory`},
		{"dotnet_test_project", &spec.TestRun{Runner: spec.Runner{
			Type: "dotnet_test", Project: `tests\Suite.csproj`, Args: []string{"--filter", "Category=Smoke"}}},
			`--logger trx --results-directory`},
		{"npm_default", &spec.TestRun{Runner: spec.Runner{Type: "npm"}}, ""},
		{"npm_script", &spec.TestRun{Runner: spec.Runner{Type: "npm", Script: "e2e"}}, ""},
		{"npm_args", &spec.TestRun{Runner: spec.Runner{
			Type: "npm", Args: []string{"run", "test:ci", "--", "--reporter=junit"}}}, ""},
	}
}

// passCase mirrors one pass-criteria evaluation from DESIGN §7.3.
type passCase struct {
	name     string
	exit     int
	format   string
	counters logs.Counters
	pc       spec.PassCriteria
	passed   bool
	exitOK   bool
	rateOK   bool
}

func passCases() []passCase {
	return []passCase{
		{"trx all pass default", 0, "trx",
			logs.Counters{Total: 3, Passed: 3}, spec.PassCriteria{}, true, true, true},
		{"trx one failure strict rate", 0, "trx",
			logs.Counters{Total: 4, Passed: 3, Failed: 1}, spec.PassCriteria{MinPassRate: ptrF64(1.0)}, false, true, false},
		{"trx skipped excluded from denom", 0, "trx",
			logs.Counters{Total: 4, Passed: 3, Skipped: 1}, spec.PassCriteria{MinPassRate: ptrF64(0.75)}, true, true, true},
		{"trx boundary rate", 0, "trx",
			logs.Counters{Total: 4, Passed: 3, Failed: 1}, spec.PassCriteria{MinPassRate: ptrF64(0.75)}, true, true, true},
		{"trx bad exit", 1, "trx",
			logs.Counters{Total: 3, Passed: 3}, spec.PassCriteria{}, false, false, true},
		{"trx custom exit codes", 7, "trx",
			logs.Counters{Total: 2, Passed: 2}, spec.PassCriteria{ExitCodes: []int{7}}, true, true, true},
		{"trx zero tests exit-code-only", 0, "trx",
			logs.Counters{}, spec.PassCriteria{}, true, true, true},
		{"trx zero tests bad exit", 1, "trx",
			logs.Counters{}, spec.PassCriteria{}, false, false, true},
		{"trx all skipped vacuous pass", 0, "trx",
			logs.Counters{Total: 5, Skipped: 5}, spec.PassCriteria{}, true, true, true},
		{"none exit ok", 0, "none",
			engine.NoResultCounters(), spec.PassCriteria{}, true, true, true},
		{"none bad exit", 2, "none",
			engine.NoResultCounters(), spec.PassCriteria{}, false, false, true},
		{"empty format custom exit", 3, "",
			engine.NoResultCounters(), spec.PassCriteria{ExitCodes: []int{0, 3}}, true, true, true},
	}
}

// testrunWorld carries state across the steps of a single scenario.
type testrunWorld struct {
	// runner-expansion scenario
	runners  []runnerCase
	expanded map[string]string

	// pass-criteria scenario
	cases []passCase
}

// --- Runner expansion golden -------------------------------------------------

func (w *testrunWorld) runnerSpecsDeclared() error {
	w.runners = runnerCases()
	// Every runner.type must be represented (exec, vstest, dotnet_test, npm).
	seen := map[string]bool{}
	for _, c := range w.runners {
		seen[c.tr.Runner.Type] = true
	}
	for _, want := range []string{"exec", "vstest", "dotnet_test", "npm"} {
		if !seen[want] {
			return fmt.Errorf("runner.type %q not covered by the golden matrix", want)
		}
	}
	return nil
}

func (w *testrunWorld) runnerCommandsExpanded() error {
	if len(w.runners) == 0 {
		return fmt.Errorf("runner specs not declared (Given step did not run)")
	}
	w.expanded = map[string]string{}
	for _, c := range w.runners {
		got := engine.RunnerCommand(c.tr)
		if strings.TrimSpace(got) == "" {
			return fmt.Errorf("%s: empty expanded runner command", c.name)
		}
		w.expanded[c.name] = got
	}
	return nil
}

func (w *testrunWorld) runnerCommandsMatchGolden() error {
	root, err := testrunModuleRoot()
	if err != nil {
		return err
	}
	sawResultsFlag := false
	for _, c := range w.runners {
		got, ok := w.expanded[c.name]
		if !ok {
			return fmt.Errorf("no expanded command for %s", c.name)
		}
		goldenPath := filepath.Join(root, "internal", "engine", "testdata", "runner", c.name+".golden")
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			return fmt.Errorf("read runner golden %s: %w", goldenPath, err)
		}
		if got != string(want) {
			return fmt.Errorf("runner command for %s golden drift:\n---got---\n%s\n---want---\n%s",
				c.name, got, string(want))
		}
		if c.requireFlag != "" {
			if !strings.Contains(got, c.requireFlag) {
				return fmt.Errorf("%s: expanded command missing results-dir flag %q:\n%s",
					c.name, c.requireFlag, got)
			}
			sawResultsFlag = true
		}
	}
	if !sawResultsFlag {
		return fmt.Errorf("no runner case asserted the results-dir logger flags")
	}
	return nil
}

// --- Pass criteria evaluation ------------------------------------------------

func (w *testrunWorld) passCasesDeclared() error {
	w.cases = passCases()
	if len(w.cases) == 0 {
		return fmt.Errorf("no pass-criteria cases declared")
	}
	return nil
}

func (w *testrunWorld) passCriteriaEvaluated() error {
	// Evaluation is pure; assertion happens in the Then step. Nothing to stage
	// here beyond confirming the Given ran.
	if len(w.cases) == 0 {
		return fmt.Errorf("pass-criteria cases not declared (Given step did not run)")
	}
	return nil
}

func (w *testrunWorld) verdictsAndSentinelHold() error {
	sawNone := false
	for _, c := range w.cases {
		pc := c.pc
		passed, exitOK, rateOK := engine.EvaluatePass(c.exit, c.format, c.counters, &pc)
		if passed != c.passed || exitOK != c.exitOK || rateOK != c.rateOK {
			return fmt.Errorf("EvaluatePass(%d,%q,%+v,%+v) = (passed=%t exitOK=%t rateOK=%t), want (passed=%t exitOK=%t rateOK=%t)",
				c.exit, c.format, c.counters, c.pc, passed, exitOK, rateOK, c.passed, c.exitOK, c.rateOK)
		}
		if c.format == "none" || c.format == "" {
			sawNone = true
		}
	}
	if !sawNone {
		return fmt.Errorf("no format:none/empty case exercised (the -1 sentinel path must be covered)")
	}
	// format: none yields -1 for EVERY counter (DESIGN §5.3; E2E-07).
	sentinel := engine.NoResultCounters()
	if sentinel.Total != -1 || sentinel.Passed != -1 || sentinel.Failed != -1 || sentinel.Skipped != -1 {
		return fmt.Errorf("NoResultCounters = %+v, want all -1", sentinel)
	}
	return nil
}

// InitializeScenario_e2e_test_resource_and_results_testrun_spec_and_result_parsing
// registers the step definitions for the Stage 7.1 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_e2e_test_resource_and_results_testrun_spec_and_result_parsing(ctx *godog.ScenarioContext) {
	w := &testrunWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = testrunWorld{}
		return c, nil
	})

	ctx.Step(`^a TestRun spec for each runner type$`, w.runnerSpecsDeclared)
	ctx.Step(`^the runner command is expanded$`, w.runnerCommandsExpanded)
	ctx.Step(`^it matches the committed golden including results-dir flags$`, w.runnerCommandsMatchGolden)

	ctx.Step(`^parsed counters plus pass criteria for each case$`, w.passCasesDeclared)
	ctx.Step(`^the pass criteria are evaluated$`, w.passCriteriaEvaluated)
	ctx.Step(`^passed reflects exit_codes and min_pass_rate and format none yields -1 counters$`, w.verdictsAndSentinelHold)
}

// TestE2E_e2e_test_resource_and_results_testrun_spec_and_result_parsing is the go
// test entrypoint for the Stage 7.1 godog suite.
func TestE2E_e2e_test_resource_and_results_testrun_spec_and_result_parsing(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_e2e_test_resource_and_results_testrun_spec_and_result_parsing,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"e2e_test_resource_and_results_testrun_spec_and_result_parsing.feature"},
			TestingT: t,
			// Strict makes godog fail the run on undefined, pending, or ambiguous
			// steps (v0.15.1 suite.shouldFail returns s.strict for those). Without
			// it a future Gherkin reword/typo — or a new step added without a
			// step-def — is reported "undefined" yet the suite still returns 0,
			// silently turning this acceptance scenario into a green no-op.
			Strict: true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
