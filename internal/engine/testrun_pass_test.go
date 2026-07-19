package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/logs"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

func ptrF(v float64) *float64 { return &v }

// TestEvaluatePass exercises pass-criteria evaluation in-process (Stage 7.1
// scenario "Pass criteria evaluation", DESIGN §7.3): `passed` reflects the
// exit_codes set AND min_pass_rate over (total-skipped), and a `format: none`
// run rides purely on exit codes with the rate gate skipped.
func TestEvaluatePass(t *testing.T) {
	cases := []struct {
		name     string
		exit     int
		format   string
		counters logs.Counters
		pc       spec.PassCriteria
		passed   bool
		exitOK   bool
		rateOK   bool
	}{
		{
			name: "trx all pass default criteria", exit: 0, format: "trx",
			counters: logs.Counters{Total: 3, Passed: 3, Failed: 0, Skipped: 0},
			pc:       spec.PassCriteria{}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// One failure at min_pass_rate=1.0 fails the rate gate even though exit=0.
			name: "trx one failure strict rate", exit: 0, format: "trx",
			counters: logs.Counters{Total: 4, Passed: 3, Failed: 1, Skipped: 0},
			pc:       spec.PassCriteria{MinPassRate: ptrF(1.0)}, passed: false, exitOK: true, rateOK: false,
		},
		{
			// pass_rate = 3/(4-1)=1.0 over total-skipped clears min_pass_rate=0.75.
			name: "trx skipped excluded from denom", exit: 0, format: "trx",
			counters: logs.Counters{Total: 4, Passed: 3, Failed: 0, Skipped: 1},
			pc:       spec.PassCriteria{MinPassRate: ptrF(0.75)}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// 3/4 = 0.75 exactly meets min_pass_rate=0.75.
			name: "trx boundary rate", exit: 0, format: "trx",
			counters: logs.Counters{Total: 4, Passed: 3, Failed: 1, Skipped: 0},
			pc:       spec.PassCriteria{MinPassRate: ptrF(0.75)}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// Non-zero exit not in the allowed set fails regardless of rate.
			name: "trx bad exit", exit: 1, format: "trx",
			counters: logs.Counters{Total: 3, Passed: 3, Failed: 0, Skipped: 0},
			pc:       spec.PassCriteria{}, passed: false, exitOK: false, rateOK: true,
		},
		{
			// Custom exit_codes accepts a non-zero runner exit.
			name: "trx custom exit codes", exit: 7, format: "trx",
			counters: logs.Counters{Total: 2, Passed: 2, Failed: 0, Skipped: 0},
			pc:       spec.PassCriteria{ExitCodes: []int{7}}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// Zero tests reported: min_pass_rate is NOT enforced (DESIGN §7 line
			// 372 "only enforced when total>0"); verdict is exit-code-only.
			name: "trx zero tests exit-code-only", exit: 0, format: "trx",
			counters: logs.Counters{Total: 0, Passed: 0, Failed: 0, Skipped: 0},
			pc:       spec.PassCriteria{}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// Zero tests reported but a failing exit code still fails the run
			// (exit gate applies regardless of the skipped rate gate).
			name: "trx zero tests bad exit", exit: 1, format: "trx",
			counters: logs.Counters{Total: 0, Passed: 0, Failed: 0, Skipped: 0},
			pc:       spec.PassCriteria{}, passed: false, exitOK: false, rateOK: true,
		},
		{
			// All tests skipped (total>0, total==skipped): no failures ⇒ pass_rate
			// is vacuously 1.0 and the rate gate is satisfied (consistent with the
			// value written to summary.json).
			name: "trx all skipped", exit: 0, format: "trx",
			counters: logs.Counters{Total: 5, Passed: 0, Failed: 0, Skipped: 5},
			pc:       spec.PassCriteria{}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// format none: rate gate skipped, verdict is exit-code-only, passing.
			name: "none exit ok", exit: 0, format: "none",
			counters: noResultCounters(), pc: spec.PassCriteria{}, passed: true, exitOK: true, rateOK: true,
		},
		{
			// format none: failing exit code fails even though rate gate skipped.
			name: "none bad exit", exit: 2, format: "none",
			counters: noResultCounters(), pc: spec.PassCriteria{}, passed: false, exitOK: false, rateOK: true,
		},
		{
			// empty format behaves as none (default) — exit-code-only.
			name: "empty format custom exit", exit: 3, format: "",
			counters: noResultCounters(), pc: spec.PassCriteria{ExitCodes: []int{0, 3}},
			passed:   true, exitOK: true, rateOK: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			passed, exitOK, rateOK := evaluatePass(c.exit, c.format, c.counters, &c.pc)
			if passed != c.passed || exitOK != c.exitOK || rateOK != c.rateOK {
				t.Fatalf("evaluatePass(%d,%q,%+v,%+v) = (passed=%t exitOK=%t rateOK=%t), want (passed=%t exitOK=%t rateOK=%t)",
					c.exit, c.format, c.counters, c.pc, passed, exitOK, rateOK, c.passed, c.exitOK, c.rateOK)
			}
		})
	}
}

