package engine

import (
	"testing"

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
			// Zero tests discovered never clears the rate gate (E2E-07 edge).
			name: "trx zero tests", exit: 0, format: "trx",
			counters: logs.Counters{Total: 0, Passed: 0, Failed: 0, Skipped: 0},
			pc:       spec.PassCriteria{}, passed: false, exitOK: true, rateOK: false,
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
