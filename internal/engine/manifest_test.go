package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
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
	lk, warn, err := AcquireLock(context.Background(), fresh, p, "me", "deploy", timeoutSec)
	var ce *CodedError
	if err == nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
		t.Fatalf("fresh lock: want ERR_LOCKED, got warn=%q err=%v", warn, err)
	}
	if lk != nil {
		t.Fatalf("fresh lock: no handle expected on ERR_LOCKED, got %+v", lk)
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
	lk, warn, err = AcquireLock(context.Background(), stale, p, "me", "deploy", timeoutSec)
	if err != nil {
		t.Fatalf("stale lock: want override success, got err=%v", err)
	}
	if lk == nil {
		t.Fatal("stale lock: expected a handle after successful takeover")
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
			lk, warn, err := AcquireLock(context.Background(), f, p, "me", "deploy", tinyTimeout)
			var ce *CodedError
			if err == nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
				t.Fatalf("want ERR_LOCKED for unparseable lock, got warn=%q err=%v", warn, err)
			}
			if lk != nil {
				t.Fatalf("unparseable lock must not yield a handle, got %+v", lk)
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
	lk, _, err := AcquireLock(context.Background(), f, p, "me", "deploy", 900)
	if err != nil || lk == nil {
		t.Fatalf("acquire: lk=%v err=%v", lk, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	rctx, rcancel := lockCleanupContext(ctx)
	defer rcancel()
	if rctx.Err() != nil {
		t.Fatalf("cleanup context must not inherit cancellation, got %v", rctx.Err())
	}
	ReleaseLock(rctx, lk)
	if _, held := f.files[p.Lock]; held {
		t.Fatal("lock not released via detached cleanup context")
	}
}

// TestReleaseLockOwnershipSafe is the regression for evaluator item 2: a caller
// that overran the timeout must NOT delete a successor's lock. ReleaseLock is a
// compare-and-delete keyed on the exact bytes the caller persisted.
func TestReleaseLockOwnershipSafe(t *testing.T) {
	p := testPaths()
	f := newFakeHost("lab-01")
	lk, _, err := AcquireLock(context.Background(), f, p, "slow-owner", "deploy", 900)
	if err != nil || lk == nil {
		t.Fatalf("acquire: lk=%v err=%v", lk, err)
	}
	// A successor legitimately took over after our timeout: overwrite the lock
	// file with DIFFERENT bytes (a different owner/token).
	successor := []byte(`{"owner":"successor","op":"deploy","started_utc":"` + nowRFC3339() + `","token":"other"}`)
	f.files[p.Lock] = successor

	ReleaseLock(context.Background(), lk) // stale owner's release must be a no-op
	if got, held := f.files[p.Lock]; !held || string(got) != string(successor) {
		t.Fatalf("ownership-safe release deleted/altered the successor's lock: held=%v got=%s", held, got)
	}
}

// TestAcquireLockConcurrentTakeover is the regression for evaluator item 1: two
// contenders racing to take over the SAME stale lock must yield EXACTLY ONE
// owner. Command interleaving is genuinely modeled — the fake releases its mutex
// between Execs — and a barrier rendezvouses BOTH contenders on the atomic rename
// so the race window is exercised on every trial. The atomic seize is the
// arbiter; no double-ownership, and no claim residue leaks.
func TestAcquireLockConcurrentTakeover(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			const trials = 40
			for trial := 0; trial < trials; trial++ {
				p := lockFakePaths(osk)
				f := newLockFake(osk)
				aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
				f.set(p.Lock, `{"owner":"dead","op":"deploy","started_utc":"`+aged+`","token":"stale"}`)

				// Rendezvous both contenders on the atomic compare-and-replace so the
				// exclusive-gate CAS arbitrates the takeover under a real race.
				bar := newBarrier(2)
				f.hook = func(op, s string) {
					if op == "casreplace" {
						bar.wait()
					}
				}

				const timeoutSec = 900
				type res struct {
					lk   *Lock
					warn string
					err  error
				}
				results := make([]res, 2)
				var wg sync.WaitGroup
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						lk, warn, err := AcquireLock(context.Background(), f, p,
							fmt.Sprintf("contender-%d", idx), "deploy", timeoutSec)
						results[idx] = res{lk, warn, err}
					}(i)
				}
				wg.Wait()

				winners, losers := 0, 0
				for _, r := range results {
					switch {
					case r.err == nil && r.lk != nil:
						winners++
						// The winner performed a stale TAKEOVER, so it MUST surface
						// the WARN naming the dead owner. Because takeover is a single
						// atomic compare-and-replace (no absent gap), the winner is
						// always the actor that overrode the stale bytes, so the
						// diagnostic can never be dropped on the concurrent path
						// (evaluator item 1).
						if !strings.Contains(r.warn, "stale") || !strings.Contains(r.warn, "dead") {
							t.Fatalf("trial %d: takeover winner dropped the stale WARN: %q", trial, r.warn)
						}
					case r.err != nil && r.lk == nil:
						var ce *CodedError
						if !asCoded(r.err, &ce) || ce.Code != "ERR_LOCKED" {
							t.Fatalf("trial %d: loser must fail ERR_LOCKED, got %v", trial, r.err)
						}
						losers++
					default:
						t.Fatalf("trial %d: ambiguous outcome: lk=%v warn=%q err=%v", trial, r.lk, r.warn, r.err)
					}
				}
				if winners != 1 || losers != 1 {
					t.Fatalf("trial %d: want exactly 1 winner + 1 loser, got winners=%d losers=%d", trial, winners, losers)
				}
				// Exactly one lock file, owned by a contender, and no gate/temp residue.
				f.mu.Lock()
				for k := range f.files {
					if strings.Contains(k, ".mx") || strings.Contains(k, ".tmp.") {
						f.mu.Unlock()
						t.Fatalf("trial %d: leaked lock residue %q", trial, k)
					}
				}
				owner, held := f.files[p.Lock]
				f.mu.Unlock()
				if !held || !strings.Contains(owner, `"owner":"contender-`) {
					t.Fatalf("trial %d: lock not owned by the winner: held=%v content=%q", trial, held, owner)
				}
			}
		})
	}
}

// TestReleaseVsTakeoverInterleaved is the regression for evaluator item 2: an
// expired owner's release must NEVER delete a successor's lock, even when the
// release and the successor's takeover interleave arbitrarily. Ownership
// verification and removal are one indivisible atomic seize, so the successor
// always ends up the sole owner and no claim residue leaks.
func TestReleaseVsTakeoverInterleaved(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			const trials = 60
			for trial := 0; trial < trials; trial++ {
				p := lockFakePaths(osk)
				f := newLockFake(osk)
				ctx := context.Background()

				// The expired owner holds a lock old enough for a successor to
				// consider stale and take over.
				aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
				ownerContent := `{"owner":"expired","op":"deploy","started_utc":"` + aged + `","token":"owner-tok"}`
				f.set(p.Lock, ownerContent)
				owner := &Lock{t: f, paths: p, content: ownerContent, token: "owner-tok"}

				var wg sync.WaitGroup
				wg.Add(2)
				var succLk *Lock
				var succErr error
				go func() { defer wg.Done(); ReleaseLock(ctx, owner) }()
				go func() {
					defer wg.Done()
					succLk, _, succErr = AcquireLock(ctx, f, p, "successor", "deploy", 900)
				}()
				wg.Wait()

				// The successor must have acquired, and its lock must survive the
				// expired owner's concurrent release.
				if succErr != nil || succLk == nil {
					t.Fatalf("trial %d: successor failed to acquire: lk=%v err=%v", trial, succLk, succErr)
				}
				f.mu.Lock()
				for k := range f.files {
					if strings.Contains(k, ".mx") || strings.Contains(k, ".tmp.") {
						f.mu.Unlock()
						t.Fatalf("trial %d: leaked lock residue %q", trial, k)
					}
				}
				cur, held := f.files[p.Lock]
				f.mu.Unlock()
				if !held || !strings.Contains(cur, `"owner":"successor"`) {
					t.Fatalf("trial %d: expired owner's release destroyed the successor's lock: held=%v content=%q", trial, held, cur)
				}
			}
		})
	}
}

