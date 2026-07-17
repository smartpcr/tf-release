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
// winner (WARN + handle); the loser gets ERR_LOCKED. The atomic create-new gate
// is the arbiter — no double-ownership.
func TestAcquireLockConcurrentTakeover(t *testing.T) {
	for _, osk := range []spec.OSKind{spec.OSWindows, spec.OSLinux} {
		osk := osk
		t.Run(string(osk), func(t *testing.T) {
			p := lockFakePaths(osk)
			f := newLockFake(osk)
			aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			f.set(p.Lock, `{"owner":"dead","op":"deploy","started_utc":"`+aged+`","token":"stale"}`)

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
				case r.err == nil && r.lk != nil && strings.Contains(r.warn, "stale"):
					winners++
				case r.err != nil && r.lk == nil:
					var ce *CodedError
					if !asCoded(r.err, &ce) || ce.Code != "ERR_LOCKED" {
						t.Fatalf("loser must fail ERR_LOCKED, got %v", r.err)
					}
					losers++
				default:
					t.Fatalf("ambiguous outcome: lk=%v warn=%q err=%v", r.lk, r.warn, r.err)
				}
			}
			if winners != 1 || losers != 1 {
				t.Fatalf("want exactly 1 winner + 1 loser, got winners=%d losers=%d", winners, losers)
			}
		})
	}
}

// TestLockLifecycleLinux is the regression for evaluator item 4: exercise the
// POSIX `set -C` create-new, exit-48 contention, stale replacement, and
// ownership-safe release scripts against a Linux fake transport.
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
		t.Fatal("linux: lock file not created via set -C")
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
	lfWinOpen   = regexp.MustCompile(`\[IO\.File\]::Open\('([^']+)','CreateNew'\)`)
	lfWinNewB64 = regexp.MustCompile(`FromBase64String\('([^']*)'\)`)
	lfWinGuard  = regexp.MustCompile(`\$cur -eq '([^']*)'`)
	lfWinReadP  = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
	lfWinRelP   = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)

	lfShWriteP = regexp.MustCompile(`base64 -d > '([^']+)'`)
	lfShNewB64 = regexp.MustCompile(`printf '%s' '([^']*)' \| base64 -d`)
	lfShGuard  = regexp.MustCompile(`\[ "\$cur" = '([^']*)' \]`)
	lfShReadP  = regexp.MustCompile(`base64 < '([^']+)'`)
	lfShRelP   = regexp.MustCompile(`rm -f '([^']+)'`)
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func firstGroup(re *regexp.Regexp, s string) (string, bool) {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

// Exec models the four lock scripts atomically at the per-Exec granularity — the
// same guarantee a single remote command execution provides. The create-new
// path is the exclusive arbiter, so two concurrent takeovers cannot both win.
func (f *lockFake) Exec(_ context.Context, c transport.Cmd) (transport.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := c.Script
	win := f.osk == spec.OSWindows

	isCreate := strings.Contains(s, "'CreateNew'") || strings.Contains(s, "set -C")
	hasGuard := lfWinGuard.MatchString(s) || lfShGuard.MatchString(s)
	// readSmallFile is the only script that signals absence via exit 3.
	isRead := strings.Contains(s, "exit 3") && !isCreate

	switch {
	case isRead:
		var path string
		if win {
			path, _ = firstGroup(lfWinReadP, s)
		} else {
			path, _ = firstGroup(lfShReadP, s)
		}
		content, ok := f.files[path]
		if !ok {
			return transport.Result{ExitCode: 3}, nil
		}
		return transport.Result{ExitCode: 0, Stdout: b64(content)}, nil

	case isCreate:
		var path, newB64, expB64 string
		if win {
			path, _ = firstGroup(lfWinOpen, s)
			newB64, _ = firstGroup(lfWinNewB64, s)
			expB64, _ = firstGroup(lfWinGuard, s)
		} else {
			path, _ = firstGroup(lfShWriteP, s)
			newB64, _ = firstGroup(lfShNewB64, s)
			expB64, _ = firstGroup(lfShGuard, s)
		}
		// Atomic takeover: conditionally remove the stale file ONLY if unchanged.
		if hasGuard {
			if cur, held := f.files[path]; held && b64(cur) == expB64 {
				delete(f.files, path)
			}
		}
		if _, held := f.files[path]; held {
			return transport.Result{ExitCode: 48}, nil // exclusive create-new gate
		}
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		f.files[path] = string(raw)
		return transport.Result{ExitCode: 0}, nil

	case hasGuard: // ownership-safe release (compare-and-delete, no create)
		var path, expB64 string
		if win {
			path, _ = firstGroup(lfWinRelP, s)
			expB64, _ = firstGroup(lfWinGuard, s)
		} else {
			path, _ = firstGroup(lfShRelP, s)
			expB64, _ = firstGroup(lfShGuard, s)
		}
		if cur, held := f.files[path]; held && b64(cur) == expB64 {
			delete(f.files, path)
		}
		return transport.Result{ExitCode: 0}, nil
	}
	return transport.Result{ExitCode: 0}, nil
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
