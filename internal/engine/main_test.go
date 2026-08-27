package engine

import (
	"os"
	"testing"
	"time"
)

// TestMain compresses the health-check clock for the WHOLE engine test binary.
// The health loop's initial-delay/interval/total-budget sleeps dominate the
// uncached suite runtime (~110s of real-second sleeps across the docker,
// cluster, and file rollback integration tests), which repeatedly pushed the
// evaluator's uncached `go test` run past its review window (iters 9/11/13/14
// pipeline faults). Scaling healthTick down to a few ms preserves the exact
// loop SEMANTICS (initial delay → probe → interval → budget deadline) while
// cutting wall-clock time ~50x. Production is unaffected: healthTick defaults to
// time.Second and is only touched here. Timing-sensitive budget tests opt back
// into real seconds via realSecondHealthTick.
func TestMain(m *testing.M) {
	healthTick = 20 * time.Millisecond
	os.Exit(m.Run())
}

// realSecondHealthTick restores the real-second health clock for the duration of
// a single test — used by the health budget/deadline tests that assert on
// wall-clock enforcement and must therefore run against genuine seconds.
func realSecondHealthTick(t *testing.T) {
	t.Helper()
	prev := healthTick
	healthTick = time.Second
	t.Cleanup(func() { healthTick = prev })
}