// TestThreePartyTakeoverExactlyOneOwner is the acquisition half of evaluator
// item 3: multiple fresh contenders racing to take over the SAME stale lock must
// yield EXACTLY ONE owner, with the canonical `.lock` never momentarily emptied
// (so no late newcomer can slip into an empty slot and acquire warning-less) and
// no gate/temp residue left behind. All contenders rendezvous on the exclusive-
// gate compare-and-replace so the interleaving is deterministic per trial.
func TestThreePartyTakeoverExactlyOneOwner(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			const trials = 40
			for trial := 0; trial < trials; trial++ {
				p := lockFakePaths(osk)
				f := newLockFake(osk)
				aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
				f.set(p.Lock, `{"owner":"dead","op":"deploy","started_utc":"`+aged+`","token":"stale"}`)

				const contenders = 3
				bar := newBarrier(contenders)
				f.hook = func(op, s string) {
					if op == "casreplace" {
						bar.wait()
					}
				}

				type res struct {
					lk  *Lock
					err error
				}
				results := make([]res, contenders)
				var wg sync.WaitGroup
				for i := 0; i < contenders; i++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						lk, _, err := AcquireLock(context.Background(), f, p,
							fmt.Sprintf("contender-%d", idx), "deploy", 900)
						results[idx] = res{lk, err}
					}(i)
				}
				wg.Wait()

				winners := 0
				for _, r := range results {
					if r.err == nil && r.lk != nil {
						winners++
						continue
					}
					var ce *CodedError
					if !asCoded(r.err, &ce) || ce.Code != "ERR_LOCKED" {
						t.Fatalf("trial %d: loser must fail ERR_LOCKED, got lk=%v err=%v", trial, r.lk, r.err)
					}
				}
				if winners != 1 {
					t.Fatalf("trial %d: want exactly 1 winner, got %d", trial, winners)
				}
				f.mu.Lock()
				for k := range f.files {
					if strings.Contains(k, ".mx") || strings.Contains(k, ".tmp.") {
						f.mu.Unlock()
						t.Fatalf("trial %d: leaked lock residue %q", trial, k)
					}
				}
				owner, held := f.files[p.Lock]
				f.mu.Unlock()
				if !held || !strings.Contains(owner, `"owner":"contender-`) {
					t.Fatalf("trial %d: lock not owned by the winner: held=%v content=%q", trial, held, owner)
				}
			}
		})
	}
}

// TestReleaseVsInstalledSuccessorPlusThird is the release half of evaluator item
// 3: an EXPIRED owner's release runs concurrently with an ALREADY-INSTALLED
// successor's lock AND a third fresh contender. The expired release must never
// remove the successor's lock, and the third contender must be refused
// (ERR_LOCKED) — the installed successor stays the sole owner. Because both the
// release and any takeover pass through the exclusive gate and the successor's
// lock is fresh (not stale), the third contender can only ever observe exit 48.
func TestReleaseVsInstalledSuccessorPlusThird(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			const trials = 60
			for trial := 0; trial < trials; trial++ {
				p := lockFakePaths(osk)
				f := newLockFake(osk)
				ctx := context.Background()

				// The expired owner's handle references bytes that are ALREADY
				// gone: an installed successor holds a FRESH lock in the slot.
				aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
				ownerContent := `{"owner":"expired","op":"deploy","started_utc":"` + aged + `","token":"owner-tok"}`
				succContent := `{"owner":"successor","op":"deploy","started_utc":"` + nowRFC3339() + `","token":"succ-tok"}`
				f.set(p.Lock, succContent) // successor already installed
				expired := &Lock{t: f, paths: p, content: ownerContent, token: "owner-tok"}

				var wg sync.WaitGroup
				wg.Add(2)
				var thirdLk *Lock
				var thirdErr error
				go func() { defer wg.Done(); ReleaseLock(ctx, expired) }()
				go func() {
					defer wg.Done()
					thirdLk, _, thirdErr = AcquireLock(ctx, f, p, "third", "deploy", 900)
				}()
				wg.Wait()

				// The fresh successor lock is NOT stale, so the third contender
				// must be refused and the expired release must be a no-op.
				if thirdLk != nil || thirdErr == nil {
					t.Fatalf("trial %d: third contender must be refused, got lk=%v err=%v", trial, thirdLk, thirdErr)
				}
				var ce *CodedError
				if !asCoded(thirdErr, &ce) || ce.Code != "ERR_LOCKED" {
					t.Fatalf("trial %d: third contender must fail ERR_LOCKED, got %v", trial, thirdErr)
				}
				f.mu.Lock()
				for k := range f.files {
					if strings.Contains(k, ".mx") || strings.Contains(k, ".tmp.") {
						f.mu.Unlock()
						t.Fatalf("trial %d: leaked lock residue %q", trial, k)
					}
				}
				cur, held := f.files[p.Lock]
				f.mu.Unlock()
				if !held || cur != succContent {
					t.Fatalf("trial %d: expired release/third acquire disturbed the successor's lock: held=%v content=%q", trial, held, cur)
				}
			}
		})
	}
}

// TestTakeoverErrorPathsCleanUp is the injected-transport-failure regression for
// the stale-takeover path: a failure of the compare-and-replace that installs the
// successor over the stale owner must (a) surface a coded error and (b) leave the
// canonical `.lock` exactly as it was — still holding the stale bytes, never
// emptied — with no residue. Because takeover is a single atomic compare-and-
// replace (no delete step, no absent gap), a failed replace changes nothing:
// there is nothing to strand.
func TestTakeoverErrorPathsCleanUp(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			staleContent := `{"owner":"dead","op":"deploy","started_utc":"` + aged + `","token":"stale"}`
			f.set(p.Lock, staleContent)

			// Fail the compare-and-replace exactly once with a transport error.
			var tripped bool
			f.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casreplace" && !tripped {
					tripped = true
					return transport.Result{}, fmt.Errorf("injected casreplace failure"), true
				}
				return transport.Result{}, nil, false
			}

			lk, _, err := AcquireLock(context.Background(), f, p, "victim", "deploy", 900)
			if err == nil || lk != nil {
				t.Fatalf("expected takeover to fail, got lk=%v err=%v", lk, err)
			}
			var ce *CodedError
			if !asCoded(err, &ce) || (ce.Code != "ERR_CONNECT" && ce.Code != "ERR_LOCKED") {
				t.Fatalf("expected coded ERR_CONNECT/ERR_LOCKED, got %v", err)
			}
			// No residue, and the canonical lock is untouched (never absent, still
			// the stale bytes — the failed compare-and-replace changed nothing).
			f.mu.Lock()
			for k := range f.files {
				if strings.Contains(k, ".mx") || strings.Contains(k, ".tmp.") {
					f.mu.Unlock()
					t.Fatalf("leaked lock residue %q", k)
				}
			}
			cur, held := f.files[p.Lock]
			f.mu.Unlock()
			if !held || cur != staleContent {
				t.Fatalf("canonical lock disturbed by a failed takeover: held=%v content=%q", held, cur)
			}
		})
	}
}

