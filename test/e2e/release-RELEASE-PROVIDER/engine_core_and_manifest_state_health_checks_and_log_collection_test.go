//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 3.4 (Health
// Checks and Log Collection).
//
// Both scenarios exercise the REAL in-process code — no external service is
// required (setup: inline), so both are proved against committed golden fixtures:
//
//   - Health script generation -> for each health_check.type (http with and
//     without expect_status/expect_body_regex, tcp, exec) renders the REAL
//     engine.BuildProbeCmd for windows AND linux, asserting each generated probe
//     script is byte-identical to the committed golden under
//     internal/engine/testdata/health, and that the http_expect scripts actually
//     encode the configured expect_status (503) and expect_body_regex (proof:
//     golden; DESIGN §6.5).
//   - TRX and JUnit counters -> parses the committed TRX and JUnit fixtures under
//     internal/logs/testdata via the REAL logs.ParseTRX / logs.ParseJUnit /
//     logs.SumResults and asserts the total/passed/failed/skipped counters match
//     the expected values (proof: golden; T8, DESIGN §7).
//
// Every Given/When/Then invokes the real engine/logs code and asserts on its
// result.
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

// healthModuleRoot walks up from this source file to the module root so the
// golden-fixture scenarios can read internal/{engine,logs}/testdata regardless
// of the working directory go test chooses.
func healthModuleRoot() (string, error) {
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

// healthCase mirrors one committed health-script golden: a health_check config
// plus the per-OS golden basename the REAL engine.BuildProbeCmd must reproduce.
type healthCase struct {
	name        string
	hc          *spec.HealthCheck
	expectProbe bool // http_expect: script must encode expect_status/expect_body_regex
	noProbe     bool // type "none": probe is skipped entirely (empty script, no golden)
}

func healthCases() []healthCase {
	return []healthCase{
		{"http_default", &spec.HealthCheck{Type: "http", HTTP: spec.HTTPCheck{URL: "http://localhost:8080/health"}}, false, false},
		{"http_expect", &spec.HealthCheck{Type: "http", HTTP: spec.HTTPCheck{
			URL: "http://localhost:8080/health", ExpectStatus: 503, ExpectBodyRegex: "v=1\\.2\\.3"}}, true, false},
		{"tcp", &spec.HealthCheck{Type: "tcp", TCP: spec.TCPCheck{Port: 5432}}, false, false},
		{"exec", &spec.HealthCheck{Type: "exec", Exec: spec.ExecCheck{Command: "bin\\healthcheck.exe --probe"}}, false, false},
		{"none", &spec.HealthCheck{Type: "none"}, false, true},
	}
}

type healthOS struct {
	kind    spec.OSKind
	label   string
	workDir string
}

func healthOSes() []healthOS {
	return []healthOS{
		{spec.OSWindows, "windows", `C:\deploy\app\releases\1.0.0`},
		{spec.OSLinux, "linux", "/opt/deploy/app/releases/1.0.0"},
	}
}

// healthWorld carries state across the steps of a single scenario.
type healthWorld struct {
	// health-script scenario
	cases []healthCase
	oses  []healthOS
	// generated[caseName][osLabel] = rendered probe script
	generated map[string]map[string]string

	// counters scenario
	trxSingle  logs.Counters
	trxSum     logs.Counters
	trxFiles   int
	junitMulti logs.Counters
	junitSum   logs.Counters
	junitFiles int
	parseErr   error
}

// --- Health script generation ------------------------------------------------

func (w *healthWorld) healthTypesDeclared() error {
	w.cases = healthCases()
	w.oses = healthOSes()
	if len(w.cases) != 5 {
		return fmt.Errorf("expected 5 health_check cases (http x2, tcp, exec, none), got %d", len(w.cases))
	}
	return nil
}

func (w *healthWorld) probeScriptsGeneratedForEachOS() error {
	if len(w.cases) == 0 || len(w.oses) == 0 {
		return fmt.Errorf("health cases not declared (Given step did not run)")
	}
	w.generated = map[string]map[string]string{}
	for _, c := range w.cases {
		w.generated[c.name] = map[string]string{}
		for _, o := range w.oses {
			cmd := engine.BuildProbeCmd(o.kind, c.hc, o.workDir, nil)
			// type "none" short-circuits: the probe is skipped entirely, so the
			// engine yields an EMPTY script (no golden fixture exists for it).
			if c.noProbe {
				if cmd.Script != "" {
					return fmt.Errorf("%s/%s: type none must produce an empty script, got %q", c.name, o.label, cmd.Script)
				}
			} else if cmd.Script == "" {
				return fmt.Errorf("%s/%s: empty probe script", c.name, o.label)
			}
			w.generated[c.name][o.label] = cmd.Script
		}
	}
	return nil
}

func (w *healthWorld) probeScriptsMatchGoldenAndEncodeExpect() error {
	root, err := healthModuleRoot()
	if err != nil {
		return err
	}
	sawExpect := false
	sawNone := false
	for _, c := range w.cases {
		for _, o := range w.oses {
			got, ok := w.generated[c.name][o.label]
			if !ok {
				return fmt.Errorf("no probe script generated for %s/%s", c.name, o.label)
			}
			// type "none" has no golden fixture: the probe is skipped so the
			// script must be empty (there is nothing to match against a golden).
			if c.noProbe {
				if got != "" {
					return fmt.Errorf("%s/%s: type none must yield an empty script, got %q", c.name, o.label, got)
				}
				sawNone = true
				continue
			}
			goldenPath := filepath.Join(root, "internal", "engine", "testdata", "health", c.name+"_"+o.label+".golden")
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				return fmt.Errorf("read health golden %s: %w", goldenPath, err)
			}
			if got != string(want) {
				return fmt.Errorf("probe script for %s/%s golden drift:\n---got---\n%s\n---want---\n%s",
					c.name, o.label, got, string(want))
			}
			if c.expectProbe {
				// The http_expect script must encode BOTH the configured
				// expect_status (503, NOT the 200 default) and expect_body_regex.
				if !strings.Contains(got, "503") {
					return fmt.Errorf("%s/%s does not encode expect_status 503:\n%s", c.name, o.label, got)
				}
				if !strings.Contains(got, `v=1\.2\.3`) {
					return fmt.Errorf("%s/%s does not encode expect_body_regex:\n%s", c.name, o.label, got)
				}
				sawExpect = true
			}
		}
	}
	if !sawExpect {
		return fmt.Errorf("no http_expect case asserted expect_status/expect_body_regex encoding")
	}
	if !sawNone {
		return fmt.Errorf("type none was not exercised (each health_check.type must be covered)")
	}
	return nil
}

