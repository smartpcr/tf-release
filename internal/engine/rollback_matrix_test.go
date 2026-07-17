package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
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
	name         string
	inject       func(f *fakeHost)
	check        func(t *testing.T, f *fakeHost, err error)
	spec         func(t *testing.T, url, sum string) *spec.Deployment // nil ⇒ winSvcSpec
	newTransErr  bool                                                 // VALIDATE row: NewTransport fails
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

// stagingEmpty asserts the staging tree was wiped: no file key lives under a
// `\staging\` path (DESIGN §10.2 "wipe staging" / §9.1 wiped at end of every op).
func stagingEmpty(t *testing.T, f *fakeHost) {
	t.Helper()
	for k := range f.files {
		if strings.Contains(k, `\staging\`) {
			t.Fatalf("staging must be empty after a pre-switch failure; found %q", k)
		}
	}
	for d := range f.dirs {
		if strings.Contains(d, `\staging\`) {
			t.Fatalf("staging dir must be wiped after a pre-switch failure; found %q", d)
		}
	}
}

// noRelease asserts no version release tree survives (fresh clean).
func noRelease(t *testing.T, f *fakeHost) {
	t.Helper()
	for k := range f.files {
		if strings.Contains(k, `\releases\`) {
			t.Fatalf("fresh failure must leave NO release tree; found %q", k)
		}
	}
}

func (r matrixRow) makeSpec(t *testing.T, url, sum string) *spec.Deployment {
	if r.spec != nil {
		return r.spec(t, url, sum)
	}
	return winSvcSpec(t, url, sum)
}

// TestRollbackMatrixFresh injects a failure at each mutating step of a FRESH
// install and asserts the machine is left clean (no service, no junction, no
// manifest) — DESIGN §10.2 rows "no mutation", "staging only", and fresh
// "switch". Pre-switch rows additionally assert staging is wiped.
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
	// preSwitch: nothing mutated / staging wiped, plus no release tree left behind.
	preSwitch := func(t *testing.T, f *fakeHost, err error) {
		cleaned(t, f, err)
		stagingEmpty(t, f)
		noRelease(t, f)
	}
	rows := []matrixRow{
		{name: "validate", newTransErr: true, check: cleaned},
		{name: "connect", inject: func(f *fakeHost) { f.fail["connect"] = true }, check: cleaned},
		{name: "preflight", inject: func(f *fakeHost) { f.fail["preflight"] = true }, check: cleaned},
		{name: "lock", inject: func(f *fakeHost) { f.fail["lock"] = true }, check: cleaned},
		{name: "stage", inject: func(f *fakeHost) { f.fail["stage"] = true }, check: preSwitch},
		{name: "fetch", inject: func(f *fakeHost) { f.fail["fetch"] = true }, check: preSwitch},
		{name: "checksum", inject: func(f *fakeHost) { f.fail["checksum"] = true }, check: preSwitch},
		{name: "extract", inject: func(f *fakeHost) { f.fail["extract"] = true }, check: preSwitch},
		{name: "render", inject: func(f *fakeHost) { f.fail["render"] = true }, check: preSwitch, spec: winSvcSpecRender},
		{name: "switch", inject: func(f *fakeHost) { f.fail["switch"] = true }, check: cleaned},
		{name: "configure", inject: func(f *fakeHost) { f.fail["configure"] = true }, check: cleaned},
		{name: "start", inject: func(f *fakeHost) { f.fail["start"] = true }, check: cleaned},
		{name: "health", inject: func(f *fakeHost) { f.fail["health"] = true }, check: cleaned},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			url, sum, done := testArtifactServer(t, []byte("fresh-"+r.name))
			defer done()
			f := newFakeHost("lab-01")
			if r.inject != nil {
				r.inject(f)
			}
			eng := engineWith(f)
			if r.newTransErr {
				eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
					return nil, fmt.Errorf("simulated NewTransport (VALIDATE) failure")
				}
			}
			_, err := eng.Deploy(context.Background(), r.makeSpec(t, url, sum))
			r.check(t, f, err)
		})
	}
}

// TestRollbackMatrixUpdate injects a failure at each mutating step of an UPDATE
// (1.0.0 → 2.0.0). No-mutation and staging-only failures leave 1.0.0 intact and
// running (staging wiped); switch-phase failures roll the machine back to 1.0.0
// — DESIGN §10.2 rows "no mutation", "staging only", update "switch".
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
	}
	// stagingWiped: prev intact AND staging emptied AND no partial 2.0.0 release.
	stagingWiped := func(t *testing.T, f *fakeHost, err error) {
		prevIntact(t, f, err)
		stagingEmpty(t, f)
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
		{name: "validate", newTransErr: true, check: prevIntact},
		{name: "connect", inject: func(f *fakeHost) { f.fail["connect"] = true }, check: prevIntact},
		{name: "preflight", inject: func(f *fakeHost) { f.fail["preflight"] = true }, check: prevIntact},
		{name: "lock", inject: func(f *fakeHost) { f.fail["lock"] = true }, check: prevIntact},
		{name: "stage", inject: func(f *fakeHost) { f.fail["stage"] = true }, check: stagingWiped},
		{name: "fetch", inject: func(f *fakeHost) { f.fail["fetch"] = true }, check: stagingWiped},
		{name: "checksum", inject: func(f *fakeHost) { f.fail["checksum"] = true }, check: stagingWiped},
		{name: "extract", inject: func(f *fakeHost) { f.fail["extract"] = true }, check: stagingWiped},
		{name: "render", inject: func(f *fakeHost) { f.fail["render"] = true }, check: stagingWiped, spec: winSvcSpecRender},
		{name: "switch", inject: func(f *fakeHost) { f.failN["switch"] = 1 }, check: restored},
		{name: "configure", inject: func(f *fakeHost) { f.failN["configure"] = 1 }, check: restored},
		{name: "start", inject: func(f *fakeHost) { f.failN["start"] = 1 }, check: restored},
		{name: "health", inject: func(f *fakeHost) {
			// Health fails only while the junction points at the NEW release, so
			// the post-rollback probe against 1.0.0 succeeds.
			f.healthGate = func() bool { return strings.HasSuffix(f.current, `2.0.0`) }
		}, check: restored},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			url1, sum1, done1 := testArtifactServer(t, []byte("v1-"+r.name))
			defer done1()
			f := newFakeHost("lab-01")
			eng := engineWith(f)
			// v1 install uses the SAME spec shape as the update so a rendered file
			// (when present) exists for the render row's update leg.
			if _, err := eng.Deploy(context.Background(), r.makeSpec(t, url1, sum1)); err != nil {
				t.Fatalf("v1 deploy: %v", err)
			}
			url2, sum2, done2 := testArtifactServer(t, []byte("v2-"+r.name))
			defer done2()
			d2 := r.makeSpec(t, url2, sum2)
			d2.Artifact.Version = "2.0.0"
			sum := sha256.Sum256([]byte("v2-" + r.name))
			d2.Artifact.Checksum = "sha256:" + hex.EncodeToString(sum[:])
			if r.inject != nil {
				r.inject(f)
			}
			if r.newTransErr {
				// VALIDATE row: fail NewTransport on the UPDATE only (after v1 is
				// already deployed) so the prior version stays intact.
				eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
					return nil, fmt.Errorf("simulated NewTransport (VALIDATE) failure")
				}
			}
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

// TestFreshRollbackDisabledPersistsFailed covers the DESIGN §10.2 last row on the
// FRESH path: with rollback_on_failure=false a failed install must still persist a
// manifest recording last_operation.result=failed at the ATTEMPTED version (there
// is no previous version), so Read reports drift. Regression guard for the bug
// where rollbackSingle passed an empty prev to finalizeFailed, which no-op'd.
func TestFreshRollbackDisabledPersistsFailed(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("fresh-noroll"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["start"] = true // forward install fails at START (switch phase)
	d := winSvcSpec(t, url, sum)
	rb := false
	d.Strategy.RollbackOnFailure = &rb // §10.2 last row: leave target as-is
	eng := engineWith(f)
	_, err := eng.Deploy(context.Background(), d)
	mustErr(t, err)
	if !strings.Contains(err.Error(), "rollback_on_failure=false") {
		t.Fatalf("want rollback_on_failure=false surfaced, got: %v", err)
	}
	m := manifestOf(f)
	if m == "" {
		t.Fatalf("fresh rollback-disabled failure must persist a failed manifest; none written (log=%v)", f.log)
	}
	if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("failed manifest must record the attempted version 1.0.0 with result=failed: %s", m)
	}
}