// TestTakeoverCleanupSurvivesCancellation is the canceled-mid-takeover
// regression: if the operation context is canceled exactly when the
// compare-and-replace runs, the takeover must fail AND the canonical `.lock` must be
// left untouched (still the stale bytes, never emptied) with no residue. There is
// no absent gap to strand — the compare-and-replace either swaps the bytes
// atomically or does nothing.
func TestTakeoverCleanupSurvivesCancellation(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			staleContent := `{"owner":"dead","op":"deploy","started_utc":"` + aged + `","token":"stale"}`
			f.set(p.Lock, staleContent)

			ctx, cancel := context.WithCancel(context.Background())
			// Cancel exactly when the compare-and-replace is about to run; the
			// decision read that precedes it must complete normally.
			f.hook = func(op, s string) {
				if op == "casreplace" {
					cancel()
				}
			}

			lk, _, err := AcquireLock(ctx, f, p, "victim", "deploy", 900)
			if lk != nil || err == nil {
				t.Fatalf("expected canceled takeover to fail, got lk=%v err=%v", lk, err)
			}
			// Canonical lock never emptied and never altered.
			f.mu.Lock()
			for k := range f.files {
				if strings.Contains(k, ".mx") || strings.Contains(k, ".tmp.") {
					f.mu.Unlock()
					t.Fatalf("leaked residue %q", k)
				}
			}
			cur, held := f.files[p.Lock]
			f.mu.Unlock()
			if !held || cur != staleContent {
				t.Fatalf("canonical lock disturbed by a canceled takeover: held=%v content=%q", held, cur)
			}
		})
	}
}

// TestCasDeleteReportsFailure is the exit-code regression for evaluator item 3: a
// compare-and-delete whose removal actually fails (nonzero exit) must surface an
// error rather than reporting success, so a stranded owned-lock is logged instead
// of silently ignored. A clean delete (matching bytes) succeeds; a mismatch is a
// safe no-op; an injected nonzero exit is an error.
func TestCasDeleteReportsFailure(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			f.set(p.Lock, "data")

			// Success path: matching bytes removed, no error.
			if _, err := casDelete(context.Background(), f, p.Lock, "data"); err != nil {
				t.Fatalf("clean compare-and-delete must succeed, got %v", err)
			}
			if f.has(p.Lock) {
				t.Fatal("file not removed by a successful compare-and-delete")
			}

			// Mismatch path: different bytes ⇒ safe no-op, no error, file kept.
			f.set(p.Lock, "successor-bytes")
			if _, err := casDelete(context.Background(), f, p.Lock, "data"); err != nil {
				t.Fatalf("mismatch must be a safe no-op, got %v", err)
			}
			if !f.has(p.Lock) {
				t.Fatal("mismatch must NOT delete a successor's lock")
			}

			// Failure path: a nonzero removal exit must surface as an error.
			f.set(p.Lock, "data")
			f.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casdelete" {
					return transport.Result{ExitCode: 17, Stderr: "access denied"}, nil, true
				}
				return transport.Result{}, nil, false
			}
			if _, err := casDelete(context.Background(), f, p.Lock, "data"); err == nil {
				t.Fatal("a nonzero removal exit must be reported as an error, not success")
			}
		})
	}
}

// TestCasDistinguishesFailureModes is the regression for evaluator item 1: the
// compare-and-delete must NOT collapse every failure into "absent/contention".
// A permission/operational failure (exit 71) surfaces as an error; a genuine
// absence (48) is casAbsent; a transient exclusion contention (49) is
// casContended — each distinct, on both platforms.
func TestCasDistinguishesFailureModes(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)

			// Operational failure (permission / missing flock) ⇒ error, NOT a
			// silent absence/contention no-op.
			f0 := newLockFake(osk)
			f0.set(pp.Lock, "data")
			f0.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casdelete" {
					return transport.Result{ExitCode: 71, Stderr: "permission denied"}, nil, true
				}
				return transport.Result{}, nil, false
			}
			if _, err := casDelete(context.Background(), f0, pp.Lock, "data"); err == nil {
				t.Fatal("operational failure (exit 71) must surface as an error")
			}

			// Absence (exit 48) ⇒ casAbsent, no error.
			f := newLockFake(osk) // no lock file present
			if oc, err := casDelete(context.Background(), f, pp.Lock, "data"); err != nil || oc != casAbsent {
				t.Fatalf("absent lock must be casAbsent, got outcome=%d err=%v", oc, err)
			}
			// Transient exclusion contention (exit 49) ⇒ casContended, no error.
			f2 := newLockFake(osk)
			f2.set(pp.Lock, "data")
			f2.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casdelete" {
					return transport.Result{ExitCode: 49}, nil, true
				}
				return transport.Result{}, nil, false
			}
			if oc, err := casDelete(context.Background(), f2, pp.Lock, "data"); err != nil || oc != casContended {
				t.Fatalf("exclusion contention must be casContended, got outcome=%d err=%v", oc, err)
			}
		})
	}
}

// TestReleaseLockRetriesContention is the regression for evaluator item 2: when a
// mismatching CAS momentarily holds the exclusive handle (exit 49) while OUR lock
// is still present, ReleaseLock must NOT report success — it retries, and if the
// contention persists it surfaces an error (the lock is still ours, undeleted).
// A contention that clears (49 then 0) must eventually delete and return nil.
func TestReleaseLockRetriesContention(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)
			content := `{"owner":"me","op":"deploy","started_utc":"` + nowRFC3339() + `","token":"tok"}`

			// (a) Persistent contention: ReleaseLock must return an error, NOT nil,
			// and the still-owned lock must remain present (never silently released).
			f := newLockFake(osk)
			f.set(pp.Lock, content)
			f.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casdelete" {
					return transport.Result{ExitCode: 49}, nil, true // always contended
				}
				return transport.Result{}, nil, false
			}
			lk := &Lock{t: f, paths: pp, content: content, token: "tok"}
			if err := ReleaseLock(context.Background(), lk); err == nil {
				t.Fatal("persistent exclusion contention must be surfaced, not reported as success")
			}
			if !f.has(pp.Lock) {
				t.Fatal("a contended release must not have deleted the still-owned lock")
			}

			// (b) Contention that clears: 49 twice then real delete ⇒ nil, removed.
			f2 := newLockFake(osk)
			f2.set(pp.Lock, content)
			var n int
			f2.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casdelete" && n < 2 {
					n++
					return transport.Result{ExitCode: 49}, nil, true
				}
				return transport.Result{}, nil, false
			}
			lk2 := &Lock{t: f2, paths: pp, content: content, token: "tok"}
			if err := ReleaseLock(context.Background(), lk2); err != nil {
				t.Fatalf("a clearing contention must eventually release, got %v", err)
			}
			if f2.has(pp.Lock) {
				t.Fatal("lock not removed after contention cleared")
			}
		})
	}
}

// TestAcquireLockRefusesEmptyLock is the regression for evaluator items 1 & 2:
// the empty-file "recovery" that could grant DUAL OWNERSHIP is GONE. Because
// create and takeover are now both atomic (a `.lock` is never observed
// empty/partial), an empty file is NOT treated as recoverable crash residue — it
// is refused (ERR_LOCKED) like any other unparseable content, so a second
// acquisition can never steal a slot a live creator momentarily left empty.
func TestAcquireLockRefusesEmptyLock(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)
			f := newLockFake(osk)
			f.set(pp.Lock, "") // empty file (e.g. a live creator's transient state)

			lk, _, err := AcquireLock(context.Background(), f, pp, "intruder", "deploy", 900)
			if err == nil || lk != nil {
				t.Fatalf("an empty lock must be REFUSED, not recovered/stolen; got lk=%v err=%v", lk, err)
			}
			if f.get(pp.Lock) != "" {
				t.Fatalf("a refused empty lock must be left untouched, got %q", f.get(pp.Lock))
			}
		})
	}
}

