package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

func testPaths() layout.Paths {
	return layout.NewPaths(spec.OSWindows, `C:\deploy`, "sample-svc", "1.0.0")
}

// TestManifestRoundTrip writes a fully-populated manifest through the fake
// transport and reads it back, asserting every field — including the nested
// last_operation — survives the JSON round-trip unchanged (Stage 3.2 scenario
// "Manifest round-trip").
func TestManifestRoundTrip(t *testing.T) {
	f := newFakeHost("lab-01")
	p := testPaths()
	in := &Manifest{
		Schema:           1,
		App:              "sample-svc",
		Pattern:          "windows_service",
		CurrentVersion:   "1.2.0",
		PreviousVersion:  "1.1.0",
		CurrentRelease:   `C:\deploy\sample-svc\releases\1.2.0`,
		ArtifactChecksum: "sha256:deadbeef",
		ProviderVersion:  ProviderVersion,
		Extra:            map[string]string{"pipeline": "run-42", "env": "lab"},
		LastOperation: LastOp{
			Type:     "deploy",
			Result:   "success",
			Started:  "2026-07-17T00:00:00Z",
			Finished: "2026-07-17T00:01:00Z",
		},
	}
	if err := WriteManifest(context.Background(), f, p, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := ReadManifest(context.Background(), f, p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out == nil {
		t.Fatal("read returned nil for a written manifest")
	}
	if out.Schema != in.Schema || out.App != in.App || out.Pattern != in.Pattern ||
		out.CurrentVersion != in.CurrentVersion || out.PreviousVersion != in.PreviousVersion ||
		out.CurrentRelease != in.CurrentRelease || out.ArtifactChecksum != in.ArtifactChecksum ||
		out.ProviderVersion != in.ProviderVersion {
		t.Fatalf("scalar field drift:\n in=%+v\nout=%+v", in, out)
	}
	if out.LastOperation != in.LastOperation {
		t.Fatalf("last_operation drift:\n in=%+v\nout=%+v", in.LastOperation, out.LastOperation)
	}
	if len(out.Extra) != len(in.Extra) {
		t.Fatalf("extra length drift: in=%v out=%v", in.Extra, out.Extra)
	}
	for k, v := range in.Extra {
		if out.Extra[k] != v {
			t.Fatalf("extra[%q] drift: in=%q out=%q", k, v, out.Extra[k])
		}
	}
}

// TestAcquireLockFreshVsStale drives AcquireLock against a held lock in two
// states: a FRESH held lock must yield ERR_LOCKED naming the owner, while an
// AGED lock (older than lock_timeout_seconds) must be overridden and return a
// non-empty stale WARN (Stage 3.2 scenario "Lock fresh vs stale").
func TestAcquireLockFreshVsStale(t *testing.T) {
	p := testPaths()
	const timeoutSec = 900

	// --- Fresh: held < timeout ⇒ ERR_LOCKED naming the owner. ---
	fresh := newFakeHost("lab-01")
	fresh.files[p.Lock] = []byte(fmt.Sprintf(
		`{"owner":"ci-runner-7","op":"deploy","started_utc":"%s"}`,
		time.Now().UTC().Format(time.RFC3339)))
	warn, err := AcquireLock(context.Background(), fresh, p, "me", "deploy", timeoutSec)
	var ce *CodedError
	if err == nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
		t.Fatalf("fresh lock: want ERR_LOCKED, got warn=%q err=%v", warn, err)
	}
	if !strings.Contains(err.Error(), "ci-runner-7") {
		t.Fatalf("fresh lock error must name the owner, got: %v", err)
	}
	if warn != "" {
		t.Fatalf("fresh lock must not emit a stale WARN, got: %q", warn)
	}

	// --- Stale: held ≥ timeout ⇒ overwrite + WARN, no error. ---
	stale := newFakeHost("lab-01")
	aged := time.Now().UTC().Add(-time.Duration(timeoutSec+60) * time.Second).Format(time.RFC3339)
	stale.files[p.Lock] = []byte(fmt.Sprintf(
		`{"owner":"dead-runner","op":"deploy","started_utc":"%s"}`, aged))
	warn, err = AcquireLock(context.Background(), stale, p, "me", "deploy", timeoutSec)
	if err != nil {
		t.Fatalf("stale lock: want override success, got err=%v", err)
	}
	if warn == "" || !strings.Contains(warn, "stale") || !strings.Contains(warn, "dead-runner") {
		t.Fatalf("stale lock must emit a WARN naming the prior owner, got: %q", warn)
	}
	// The lock file now records the new owner.
	if !strings.Contains(string(stale.files[p.Lock]), `"owner":"me"`) {
		t.Fatalf("stale lock not overwritten by new owner: %s", stale.files[p.Lock])
	}
}

// TestAcquireLockUnparseableRefused is the regression for evaluator item 1: a
// held lock whose metadata does not parse (bad JSON) or whose started_utc is
// invalid is NOT proof of staleness and MUST yield ERR_LOCKED, never a silent
// override — even when timeoutSec is tiny.
func TestAcquireLockUnparseableRefused(t *testing.T) {
	p := testPaths()
	const tinyTimeout = 1 // seconds: an old-but-unparseable lock must still be refused

	cases := []struct {
		name    string
		content string
	}{
		{"invalid json", `this-is-not-json{{`},
		{"empty object (missing started_utc)", `{}`},
		{"bad started_utc", `{"owner":"x","op":"deploy","started_utc":"not-a-timestamp"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHost("lab-01")
			f.files[p.Lock] = []byte(tc.content)
			warn, err := AcquireLock(context.Background(), f, p, "me", "deploy", tinyTimeout)
			var ce *CodedError
			if err == nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
				t.Fatalf("want ERR_LOCKED for unparseable lock, got warn=%q err=%v", warn, err)
			}
			// The original (unparseable) lock must be untouched — no override.
			if string(f.files[p.Lock]) != tc.content {
				t.Fatalf("unparseable lock must NOT be overwritten; got %s", f.files[p.Lock])
			}
		})
	}
}

// TestDestroyPurgeReleasesLockOnTreeFailure is the regression for evaluator
// item 2: when purge's recursive tree removal fails, the deferred ReleaseLock
// must still remove the .lock so a retry is not stranded behind a stale lock.
func TestDestroyPurgeReleasesLockOnTreeFailure(t *testing.T) {
	payload := []byte("v1")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	d := winSvcSpec(t, url, sum)
	if _, err := eng.Deploy(context.Background(), d); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	// Force the purge tree removal to fail AFTER the lock has been acquired.
	f.fail["rmtree"] = true
	err := eng.Destroy(context.Background(), d, "purge")
	if err == nil {
		t.Fatal("expected purge to surface the tree-removal failure")
	}
	if _, held := f.files[testPaths().Lock]; held {
		t.Fatalf("lock stranded after failed purge: %s", testPaths().Lock)
	}
}

// TestReadStatusPropagatesStatusError is the regression for evaluator item 5:
// a failed pattern.Status probe must make ReadStatus return an error, never a
// Status carrying an empty service_status.
func TestReadStatusPropagatesStatusError(t *testing.T) {
	payload := []byte("v1")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	// Seed a present, healthy manifest so ReadStatus proceeds to the status probe.
	m := &Manifest{
		Schema: 1, App: "sample-svc", Pattern: "windows_service",
		CurrentVersion: "1.0.0", CurrentRelease: `C:\deploy\sample-svc\releases\1.0.0`,
		ProviderVersion: ProviderVersion,
		LastOperation:   LastOp{Type: "deploy", Result: "success", Started: nowRFC3339()},
	}
	if err := WriteManifest(context.Background(), f, testPaths(), m); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	f.svc = "Running"
	f.fail["status"] = true

	eng := engineWith(f)
	st, err := eng.ReadStatus(context.Background(), winSvcSpec(t, url, sum))
	if err == nil {
		t.Fatalf("expected ReadStatus to propagate the status probe failure, got st=%+v", st)
	}
	if st != nil {
		t.Fatalf("failed status probe must not yield a Status, got %+v", st)
	}
	var ce *CodedError
	if !asCoded(err, &ce) {
		t.Fatalf("want a coded error, got %v", err)
	}
}

// TestReleaseLockRunsWithCanceledContext is the regression for evaluator item 3:
// the deferred lock cleanup must run even when the operation context is already
// canceled — lockCleanupContext detaches from cancellation.
func TestReleaseLockRunsWithCanceledContext(t *testing.T) {
	p := testPaths()
	f := newFakeHost("lab-01")
	f.files[p.Lock] = []byte(`{"owner":"me","op":"deploy","started_utc":"` + nowRFC3339() + `"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	rctx, rcancel := lockCleanupContext(ctx)
	defer rcancel()
	if rctx.Err() != nil {
		t.Fatalf("cleanup context must not inherit cancellation, got %v", rctx.Err())
	}
	ReleaseLock(rctx, f, p)
	if _, held := f.files[p.Lock]; held {
		t.Fatal("lock not released via detached cleanup context")
	}
}

// TestAcquireLockFreshWins covers the uncontended path: no lock present ⇒ lock
// created, no WARN, no error.
func TestAcquireLockFreshWins(t *testing.T) {
	p := testPaths()
	f := newFakeHost("lab-01")
	warn, err := AcquireLock(context.Background(), f, p, "me", "deploy", 900)
	if err != nil || warn != "" {
		t.Fatalf("uncontended acquire: warn=%q err=%v", warn, err)
	}
	if _, held := f.files[p.Lock]; !held {
		t.Fatal("lock file was not created")
	}
}

// TestReconcileManifest exercises the DESIGN §10.4 Read-refresh helper directly.
func TestReconcileManifest(t *testing.T) {
	// Absent ⇒ not present (caller RemoveResource).
	if present, _, _ := ReconcileManifest(nil); present {
		t.Fatal("nil manifest must report not present")
	}
	// Success ⇒ clean version, no warn.
	ok := &Manifest{CurrentVersion: "1.0.0", LastOperation: LastOp{Type: "deploy", Result: "success"}}
	present, ver, warn := ReconcileManifest(ok)
	if !present || ver != "1.0.0" || warn != "" {
		t.Fatalf("success reconcile: present=%v ver=%q warn=%q", present, ver, warn)
	}
	// Failed ⇒ !failed marker + warn.
	bad := &Manifest{CurrentVersion: "2.0.0", LastOperation: LastOp{Type: "deploy", Result: "failed", Started: "2026-07-17T00:00:00Z"}}
	present, ver, warn = ReconcileManifest(bad)
	if !present || ver != "2.0.0"+FailedMarker || warn == "" {
		t.Fatalf("failed reconcile: present=%v ver=%q warn=%q", present, ver, warn)
	}
}

// TestReadStatusFailedMarker asserts the reconciliation surfaces end-to-end
// through ReadStatus: a manifest whose last_operation.result==failed makes
// ReadStatus report "<version>!failed" and register a warning.
func TestReadStatusFailedMarker(t *testing.T) {
	payload := []byte("v1")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	// Seed a failed manifest on the target.
	m := &Manifest{
		Schema: 1, App: "sample-svc", Pattern: "windows_service",
		CurrentVersion: "1.0.0", CurrentRelease: `C:\deploy\sample-svc\releases\1.0.0`,
		ProviderVersion: ProviderVersion,
		LastOperation:   LastOp{Type: "deploy", Result: "failed", Started: nowRFC3339()},
	}
	if err := WriteManifest(context.Background(), f, testPaths(), m); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	f.svc = "Stopped"

	eng := engineWith(f)
	st, err := eng.ReadStatus(context.Background(), winSvcSpec(t, url, sum))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if st == nil {
		t.Fatal("read returned absent for a present (failed) manifest")
	}
	if st.DeployedVersion != "1.0.0"+FailedMarker {
		t.Fatalf("want deployed_version %q, got %q", "1.0.0"+FailedMarker, st.DeployedVersion)
	}
	if len(eng.Warnings) == 0 || !strings.Contains(strings.Join(eng.Warnings, " "), "failed") {
		t.Fatalf("expected a failed-operation warning, got %v", eng.Warnings)
	}
}
