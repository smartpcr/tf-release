package logs

import (
	"path/filepath"
	"testing"
)

// TestParseCommittedFixtures exercises the committed TRX/JUnit fixtures under
// testdata/ (Scenario: "TRX and JUnit counters", T8). It proves:
//   - TRX parsing is namespace-insensitive (fixtures carry the VS xmlns).
//   - counters are summed across multiple result files.
func TestParseCommittedFixtures(t *testing.T) {
	// Single namespaced TRX file.
	c, err := ParseTRX(filepath.Join("testdata", "trx", "run1.trx"))
	if err != nil {
		t.Fatalf("ParseTRX run1: %v", err)
	}
	if (c != Counters{Total: 10, Passed: 7, Failed: 2, Skipped: 1}) {
		t.Fatalf("run1 counters: %+v", c)
	}

	// Sum across BOTH namespaced TRX fixtures: 10+5 / 7+5 / 2+0 / 1+0.
	sum, files, err := SumResults("trx", "testdata", []string{"trx/*.trx"})
	if err != nil {
		t.Fatalf("SumResults trx: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 trx files, got %d", len(files))
	}
	if (sum != Counters{Total: 15, Passed: 12, Failed: 2, Skipped: 1}) {
		t.Fatalf("trx sum counters: %+v", sum)
	}

	// Multi-suite JUnit: tests 4+6, failures 1, errors 1, skipped 1.
	jc, err := ParseJUnit(filepath.Join("testdata", "junit", "suites.xml"))
	if err != nil {
		t.Fatalf("ParseJUnit suites: %v", err)
	}
	if (jc != Counters{Total: 10, Passed: 7, Failed: 2, Skipped: 1}) {
		t.Fatalf("junit multi counters: %+v", jc)
	}

	// Sum across multi + single suite files: 10+3 total, 7+3 passed.
	jsum, jfiles, err := SumResults("junit", "testdata", []string{"junit/*.xml"})
	if err != nil {
		t.Fatalf("SumResults junit: %v", err)
	}
	if len(jfiles) != 2 {
		t.Fatalf("want 2 junit files, got %d", len(jfiles))
	}
	if (jsum != Counters{Total: 13, Passed: 10, Failed: 2, Skipped: 1}) {
		t.Fatalf("junit sum counters: %+v", jsum)
	}
}