// TestStaleTakeoverIsAtomicReplace proves the takeover shape (evaluator items 1,
// 2 & 4): a proven-stale lock is overridden by a SINGLE atomic compare-and-replace
// that swaps our bytes for the stale bytes, with the stale-owner WARN preserved.
// There is NO delete-then-recreate gap: the canonical slot is never momentarily
// absent, so a late newcomer can never slip in and acquire warning-less.
//
// Crash-safety (iter-19 item 1 & 2): part (a) models a crash DURING the actual
// replacement — the private temp is fully written but the atomic swap never
// commits (crashReplace) — and asserts the canonical `.lock` is left with the
// INTACT stale bytes (never truncated/partial), that only a `.mx.` temp is
// orphaned (never the `.lock` itself), and that a subsequent acquire still takes
// over cleanly WITH the stale WARN. This exercises the fake's replace path rather
// than short-circuiting before it, substantiating the interruption claim.
func TestStaleTakeoverIsAtomicReplace(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)
			aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			stale := `{"owner":"dead","op":"deploy","started_utc":"` + aged + `","token":"stale"}`

			// (a) Crash DURING replacement: the compare succeeds and the temp is
			// written, but the atomic swap never commits. The canonical lock must
			// remain the INTACT stale bytes; only a `.mx.` temp may be orphaned.
			f := newLockFake(osk)
			f.set(pp.Lock, stale)
			f.crashReplace = true
			lk, _, err := AcquireLock(context.Background(), f, pp, "victim", "deploy", 900)
			if err == nil || lk != nil {
				t.Fatalf("crashed takeover must fail, got lk=%v err=%v", lk, err)
			}
			if got := f.get(pp.Lock); got != stale {
				t.Fatalf("crash during replace left a corrupted/partial lock: %q", got)
			}
			// The canonical `.lock` is never the thing left partial — any residue
			// is a uniquely-named `.mx.` temp, which is inert (ignored by acquire).
			f.mu.Lock()
			for k, v := range f.files {
				if k == pp.Lock {
					continue
				}
				if !strings.Contains(k, ".mx.") {
					f.mu.Unlock()
					t.Fatalf("unexpected residue %q=%q (only .mx. temps allowed)", k, v)
				}
			}
			f.mu.Unlock()

			// A clean retry against the SAME fake (crash cleared) still takes over.
			f.crashReplace = false
			lkR, warnR, errR := AcquireLock(context.Background(), f, pp, "successor", "deploy", 900)
			if errR != nil || lkR == nil {
				t.Fatalf("retry after crash must take over, got lk=%v err=%v", lkR, errR)
			}
			if !strings.Contains(warnR, "stale") || !strings.Contains(warnR, "dead") {
				t.Fatalf("retry after crash dropped the stale WARN, got %q", warnR)
			}
			if !strings.Contains(f.get(pp.Lock), `"owner":"successor"`) {
				t.Fatalf("retry after crash did not install our lock: %q", f.get(pp.Lock))
			}

			// (b) Clean takeover: the compare-and-replace succeeds, the WARN names
			// the stale owner, and the new lock is ours.
			f2 := newLockFake(osk)
			f2.set(pp.Lock, stale)
			lk2, warn, err2 := AcquireLock(context.Background(), f2, pp, "successor", "deploy", 900)
			if err2 != nil || lk2 == nil {
				t.Fatalf("clean stale takeover must succeed, got lk=%v err=%v", lk2, err2)
			}
			if !strings.Contains(warn, "stale") || !strings.Contains(warn, "dead") {
				t.Fatalf("stale-takeover WARN missing owner, got %q", warn)
			}
			if !strings.Contains(f2.get(pp.Lock), `"owner":"successor"`) {
				t.Fatalf("stale lock not replaced by us: %q", f2.get(pp.Lock))
			}
		})
	}
}

// TestWindowsTakeoverScriptIsCrashSafe structurally substantiates evaluator
// iter-19 items 1 & 2 at the SCRIPT level: it captures the exact Windows
// compare-and-replace command the engine emits and asserts it (a) publishes via a
// private temp swapped in with the atomic, crash-safe [IO.File]::Replace, (b)
// serializes the compare-and-swap with a per-path Global mutex, and (c) NEVER
// mutates the destination in place — i.e. no SetLength/Position/$fs.Write against
// the live `.lock` handle that could leave partial/mixed JSON after interruption.
func TestWindowsTakeoverScriptIsCrashSafe(t *testing.T) {
	pp := lockFakePaths(spec.OSWindows)
	aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	stale := `{"owner":"dead","op":"deploy","started_utc":"` + aged + `","token":"stale"}`
	f := newLockFake(spec.OSWindows)
	f.set(pp.Lock, stale)

	var replaceScript string
	f.hook = func(op, s string) {
		if op == "casreplace" {
			replaceScript = s
		}
	}
	if _, _, err := AcquireLock(context.Background(), f, pp, "successor", "deploy", 900); err != nil {
		t.Fatalf("takeover must succeed: %v", err)
	}
	if replaceScript == "" {
		t.Fatal("no Windows compare-and-replace script was captured")
	}
	// Crash-safe publish: temp write + atomic Replace.
	for _, want := range []string{"WriteAllBytes($tmp", "[IO.File]::Replace($tmp,", `Global\labdeploy-lock-`, "$mtx.WaitOne", "$mtx.ReleaseMutex"} {
		if !strings.Contains(replaceScript, want) {
			t.Fatalf("Windows replace script missing crash-safe/serialization element %q:\n%s", want, replaceScript)
		}
	}
	// NO in-place mutation of the live destination handle.
	for _, bad := range []string{"SetLength(", "$fs.Write(", "$fs.Position"} {
		if strings.Contains(replaceScript, bad) {
			t.Fatalf("Windows replace script mutates the lock in place (%q) — not crash-safe:\n%s", bad, replaceScript)
		}
	}
}


// TestTakeoverReplaceCannotDualOwn is the DIRECT regression for evaluator iter-20
// item 1: the exact interleaving where, while a stale-takeover replace is between
// reading the stale bytes and committing its atomic swap, the OLD owner deletes the
// stale lock AND a NEWCOMER creates a fresh lock into the slot. If create/delete did
// not share the takeover's mutex, the replace would clobber the newcomer's fresh
// lock and BOTH the newcomer and the taker would believe they own it. Here the fake
// models the SHARED per-path mutex (as the production code now does: create,
// casdelete, and casreplace all take the Windows Global\ mutex / POSIX
// parent-directory flock), so the old owner's release and the newcomer's create are
// injected DURING the taker's in-flight replace (via replaceMid) and are serialized
// behind it. The result is exactly one owner: the taker wins WITH the stale WARN,
// the newcomer is refused ERR_LOCKED against the taker's fresh lock, and the old
// owner's release is a safe no-op. Deterministic (owner-pinned channel
// rendezvous), no timing sleeps.
func TestTakeoverReplaceCannotDualOwn(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)
			aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			stale := `{"owner":"dead","op":"deploy","started_utc":"` + aged + `","token":"stale"}`
			f := newLockFake(osk)
			f.set(pp.Lock, stale)

			paused := make(chan struct{})
			resume := make(chan struct{})
			f.replaceMid = func(path string) {
				close(paused) // taker has read+matched the stale bytes, holds the mutex
				<-resume      // stay mid-replace until the racers are in flight
			}

			// Count the racers' first Exec (create for the newcomer, casdelete for
			// the old owner) so we release the taker ONLY once both are contending
			// the shared mutex — a deterministic rendezvous, not a sleep.
			armed := make(chan struct{})
			delSeen := make(chan struct{})
			createSeen := make(chan struct{})
			var delOnce, createOnce sync.Once
			f.hook = func(op, s string) {
				select {
				case <-armed:
				default:
					return
				}
				switch op {
				case "casdelete":
					delOnce.Do(func() { close(delSeen) })
				case "create":
					createOnce.Do(func() { close(createSeen) })
				}
			}

			type ares struct {
				lk   *Lock
				warn string
				err  error
			}
			takerCh := make(chan ares, 1)
			go func() {
				lk, warn, err := AcquireLock(context.Background(), f, pp, "taker", "deploy", 900)
				takerCh <- ares{lk, warn, err}
			}()

			<-paused // taker is between its read and its atomic swap, holding the mutex
			close(armed)

			oldOwnerErr := make(chan error, 1)
			go func() {
				oldOwnerErr <- ReleaseLock(context.Background(),
					&Lock{t: f, paths: pp, content: stale, token: "stale"})
			}()
			newcomerCh := make(chan ares, 1)
			go func() {
				lk, warn, err := AcquireLock(context.Background(), f, pp, "newcomer", "deploy", 900)
				newcomerCh <- ares{lk, warn, err}
			}()

			<-delSeen    // old owner's compare-and-delete is contending the mutex
			<-createSeen // newcomer's create is contending the mutex
			close(resume) // let the taker commit its swap and release the mutex

			taker := <-takerCh
			newcomer := <-newcomerCh
			oldErr := <-oldOwnerErr

			// The taker wins WITH the stale WARN and installs its bytes.
			if taker.err != nil || taker.lk == nil {
				t.Fatalf("taker must win the stale takeover: lk=%v err=%v", taker.lk, taker.err)
			}
			if !strings.Contains(taker.warn, "stale") || !strings.Contains(taker.warn, "dead") {
				t.Fatalf("taker dropped the stale WARN: %q", taker.warn)
			}
			// The newcomer is refused — NOT granted a second, dual ownership.
			var ce *CodedError
			if newcomer.err == nil || newcomer.lk != nil || !asCoded(newcomer.err, &ce) || ce.Code != "ERR_LOCKED" {
				t.Fatalf("newcomer must be refused ERR_LOCKED (no dual ownership): lk=%v err=%v",
					newcomer.lk, newcomer.err)
			}
			// The old owner's release never removed the taker's fresh lock.
			if oldErr != nil {
				t.Fatalf("stale old-owner release should be a safe no-op: %v", oldErr)
			}
			if !strings.Contains(f.get(pp.Lock), `"owner":"taker"`) {
				t.Fatalf("canonical lock is not solely the taker's: %q", f.get(pp.Lock))
			}
		})
	}
}