// TestNoResultCountersAreMinusOne locks the DESIGN §5.3 contract that a
// `results.format: none` run reports -1 counters (not zero) so a consumer can
// distinguish "not measured" from a genuine empty result set (E2E-07).
func TestNoResultCountersAreMinusOne(t *testing.T) {
	c := noResultCounters()
	if c.Total != -1 || c.Passed != -1 || c.Failed != -1 || c.Skipped != -1 {
		t.Fatalf("noResultCounters = %+v, want all -1", c)
	}
}

// TestComputePassRateConsistency locks the single-source-of-truth rate helper
// shared by evaluatePass and writeSummaryJSON (evaluator item 2: the all-skipped
// edge must not disagree between verdict and persisted summary).
func TestComputePassRateConsistency(t *testing.T) {
	cases := []struct {
		name   string
		format string
		c      logs.Counters
		want   float64
	}{
		{"none sentinel", "none", noResultCounters(), -1},
		{"empty sentinel", "", noResultCounters(), -1},
		{"all pass", "trx", logs.Counters{Total: 3, Passed: 3}, 1},
		{"one fail", "junit", logs.Counters{Total: 4, Passed: 3, Failed: 1}, 0.75},
		{"skipped excluded", "trx", logs.Counters{Total: 4, Passed: 3, Skipped: 1}, 1},
		{"zero tests", "trx", logs.Counters{}, 0},
		{"all skipped", "trx", logs.Counters{Total: 5, Skipped: 5}, 1},
	}
	for _, c := range cases {
		if got := computePassRate(c.format, c.c); got != c.want {
			t.Errorf("%s: computePassRate = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestWriteSummaryJSON verifies the writer persists the DESIGN §7.4 fields with
// a pass_rate that matches computePassRate, AND that a write failure is returned
// (evaluator item 3: summary.json is a required artifact — no silent success).
func TestWriteSummaryJSON(t *testing.T) {
	tr := &spec.TestRun{
		Metadata:     spec.Metadata{Name: "smoke"},
		Artifact:     spec.Artifact{Version: "1.1.0"},
		Target:       spec.Target{Hosts: []string{"LAB-01"}},
		Results:      spec.Results{Format: "trx"},
		PassCriteria: spec.PassCriteria{ExitCodes: []int{0}, MinPassRate: ptrF(0.95)},
	}
	out := &TestOutcome{Passed: true, ExitCode: 0, Total: 4, PassedTests: 3, FailedTests: 0, SkippedTests: 1, DurationSeconds: 12}

	dir := t.TempDir()
	if err := writeSummaryJSON(dir, tr, out, time.Unix(0, 0)); err != nil {
		t.Fatalf("writeSummaryJSON: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary.json: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal summary.json: %v", err)
	}
	if doc["host"] != "lab-01" { // lower-cased
		t.Errorf("host = %v, want lab-01", doc["host"])
	}
	// pass_rate must equal computePassRate for the same counters (3/(4-1)=1.0).
	if got := doc["pass_rate"].(float64); got != 1.0 {
		t.Errorf("pass_rate = %v, want 1.0", got)
	}
	for _, k := range []string{"name", "passed", "exit_code", "total", "passed_tests", "failed", "skipped", "criteria", "error_code"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("summary.json missing required key %q", k)
		}
	}

	// A non-existent destination dir must surface a write error, not be swallowed.
	missing := filepath.Join(dir, "no", "such", "dir")
	if err := writeSummaryJSON(missing, tr, out, time.Unix(0, 0)); err == nil {
		t.Fatal("writeSummaryJSON to a non-existent dir should return an error")
	}
}

