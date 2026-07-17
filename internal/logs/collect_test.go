package logs

import (
	"os"
	"path/filepath"
	"testing"
)

const trxFixture = `<?xml version="1.0" encoding="UTF-8"?>
<TestRun id="x" xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010">
  <ResultSummary outcome="Failed">
    <Counters total="10" executed="9" passed="7" failed="2" notExecuted="1" />
  </ResultSummary>
</TestRun>`

const junitMulti = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="a" tests="4" failures="1" errors="0" skipped="1"/>
  <testsuite name="b" tests="6" failures="0" errors="1" skipped="0"/>
</testsuites>`

const junitSingle = `<testsuite name="only" tests="3" failures="0" errors="0" skipped="0"/>`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseTRX(t *testing.T) {
	d := t.TempDir()
	c, err := ParseTRX(write(t, d, "r.trx", trxFixture))
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 10 || c.Passed != 7 || c.Failed != 2 || c.Skipped != 1 {
		t.Fatalf("counters: %+v", c)
	}
}

func TestParseJUnit(t *testing.T) {
	d := t.TempDir()
	c, err := ParseJUnit(write(t, d, "multi.xml", junitMulti))
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 10 || c.Failed != 2 || c.Skipped != 1 || c.Passed != 7 {
		t.Fatalf("multi counters: %+v", c)
	}
	c, err = ParseJUnit(write(t, d, "single.xml", junitSingle))
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 3 || c.Passed != 3 {
		t.Fatalf("single counters: %+v", c)
	}
}

func TestSumResults(t *testing.T) {
	d := t.TempDir()
	sub := filepath.Join(d, "TestResults")
	_ = os.MkdirAll(sub, 0o755)
	write(t, sub, "a.trx", trxFixture)
	write(t, sub, "b.trx", trxFixture)
	c, files, err := SumResults("trx", d, []string{"TestResults/*.trx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || c.Total != 20 || c.Passed != 14 {
		t.Fatalf("sum: files=%d %+v", len(files), c)
	}
	// no matches => error (E2E-06 upstream). Basename `*.absent` matches nothing.
	if _, _, err := SumResults("trx", d, []string{"nope/*.absent"}); err == nil {
		t.Fatal("expected no-match error")
	}
}

// TestSumResultsNoDoubleCount guards evaluator item 3: overlapping/duplicate
// patterns (or a nested extraction layout) must NOT cause a result file to be
// counted more than once.
func TestSumResultsNoDoubleCount(t *testing.T) {
	d := t.TempDir()
	nested := filepath.Join(d, "host-01", "TestResults")
	_ = os.MkdirAll(nested, 0o755)
	write(t, nested, "only.trx", trxFixture)
	// Same file reachable by three overlapping patterns; must still count once.
	c, files, err := SumResults("trx", d, []string{"*.trx", "*.trx", "TestResults/*.trx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want exactly 1 matched file, got %d: %v", len(files), files)
	}
	if c.Total != 10 || c.Passed != 7 || c.Failed != 2 || c.Skipped != 1 {
		t.Fatalf("double-counted counters: %+v", c)
	}
}

// TestSumResultsDirectoryAware guards evaluator item 5: a pattern with a
// directory component (`expected/*.trx`) must NOT match a misplaced file in a
// different directory (`other/a.trx`), even though basenames would match.
func TestSumResultsDirectoryAware(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "host-01", "expected")
	bad := filepath.Join(d, "host-01", "other")
	_ = os.MkdirAll(good, 0o755)
	_ = os.MkdirAll(bad, 0o755)
	write(t, good, "a.trx", trxFixture)
	write(t, bad, "a.trx", trxFixture) // stale/misplaced — must be ignored
	c, files, err := SumResults("trx", d, []string{"expected/*.trx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want only the expected/ file, got %d: %v", len(files), files)
	}
	if c.Total != 10 {
		t.Fatalf("misplaced file counted: %+v", c)
	}
	// A bare basename pattern still matches anywhere (both files).
	_, files2, err := SumResults("trx", d, []string{"*.trx"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files2) != 2 {
		t.Fatalf("bare basename should match both, got %d", len(files2))
	}
}