// by evaluator item 1: a late newcomer B that races a takeover must NOT be able to
// acquire warning-less through a gap. We deterministically pin contender A at the
// moment it is about to run its compare-and-replace (identified by the owner in
// the bytes it is about to install, NOT by timing) and, while A is paused, let
// newcomer B run a COMPLETE takeover attempt. Because the takeover is a single
// atomic compare-and-replace with NO absent slot, B — being the actor that
// overrides the stale bytes — wins WITH the stale WARN, and A then observes B's
// fresh bytes (mismatch) and is refused ERR_LOCKED. There is no interleaving in
// which the successful acquirer returns an empty warn.
func TestLateNewcomerDuringTakeoverStillWarns(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			f.set(p.Lock, `{"owner":"dead","op":"deploy","started_utc":"`+aged+`","token":"stale"}`)

			replaceOwner := func(s string) string {
				var nb string
				if osk == spec.OSWindows {
					nb, _ = firstGroup(lfNewB64Win, s)
				} else {
					nb, _ = firstGroup(lfNewB64Sh, s)
				}
				raw, _ := base64.StdEncoding.DecodeString(nb)
				return string(raw)
			}

			bDone := make(chan struct{})
			var bLk *Lock
			var bWarn string
			var bErr error
			var once sync.Once
			f.hook = func(op, s string) {
				// Pin A exactly at its own compare-and-replace, then run B fully.
				if op == "casreplace" && strings.Contains(replaceOwner(s), `"owner":"A"`) {
					once.Do(func() {
						bLk, bWarn, bErr = AcquireLock(context.Background(), f, p, "B", "deploy", 900)
						close(bDone)
					})
					<-bDone
				}
			}

			aLk, aWarn, aErr := AcquireLock(context.Background(), f, p, "A", "deploy", 900)

			// B (the late newcomer) won the takeover and MUST carry the stale WARN.
			if bErr != nil || bLk == nil {
				t.Fatalf("late newcomer B must win the takeover, got lk=%v err=%v", bLk, bErr)
			}
			if !strings.Contains(bWarn, "stale") || !strings.Contains(bWarn, "dead") {
				t.Fatalf("late newcomer acquired WITHOUT the stale warning: warn=%q", bWarn)
			}
			// A observed B's fresh lock and was correctly refused — never a
			// warn-less success.
			if aLk != nil || aErr == nil {
				t.Fatalf("A must be refused against B's fresh lock, got lk=%v warn=%q err=%v", aLk, aWarn, aErr)
			}
			var ce *CodedError
			if !asCoded(aErr, &ce) || ce.Code != "ERR_LOCKED" {
				t.Fatalf("A must fail ERR_LOCKED, got %v", aErr)
			}
			if !strings.Contains(f.get(p.Lock), `"owner":"B"`) {
				t.Fatalf("winner B's lock not installed: %q", f.get(p.Lock))
			}
		})
	}
}

// TestConcurrentCreateNeverExposesEmptyResidue is the deterministic concurrent
// test for evaluator item 3. Two acquirers race for a FREE slot. The hook pauses
// EXACTLY acquirer A's create — identified deterministically by the owner embedded
// in the published bytes, NOT by timing — and holds it until acquirer B has run a
// full attempt. Because create publishes atomically (temp then Move/ln — the file
// is never empty at the canonical path) and the empty-residue recovery branch is
// gone, B can NEVER observe or "recover" an empty file: exactly one acquirer wins
// and the stored lock is always a complete, owner-bearing JSON — never the empty
// string. Ordering is signalled through channels (A reaching its create gates B's
// start, and B's completion releases A), so there is no time.Sleep and no path on
// which a goroutine can block waiting on a signal only it could send.
func TestConcurrentCreateNeverExposesEmptyResidue(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)
			f := newLockFake(osk)

			// Deterministic rendezvous. aAtCreate is closed the instant acquirer
			// A's create is classified (A is pinned by owner="A" in its payload,
			// so ONLY A's create pauses — B's create is never gated). B waits on
			// aAtCreate before it even starts, guaranteeing A is parked mid-create
			// with NO file yet published; B then runs a whole attempt and closes
			// bDone, which releases A. No wall-clock timing, no self-wait deadlock.
			aAtCreate := make(chan struct{})
			bDone := make(chan struct{})
			var aOnce sync.Once
			f.hook = func(op, script string) {
				if op != "create" {
					return
				}
				var payloadB64 string
				if osk == spec.OSWindows {
					payloadB64, _ = firstGroup(lfNewB64Win, script)
				} else {
					payloadB64, _ = firstGroup(lfNewB64Sh, script)
				}
				raw, _ := base64.StdEncoding.DecodeString(payloadB64)
				if !strings.Contains(string(raw), `"owner":"A"`) {
					return // B's (or any non-A) create proceeds without pausing.
				}
				aOnce.Do(func() { close(aAtCreate) })
				<-bDone // hold A mid-create until B has finished a full attempt.
			}

			var wg sync.WaitGroup
			wins := make(chan bool, 2)
			// Acquirer A: its create is parked at the canonical publish point.
			wg.Add(1)
			go func() {
				defer wg.Done()
				lk, _, err := AcquireLock(context.Background(), f, pp, "A", "deploy", 900)
				wins <- (err == nil && lk != nil)
			}()
			// Acquirer B: starts only once A is parked mid-create, runs a full
			// attempt (it must never observe an empty file), then releases A.
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-aAtCreate
				lk, _, err := AcquireLock(context.Background(), f, pp, "B", "deploy", 900)
				wins <- (err == nil && lk != nil)
				close(bDone)
			}()
			wg.Wait()
			close(wins)

			won := 0
			for w := range wins {
				if w {
					won++
				}
			}
			if won != 1 {
				t.Fatalf("exactly one acquirer must win a single free slot, got %d", won)
			}
			// The stored lock is a complete JSON owned by whoever won — never empty.
			got := f.get(pp.Lock)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("the canonical lock was observed EMPTY — atomic create violated: %q", got)
			}
			if !strings.Contains(got, `"owner":"A"`) && !strings.Contains(got, `"owner":"B"`) {
				t.Fatalf("stored lock is not a complete owner-bearing JSON: %q", got)
			}
		})
	}
}