// --- TRX and JUnit counters --------------------------------------------------

func (w *healthWorld) resultFixturesCommitted() error {
	root, err := healthModuleRoot()
	if err != nil {
		return err
	}
	base := filepath.Join(root, "internal", "logs", "testdata")
	for _, rel := range []string{
		filepath.Join("trx", "run1.trx"),
		filepath.Join("trx", "run2.trx"),
		filepath.Join("junit", "suites.xml"),
		filepath.Join("junit", "single.xml"),
	} {
		if _, err := os.Stat(filepath.Join(base, rel)); err != nil {
			return fmt.Errorf("missing committed result fixture %s: %w", rel, err)
		}
	}
	return nil
}

func (w *healthWorld) resultFixturesParsed() error {
	root, err := healthModuleRoot()
	if err != nil {
		return err
	}
	base := filepath.Join(root, "internal", "logs", "testdata")

	if w.trxSingle, w.parseErr = logs.ParseTRX(filepath.Join(base, "trx", "run1.trx")); w.parseErr != nil {
		return fmt.Errorf("ParseTRX run1: %w", w.parseErr)
	}
	var trxFileList []string
	if w.trxSum, trxFileList, w.parseErr = logs.SumResults("trx", base, []string{"trx/*.trx"}); w.parseErr != nil {
		return fmt.Errorf("SumResults trx: %w", w.parseErr)
	}
	w.trxFiles = len(trxFileList)

	if w.junitMulti, w.parseErr = logs.ParseJUnit(filepath.Join(base, "junit", "suites.xml")); w.parseErr != nil {
		return fmt.Errorf("ParseJUnit suites: %w", w.parseErr)
	}
	var junitFileList []string
	if w.junitSum, junitFileList, w.parseErr = logs.SumResults("junit", base, []string{"junit/*.xml"}); w.parseErr != nil {
		return fmt.Errorf("SumResults junit: %w", w.parseErr)
	}
	w.junitFiles = len(junitFileList)
	return nil
}

func (w *healthWorld) countersMatchExpected() error {
	if w.parseErr != nil {
		return fmt.Errorf("parse returned error: %w", w.parseErr)
	}
	check := func(label string, got, want logs.Counters) error {
		if got != want {
			return fmt.Errorf("%s counters = %+v, want %+v", label, got, want)
		}
		return nil
	}
	if err := check("trx run1", w.trxSingle, logs.Counters{Total: 10, Passed: 7, Failed: 2, Skipped: 1}); err != nil {
		return err
	}
	if w.trxFiles != 2 {
		return fmt.Errorf("expected 2 trx files summed, got %d", w.trxFiles)
	}
	if err := check("trx sum", w.trxSum, logs.Counters{Total: 15, Passed: 12, Failed: 2, Skipped: 1}); err != nil {
		return err
	}
	if err := check("junit suites", w.junitMulti, logs.Counters{Total: 10, Passed: 7, Failed: 2, Skipped: 1}); err != nil {
		return err
	}
	if w.junitFiles != 2 {
		return fmt.Errorf("expected 2 junit files summed, got %d", w.junitFiles)
	}
	if err := check("junit sum", w.junitSum, logs.Counters{Total: 13, Passed: 10, Failed: 2, Skipped: 1}); err != nil {
		return err
	}
	return nil
}

// InitializeScenario_engine_core_and_manifest_state_health_checks_and_log_collection
// registers the step definitions for the Stage 3.4 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_engine_core_and_manifest_state_health_checks_and_log_collection(ctx *godog.ScenarioContext) {
	w := &healthWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = healthWorld{}
		return c, nil
	})

	ctx.Step(`^the health_check types http-default, http-expect, tcp, exec and none$`, w.healthTypesDeclared)
	ctx.Step(`^the probe scripts are generated for windows and linux$`, w.probeScriptsGeneratedForEachOS)
	ctx.Step(`^they match the committed golden fixtures and encode expect_status and expect_body_regex logic$`, w.probeScriptsMatchGoldenAndEncodeExpect)

	ctx.Step(`^the committed TRX and JUnit result fixtures$`, w.resultFixturesCommitted)
	ctx.Step(`^they are parsed$`, w.resultFixturesParsed)
	ctx.Step(`^the total, passed, failed and skipped counters match the expected values$`, w.countersMatchExpected)
}

// TestE2E_engine_core_and_manifest_state_health_checks_and_log_collection is the
// go test entrypoint for the Stage 3.4 godog suite.
func TestE2E_engine_core_and_manifest_state_health_checks_and_log_collection(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_engine_core_and_manifest_state_health_checks_and_log_collection,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"engine_core_and_manifest_state_health_checks_and_log_collection.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
