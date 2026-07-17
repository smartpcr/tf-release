package engine

import (
	"context"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Rollback matrix (DESIGN §10.2 / implementation-plan.md:222). A fake transport
// scripts a failure at each mutating state-machine step; the resulting on-target
// state must match the matrix row:
//   - pre-switch steps (FETCH/CHECKSUM/EXTRACT/RENDER) → staging-only failure:
//     the previous release + running service are untouched (update) or the
//     target is left with no service/junction/manifest (fresh);
//   - switch steps (SWITCH/CONFIGURE/START/HEALTH) → fresh install is CLEANED,
//     an update is RESTORED to the previous version.
// A separate pair of tests drives the ERR_ROLLBACK_FAILED path (DESIGN §10.6)
// where the rollback itself fails and the machine is reported UNKNOWN.
// ----------------------------------------------------------------------------

type matrixRow struct {
	name   string
	inject func(f *fakeHost)
	check  func(t *testing.T, f *fakeHost, err error)
}

func manifestOf(f *fakeHost) string {
	return string(f.files[`C:\deploy\sample-svc\manifest.json`])
}

func mustErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected deploy failure, got nil")
	}
}

// TestRollbackMatrixFresh injects a failure at each mutating step of a FRESH
// install and asserts the machine is left clean (no service, no junction, no
// manifest) — DESIGN §10.2 row "fresh".
func TestRollbackMatrixFresh(t *testing.T) {
	cleaned := func(t *testing.T, f *fakeHost, err error) {
		mustErr(t, err)
		if _, has := f.files[`C:\deploy\sample-svc\manifest.json`]; has {
			t.Fatalf("fresh failure must leave NO manifest; log=%v", f.log)
		}
		if f.svc != "" {
			t.Fatalf("fresh failure must leave NO service, got %q", f.svc)
		}
		if f.current != "" {
			t.Fatalf("fresh failure must leave NO junction, got %q", f.current)
		}
	}
	rows := []matrixRow{
		{"fetch", func(f *fakeHost) { f.fail["fetch"] = true }, cleaned},
		{"checksum", func(f *fakeHost) { f.fail["checksum"] = true }, cleaned},
		{"extract", func(f *fakeHost) { f.fail["extract"] = true }, cleaned},
		{"switch", func(f *fakeHost) { f.fail["switch"] = true }, cleaned},
		{"configure", func(f *fakeHost) { f.fail["configure"] = true }, cleaned},
		{"start", func(f *fakeHost) { f.fail["start"] = true }, cleaned},
		{"health", func(f *fakeHost) { f.fail["health"] = true }, cleaned},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			url, sum, done := testArtifactServer(t, []byte("fresh-"+r.name))
			defer done()
			f := newFakeHost("lab-01")
			r.inject(f)
			eng := engineWith(f)
			_, err := eng.Deploy(context.Background(), winSvcSpec(t, url, sum))
			r.check(t, f, err)
		})
	}
}