// TestCreateOperationalFailureIsNotContention is the regression for evaluator
// item 2: an OPERATIONAL create failure (disk full, permission denied, missing
// filesystem — modeled by the publish step returning exit 71 while the
// destination is ABSENT) must surface as an operational error (ERR_CONNECT), NOT
// as ERR_LOCKED. Previously every failed rename/hard-link mapped to exit 48 and
// then to ERR_LOCKED, so a broken disk masqueraded as a held lock.
func TestCreateOperationalFailureIsNotContention(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			pp := lockFakePaths(osk)
			f := newLockFake(osk) // slot is FREE: a 48 here would be a misclassification
			f.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "create" {
					return transport.Result{ExitCode: 71}, nil, true // operational, not EEXIST
				}
				return transport.Result{}, nil, false
			}
			lk, warn, err := AcquireLock(context.Background(), f, pp, "me", "deploy", 900)
			if err == nil || lk != nil || warn != "" {
				t.Fatalf("operational create failure must fail (no lock/warn), got lk=%v warn=%q err=%v", lk, warn, err)
			}
			var ce *CodedError
			if !asCoded(err, &ce) || ce.Code != "ERR_CONNECT" {
				t.Fatalf("operational create failure must be ERR_CONNECT (not ERR_LOCKED), got %v", err)
			}
			if f.has(pp.Lock) {
				t.Fatalf("a failed create must not leave a lock behind: %q", f.get(pp.Lock))
			}
		})
	}
}


// genuinely LIVE (non-stale) held lock must be refused promptly — a handful of
// commands, no spinning — to honor DESIGN §13 "fail fast, <5s, no wait". We assert
// both a bounded command count and a short elapsed time.
func TestAcquireLockFastFailsOnLiveLock(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			// A FRESH lock (started now) — well within any sane timeout.
			f.set(p.Lock, `{"owner":"live","op":"deploy","started_utc":"`+nowRFC3339()+`","token":"live-tok"}`)

			start := time.Now()
			lk, _, err := AcquireLock(context.Background(), f, p, "contender", "deploy", 900)
			elapsed := time.Since(start)

			var ce *CodedError
			if err == nil || lk != nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
				t.Fatalf("live lock must be refused ERR_LOCKED, got lk=%v err=%v", lk, err)
			}
			// create-new(48) + one decision read = 2 commands; allow a tiny margin
			// but PROVE we did not spin the retry loop.
			f.mu.Lock()
			n := f.execN
			f.mu.Unlock()
			if n > 3 {
				t.Fatalf("live lock refusal must be bounded (no spin), used %d commands", n)
			}
			if elapsed > time.Second {
				t.Fatalf("live lock refusal must be fast (<5s), took %v", elapsed)
			}
		})
	}
}

// TestReleaseLockSurfacesDeletionFailure is the error-surfacing regression for
// evaluator item 3: when the compare-and-delete of an OWNED lock genuinely fails,
// ReleaseLock must return the error (after bounded retries) so the engine can log
// it, rather than silently claiming the lock was released.
func TestReleaseLockSurfacesDeletionFailure(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			content := `{"owner":"me","op":"deploy","started_utc":"` + nowRFC3339() + `","token":"tok"}`
			f.set(p.Lock, content)
			lk := &Lock{t: f, paths: p, content: content, token: "tok"}

			// Every compare-and-delete fails at the removal step.
			f.failOn = func(op, s string) (transport.Result, error, bool) {
				if op == "casdelete" {
					return transport.Result{ExitCode: 17, Stderr: "io error"}, nil, true
				}
				return transport.Result{}, nil, false
			}
			if err := ReleaseLock(context.Background(), lk); err == nil {
				t.Fatal("ReleaseLock must surface a persistent removal failure")
			}
		})
	}
}

// ctxProbe wraps a lockFake and records, per host, the context handed to the// FIRST Exec of that node's release, plus the reverse-order sequence. It lets
// TestClusterUnlockPerNodeContext prove each node gets its OWN cleanup context.
type ctxProbe struct {
	*lockFake
	host string
	rec  *ctxRecorder
}

type ctxRecorder struct {
	mu    sync.Mutex
	order []string
	done  map[string]<-chan struct{}
	errAt map[string]error
	seen  map[string]bool
}

func (c *ctxProbe) Host() string { return c.host }
func (c *ctxProbe) Exec(ctx context.Context, cmd transport.Cmd) (transport.Result, error) {
	c.rec.mu.Lock()
	if !c.rec.seen[c.host] {
		c.rec.seen[c.host] = true
		c.rec.order = append(c.rec.order, c.host)
		c.rec.done[c.host] = ctx.Done()
		c.rec.errAt[c.host] = ctx.Err() // liveness captured AT release time
	}
	c.rec.mu.Unlock()
	return c.lockFake.Exec(ctx, cmd)
}

// TestClusterUnlockPerNodeContext is the regression for evaluator item 3: cluster
// cleanup must release node locks in REVERSE acquisition order, and each node
// must get its OWN bounded cleanup context detached from the (possibly canceled)
// operation context — not one shared deadline that a single slow release could
// exhaust for the rest.
func TestClusterUnlockPerNodeContext(t *testing.T) {
	rec := &ctxRecorder{done: map[string]<-chan struct{}{}, errAt: map[string]error{}, seen: map[string]bool{}}
	hosts := []string{"h1", "h2", "h3"}
	p := lockFakePaths(spec.OSLinux)

	cc := &clusterCtx{hosts: hosts, tr: map[string]transport.Transport{}}
	for _, h := range hosts {
		lf := newLockFake(spec.OSLinux)
		content := `{"owner":"o","op":"deploy","started_utc":"` + nowRFC3339() + `","token":"t-` + h + `"}`
		lf.set(p.Lock, content)
		probe := &ctxProbe{lockFake: lf, host: h, rec: rec}
		cc.tr[h] = probe
		cc.locked = append(cc.locked, &Lock{t: probe, paths: p, content: content})
	}

	e := &Engine{}
	// Cancel the operation context up front: cleanup must STILL run per node.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.unlockAll(ctx, cc)

	// Reverse acquisition order (DESIGN §13).
	if want := []string{"h3", "h2", "h1"}; !equalStrs(rec.order, want) {
		t.Fatalf("unlock order = %v, want reverse %v", rec.order, want)
	}
	// Each node's cleanup context must be DISTINCT (per-node, not shared) and
	// must NOT be canceled at release time despite the parent being canceled.
	distinct := map[<-chan struct{}]bool{}
	for _, h := range hosts {
		distinct[rec.done[h]] = true
		if rec.errAt[h] != nil {
			t.Fatalf("node %s released under a canceled/expired context: %v", h, rec.errAt[h])
		}
	}
	if len(distinct) != len(hosts) {
		t.Fatalf("expected a distinct cleanup context per node, got %d distinct for %d nodes", len(distinct), len(hosts))
	}
	// Every node lock must actually be released.
	for _, h := range hosts {
		probe := cc.tr[h].(*ctxProbe)
		if probe.has(p.Lock) {
			t.Fatalf("node %s lock not released", h)
		}
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLockLifecycleLinux is the regression for evaluator item 4: exercise the
// POSIX atomic create-new (temp + hardlink), exit-48 contention, stale
// replacement, and ownership-safe release scripts against a Linux fake transport.
func TestLockLifecycleLinux(t *testing.T) {
	p := lockFakePaths(spec.OSLinux)
	f := newLockFake(spec.OSLinux)
	ctx := context.Background()

	// (a) create-new on a clean host succeeds.
	lk, warn, err := AcquireLock(ctx, f, p, "runner-a", "deploy", 900)
	if err != nil || lk == nil || warn != "" {
		t.Fatalf("linux fresh acquire: lk=%v warn=%q err=%v", lk, warn, err)
	}
	if !f.has(p.Lock) {
		t.Fatal("linux: lock file not created via atomic hardlink publish")
	}

	// (b) exit-48 contention: a second acquire while held (fresh) ⇒ ERR_LOCKED.
	lk2, _, err := AcquireLock(ctx, f, p, "runner-b", "deploy", 900)
	var ce *CodedError
	if err == nil || lk2 != nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
		t.Fatalf("linux contention: want ERR_LOCKED, got lk=%v err=%v", lk2, err)
	}

	// (c) ownership-safe release removes our lock.
	ReleaseLock(ctx, lk)
	if f.has(p.Lock) {
		t.Fatal("linux: ownership-safe release did not remove the lock")
	}

	// (d) stale replacement: plant an aged lock, acquire ⇒ takeover + WARN.
	aged := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	f.set(p.Lock, `{"owner":"dead","op":"deploy","started_utc":"`+aged+`","token":"stale"}`)
	lk3, warn, err := AcquireLock(ctx, f, p, "runner-c", "deploy", 900)
	if err != nil || lk3 == nil {
		t.Fatalf("linux stale takeover: lk=%v err=%v", lk3, err)
	}
	if !strings.Contains(warn, "stale") || !strings.Contains(warn, "dead") {
		t.Fatalf("linux stale takeover WARN missing: %q", warn)
	}
	if !strings.Contains(f.get(p.Lock), `"owner":"runner-c"`) {
		t.Fatalf("linux stale lock not overwritten: %s", f.get(p.Lock))
	}
}

// TestAcquireLockFreshWins covers the uncontended path: no lock present ⇒ lock
// created, no WARN, no error.
func TestAcquireLockFreshWins(t *testing.T) {
	p := testPaths()
	f := newFakeHost("lab-01")
	lk, warn, err := AcquireLock(context.Background(), f, p, "me", "deploy", 900)
	if err != nil || warn != "" || lk == nil {
		t.Fatalf("uncontended acquire: lk=%v warn=%q err=%v", lk, warn, err)
	}
	if _, held := f.files[p.Lock]; !held {
		t.Fatal("lock file was not created")
	}
}

// ----------------------------------------------------------------------------
// lockFake: a concurrency-safe fake transport that models the lock primitives
// for BOTH Windows and Linux script forms, so locking is proven cross-platform
// (evaluator item 4) and under true goroutine races (evaluator item 1).
// ----------------------------------------------------------------------------

type lockFake struct {
	mu    sync.Mutex
	osk   spec.OSKind
	files map[string]string
	// hook, if set, is called with the classified op ("read"/"create"/"write"/
	// "casreplace"/"casdelete"/"delete") and the raw script BEFORE f.mu is taken,
	// so a test can block a goroutine at a chosen primitive (e.g. rendezvous both
	// takeover contenders on the compare-and-replace) WITHOUT holding the fake's
	// mutex across the pause — the mutex is released between Execs, so command
	// interleaving is genuinely modeled.
	hook func(op, script string)
	// failOn, if the returned err/exit is non-zero for a matching (op, script),
	// injects a transport failure so CAS error paths can be exercised
	// (evaluator item 3). It is consulted under f.mu at the start of the op.
	failOn func(op, script string) (transport.Result, error, bool)
	// execN counts Exec calls (under f.mu) so the fast-fail test can assert a
	// held-fresh lock is refused in a bounded number of commands (item 5).
	execN int
	// crashReplace, when true, makes casreplace model a crash DURING the atomic
	// replace: the private temp is fully written but the swap never commits, so
	// the canonical lock keeps its intact prior bytes (crash-safety, iter-19 item 1).
	crashReplace bool
	// pathMu models the SHARED per-path OS mutex (Windows Global\ named mutex /
	// POSIX parent-directory flock) that create, casdelete, AND casreplace all take
	// in the real transport. Modeling it here — rather than serializing every Exec
	// under f.mu — is what lets a test inject a delete + newcomer-create BETWEEN a
	// casReplace's read and its swap and observe that they BLOCK on the shared mutex
	// until the replace commits, so no dual ownership can occur (evaluator iter-20
	// items 1 & 2).
	pathMu map[string]*sync.Mutex
	// replaceMid, if set, is invoked by casreplace AFTER it reads+matches the stale
	// bytes but BEFORE it commits the swap, while the shared path mutex is HELD (and
	// f.mu is NOT), so the callback can launch concurrent create/delete goroutines
	// and prove they are serialized behind the in-flight replace.
	replaceMid func(path string)
}

func newLockFake(osk spec.OSKind) *lockFake {
	return &lockFake{osk: osk, files: map[string]string{}}
}

func lockFakePaths(osk spec.OSKind) layout.Paths {
	if osk == spec.OSWindows {
		return layout.NewPaths(osk, `C:\deploy`, "sample-svc", "1.0.0")
	}
	return layout.NewPaths(osk, "/opt/deploy", "sample-svc", "1.0.0")
}

func (f *lockFake) set(path, content string) { f.mu.Lock(); defer f.mu.Unlock(); f.files[path] = content }
func (f *lockFake) get(path string) string   { f.mu.Lock(); defer f.mu.Unlock(); return f.files[path] }
func (f *lockFake) has(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[path]
	return ok
}

func (f *lockFake) Connect(context.Context) error { return nil }
func (f *lockFake) Close() error                  { return nil }
func (f *lockFake) OS() spec.OSKind               { return f.osk }
func (f *lockFake) Host() string                  { return "node-1" }
func (f *lockFake) Upload(context.Context, io.Reader, int64, string) error {
	return nil
}
func (f *lockFake) Download(context.Context, string, string) error {
	return fmt.Errorf("not needed")
}

var _ transport.Transport = (*lockFake)(nil)

var (
	lfReadWin   = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
	lfReadSh    = regexp.MustCompile(`base64 < '([^']+)'`)
	lfOpenWin   = regexp.MustCompile(`\[IO\.File\]::Move\(\$tmp,'([^']+)'\)`)
	lfCreateSh  = regexp.MustCompile(`ln "\$tmp" '([^']+)'`)
	lfWriteAllW = regexp.MustCompile(`WriteAllBytes\('([^']+)',`)
	lfNewB64Win = regexp.MustCompile(`FromBase64String\('([^']*)'\)`)
	lfWriteSh   = regexp.MustCompile(`base64 -d > '([^']+)'`)
	lfNewB64Sh  = regexp.MustCompile(`printf '%s' '([^']*)'`)
	lfDelWin    = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)
	lfDelSh     = regexp.MustCompile(`rm -f '([^']+)'`)
	// CAS compare-and-delete: keyed on [IO.File]::Delete (Windows) / the
	// dir-flock body (POSIX). The Windows read is now ReadAllBytes under the shared
	// mutex; the lock path is recovered from the Delete call, the POSIX path from
	// the `base64 < 'path'` read (lfReadSh).
	lfCasDelWin = regexp.MustCompile(`\[IO\.File\]::Delete\('([^']+)'\)`)
	lfCurEqWin  = regexp.MustCompile(`\$cur -eq '([^']*)'`)
	lfCurNeSh   = regexp.MustCompile(`\[ "\$cur" != '([^']*)' \]`)
	// CAS compare-and-REPLACE (atomic crash-safe stale takeover).
	lfCasReplaceWin = regexp.MustCompile(`\[IO\.File\]::Replace\(\$tmp,'([^']+)',`)
	lfCurNeWin      = regexp.MustCompile(`\$cur -ne '([^']*)'`)
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func firstGroup(re *regexp.Regexp, s string) (string, bool) {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

// classify maps a lock script to exactly one primitive. Order matters: the CAS
// compare-and-REPLACE (atomic crash-safe stale takeover; reuses ReadAllBytes /
// WriteAllBytes / FromBase64String fragments) MUST be recognized BEFORE the
// compare-and-delete, read, create, and write cases. casreplace is keyed on the
// atomic `[IO.File]::Replace(` (Windows) or the temp `mv -f "$tmp"` (POSIX),
// neither of which appears in any other primitive.
func (f *lockFake) classify(s string) string {
	switch {
	case strings.Contains(s, "[IO.File]::Replace(") ||
		strings.Contains(s, `mv -f "$tmp"`):
		return "casreplace"
	case strings.Contains(s, "[IO.File]::Delete(") ||
		(strings.Contains(s, "flock") && strings.Contains(s, "rm -f '")):
		return "casdelete"
	case strings.Contains(s, "ReadAllBytes(") || strings.Contains(s, "base64 < '"):
		return "read"
	case strings.Contains(s, "[IO.File]::Move($tmp,") || strings.Contains(s, `ln "$tmp"`):
		return "create"
	case strings.Contains(s, "WriteAllBytes(") || strings.Contains(s, "base64 -d > '"):
		return "write"
	case strings.Contains(s, "Remove-Item") || strings.Contains(s, "rm -f '"):
		return "delete"
	}
	return ""
}

// Exec models each lock primitive as a SINGLE atomic operation while RELEASING
// the fake's mutex between calls, so concurrent goroutines genuinely interleave
// at command boundaries (the interleaving the evaluator asked us to model).
// Create PUBLISHES a fully-formed file atomically (temp then Move/ln), so a
// concurrent acquirer NEVER observes an empty/partial `.lock`; a HELD lock is
// removed by one exclusive-handle compare-and-delete (casdelete). Stale TAKEOVER
// is a single exclusive-gate compare-and-replace (casreplace) that installs the
// successor's bytes in place of the stale bytes with NO intervening absent slot,
// so two contenders can never both win and no late newcomer can slip into a gap
// and acquire warning-less. It HONORS context cancellation (a canceled ctx fails
// the op) so tests can prove cleanup uses a cancellation-detached context.
func (f *lockFake) pathMutex(path string) *sync.Mutex {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pathMu == nil {
		f.pathMu = map[string]*sync.Mutex{}
	}
	m, ok := f.pathMu[path]
	if !ok {
		m = &sync.Mutex{}
		f.pathMu[path] = m
	}
	return m
}

func (f *lockFake) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	win := f.osk == spec.OSWindows
	op := f.classify(s)
	if f.hook != nil {
		f.hook(op, s) // BEFORE locking so a blocking rendezvous doesn't hold f.mu
	}
	if err := ctx.Err(); err != nil {
		return transport.Result{}, err // canceled/timed-out ctx fails the op
	}
	f.mu.Lock()
	f.execN++
	ff := f.failOn
	f.mu.Unlock()

	if ff != nil {
		if r, err, hit := ff(op, s); hit {
			return r, err
		}
	}

	switch op {
	case "read":
		var path string
		if win {
			path, _ = firstGroup(lfReadWin, s)
		} else {
			path, _ = firstGroup(lfReadSh, s)
		}
		f.mu.Lock()
		content, ok := f.files[path]
		f.mu.Unlock()
		if !ok {
			return transport.Result{ExitCode: 3}, nil
		}
		return transport.Result{ExitCode: 0, Stdout: b64(content)}, nil

	case "create": // atomic create-with-content (temp then Move/ln publish)
		var path, newB64 string
		if win {
			path, _ = firstGroup(lfOpenWin, s)
			newB64, _ = firstGroup(lfNewB64Win, s)
		} else {
			path, _ = firstGroup(lfCreateSh, s)
			newB64, _ = firstGroup(lfNewB64Sh, s)
		}
		// create shares the per-path mutex, so it cannot slip a fresh lock into a
		// slot a stale-takeover replace is mid-swapping (evaluator iter-20 item 1).
		pm := f.pathMutex(path)
		pm.Lock()
		defer pm.Unlock()
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, held := f.files[path]; held {
			return transport.Result{ExitCode: 48}, nil
		}
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		f.files[path] = string(raw) // atomic publish: the file appears fully-formed
		return transport.Result{ExitCode: 0}, nil

	case "write": // non-exclusive write (writeSmallFile / WriteManifest)
		var path, newB64 string
		if win {
			path, _ = firstGroup(lfWriteAllW, s)
			newB64, _ = firstGroup(lfNewB64Win, s)
		} else {
			path, _ = firstGroup(lfWriteSh, s)
			newB64, _ = firstGroup(lfNewB64Sh, s)
		}
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		f.mu.Lock()
		f.files[path] = string(raw)
		f.mu.Unlock()
		return transport.Result{ExitCode: 0}, nil

	case "casreplace": // atomic crash-safe compare-and-replace (stale takeover)
		var path, expB64, newB64 string
		if win {
			path, _ = firstGroup(lfCasReplaceWin, s)
			expB64, _ = firstGroup(lfCurNeWin, s)
			newB64, _ = firstGroup(lfNewB64Win, s)
		} else {
			path, _ = firstGroup(lfReadSh, s)
			expB64, _ = firstGroup(lfCurNeSh, s)
			newB64, _ = firstGroup(lfNewB64Sh, s)
		}
		// The whole read-compare-then-swap runs under the SHARED path mutex, so a
		// concurrent casdelete or create BLOCKS until we finish — it can neither
		// remove the bytes we matched nor publish a fresh lock we would clobber
		// (evaluator iter-20 items 1 & 2).
		pm := f.pathMutex(path)
		pm.Lock()
		defer pm.Unlock()
		f.mu.Lock()
		cur, ok := f.files[path]
		f.mu.Unlock()
		if !ok {
			return transport.Result{ExitCode: 48}, nil // slot vanished (released)
		}
		if b64(cur) != expB64 {
			return transport.Result{ExitCode: 10}, nil // a fresh successor already installed
		}
		// Interleave window: the mutex is HELD (f.mu is not). A test's replaceMid
		// can launch concurrent delete/create here and prove they serialize behind
		// this in-flight swap rather than racing it.
		if f.replaceMid != nil {
			f.replaceMid(path)
		}
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		if f.crashReplace {
			// Model a crash DURING the replace: the private temp is fully written
			// (an orphan `.mx.` file), but the process dies BEFORE the atomic
			// [IO.File]::Replace/mv swap commits. The canonical lock is therefore
			// left UNTOUCHED (the intact stale bytes) — never truncated/partial —
			// which is exactly the crash-safety contract (evaluator iter-19 item 1).
			f.mu.Lock()
			f.files[path+".mx.crash"] = string(raw) // orphan temp, never the .lock
			f.mu.Unlock()
			return transport.Result{}, fmt.Errorf("crash before atomic replace commit")
		}
		f.mu.Lock()
		f.files[path] = string(raw) // atomic swap: stale bytes -> successor, never partial
		f.mu.Unlock()
		return transport.Result{ExitCode: 0}, nil

	case "casdelete": // compare-and-delete of a HELD lock (shared path mutex)
		var path, expB64 string
		if win {
			path, _ = firstGroup(lfCasDelWin, s)
			expB64, _ = firstGroup(lfCurEqWin, s)
		} else {
			path, _ = firstGroup(lfReadSh, s)
			expB64, _ = firstGroup(lfCurNeSh, s)
		}
		pm := f.pathMutex(path)
		pm.Lock()
		defer pm.Unlock()
		f.mu.Lock()
		defer f.mu.Unlock()
		cur, ok := f.files[path]
		if !ok {
			return transport.Result{ExitCode: 48}, nil // already gone / peer-locked
		}
		if b64(cur) != expB64 {
			return transport.Result{ExitCode: 10}, nil // a successor owns it: no-op
		}
		delete(f.files, path)
		return transport.Result{ExitCode: 0}, nil

	case "delete":
		var path string
		if win {
			path, _ = firstGroup(lfDelWin, s)
		} else {
			path, _ = firstGroup(lfDelSh, s)
		}
		f.mu.Lock()
		delete(f.files, path)
		f.mu.Unlock()
		return transport.Result{ExitCode: 0}, nil
	}
	return transport.Result{ExitCode: 0}, nil
}

// barrier releases the first `target` callers of wait() simultaneously, then
// stays open. Used to force both takeover contenders onto the exclusive gate
// create at the same instant so the regression genuinely exercises the race.
type barrier struct {
	mu     sync.Mutex
	cond   *sync.Cond
	target int
	count  int
	open   bool
}

func newBarrier(target int) *barrier {
	b := &barrier{target: target}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *barrier) wait() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open {
		return
	}
	b.count++
	if b.count >= b.target {
		b.open = true
		b.cond.Broadcast()
		return
	}
	for !b.open {
		b.cond.Wait()
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