// TestRollbackMatrixUpdate injects a failure at each mutating step of an UPDATE
// (1.0.0 → 2.0.0). Pre-switch failures leave 1.0.0 intact and running;
// switch-phase failures roll the machine back to 1.0.0 — DESIGN §10.2 row
// "update".
func TestRollbackMatrixUpdate(t *testing.T) {
	prevIntact := func(t *testing.T, f *fakeHost, err error) {
		mustErr(t, err)
		if !strings.HasSuffix(f.current, `releases\1.0.0`) {
			t.Fatalf("pre-switch failure must keep junction at 1.0.0, got %q", f.current)
		}
		m := manifestOf(f)
		if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "success"`) {
			t.Fatalf("pre-switch failure must leave the 1.0.0 success manifest untouched: %s", m)
		}
		if _, has := f.files[`C:\deploy\sample-svc\releases\2.0.0\.labdeploy-release.json`]; has {
			t.Fatalf("incomplete 2.0.0 release marker must be removed on staging failure")
		}
	}
	restored := func(t *testing.T, f *fakeHost, err error) {
		mustErr(t, err)
		if !strings.Contains(err.Error(), "rolled back to 1.0.0") {
			t.Fatalf("switch-phase failure must roll back to 1.0.0, got: %v", err)
		}
		if !strings.HasSuffix(f.current, `releases\1.0.0`) {
			t.Fatalf("rollback must restore junction to 1.0.0, got %q", f.current)
		}
		m := manifestOf(f)
		if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "rolled_back"`) {
			t.Fatalf("rollback manifest must record 1.0.0 rolled_back: %s", m)
		}
	}
	rows := []matrixRow{
		{"fetch", func(f *fakeHost) { f.fail["fetch"] = true }, prevIntact},
		{"checksum", func(f *fakeHost) { f.fail["checksum"] = true }, prevIntact},
		{"extract", func(f *fakeHost) { f.fail["extract"] = true }, prevIntact},
		{"switch", func(f *fakeHost) { f.failN["switch"] = 1 }, restored},
		{"configure", func(f *fakeHost) { f.failN["configure"] = 1 }, restored},
		{"start", func(f *fakeHost) { f.failN["start"] = 1 }, restored},
		{"health", func(f *fakeHost) {
			// Health fails only while the junction points at the NEW release, so
			// the post-rollback probe against 1.0.0 succeeds.
			f.healthGate = func() bool { return strings.HasSuffix(f.current, `2.0.0`) }
		}, restored},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			url1, sum1, done1 := testArtifactServer(t, []byte("v1-"+r.name))
			defer done1()
			f := newFakeHost("lab-01")
			eng := engineWith(f)
			if _, err := eng.Deploy(context.Background(), winSvcSpec(t, url1, sum1)); err != nil {
				t.Fatalf("v1 deploy: %v", err)
			}
			url2, sum2, done2 := testArtifactServer(t, []byte("v2-"+r.name))
			defer done2()
			d2 := winSvcSpecVersion(t, url2, sum2, "2.0.0")
			r.inject(f)
			f.log = nil
			_, err := eng.Deploy(context.Background(), d2)
			r.check(t, f, err)
		})
	}
}

// TestUpdateRollbackFailedUnknownState covers DESIGN §10.6: when the forward
// deploy fails AND the rollback also fails (health never recovers), Deploy
// returns ERR_ROLLBACK_FAILED with a detail starting "MACHINE IN UNKNOWN STATE
// host=<h>" rather than falsely reporting a restored machine.
func TestUpdateRollbackFailedUnknownState(t *testing.T) {
	url1, sum1, done1 := testArtifactServer(t, []byte("v1"))
	defer done1()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	if _, err := eng.Deploy(context.Background(), winSvcSpec(t, url1, sum1)); err != nil {
		t.Fatalf("v1 deploy: %v", err)
	}
	url2, sum2, done2 := testArtifactServer(t, []byte("v2"))
	defer done2()
	d2 := winSvcSpecVersion(t, url2, sum2, "2.0.0")
	// Health fails on EVERY probe: forward health fails → rollback → rollback
	// health also fails → ERR_ROLLBACK_FAILED.
	f.fail["health"] = true
	_, err := eng.Deploy(context.Background(), d2)
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_ROLLBACK_FAILED" {
		t.Fatalf("want ERR_ROLLBACK_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "MACHINE IN UNKNOWN STATE host=lab-01") {
		t.Fatalf("want MACHINE IN UNKNOWN STATE host=lab-01 detail, got: %v", err)
	}
	if !strings.Contains(manifestOf(f), `"result": "failed"`) {
		t.Fatalf("failed manifest must be persisted before ERR_ROLLBACK_FAILED: %s", manifestOf(f))
	}
}

// TestFreshRollbackFailedUnknownState covers DESIGN §10.6 on the FRESH path:
// the install fails at START and the cleanup itself fails (Uninstall errors), so
// the machine cannot be certified clean and ERR_ROLLBACK_FAILED is returned.
func TestFreshRollbackFailedUnknownState(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("fresh"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["start"] = true     // forward install fails at START → clean rollback
	f.fail["uninstall"] = true // cleanup Uninstall fails → machine NOT clean
	eng := engineWith(f)
	_, err := eng.Deploy(context.Background(), winSvcSpec(t, url, sum))
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_ROLLBACK_FAILED" {
		t.Fatalf("want ERR_ROLLBACK_FAILED on failed fresh cleanup, got %v", err)
	}
	if !strings.Contains(err.Error(), "MACHINE IN UNKNOWN STATE host=lab-01") {
		t.Fatalf("want MACHINE IN UNKNOWN STATE host=lab-01 detail, got: %v", err)
	}
}
