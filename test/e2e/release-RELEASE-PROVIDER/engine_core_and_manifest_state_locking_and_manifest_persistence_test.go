//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 3.2 (Locking and
// Manifest Persistence).
//
// Both scenarios are proved in-process (setup: inline) against a fake Transport
// whose concurrency-safe in-memory filesystem answers the REAL lock and manifest
// shell primitives the engine emits (create-new / read / compare-and-replace /
// compare-and-delete / write). No external service is required:
//
//   - Lock fresh vs stale -> AcquireLock is run against a fake whose canonical
//     `.lock` is FRESH (age < timeout, owned by peer-alpha) and against a fake
//     whose `.lock` is AGED (age >= timeout, owned by peer-bravo). The fresh case
//     must fail fast with engine.CodedError code ERR_LOCKED naming the owner and
//     no lock handle; the aged case must atomically take the lock over, returning
//     a *Lock plus a WARN diagnostic naming the stale owner (DESIGN §13).
//   - Manifest round-trip -> a fully populated engine.Manifest (including
//     last_operation) is persisted with engine.WriteManifest and re-read with
//     engine.ReadManifest through the SAME fake transport; every field must
//     survive byte-for-byte (DESIGN §10.4).
//
// Every Given/When/Then invokes the real engine code and asserts on its result.
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ----------------------------------------------------------------------------
// lmpFake: a filesystem-backed fake Transport that models the POSIX lock and
// manifest primitives the engine emits, so AcquireLock / ReleaseLock /
// WriteManifest / ReadManifest execute their REAL logic end to end. Identifiers
// are prefixed `lmp` so sibling stages sharing the e2e package do not collide.
// ----------------------------------------------------------------------------

type lmpFake struct {
	mu    sync.Mutex
	files map[string]string
}

func lmpNewFake() *lmpFake { return &lmpFake{files: map[string]string{}} }

func (f *lmpFake) seed(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

func (f *lmpFake) get(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[path]
	return c, ok
}

func (f *lmpFake) Connect(context.Context) error                          { return nil }
func (f *lmpFake) Close() error                                           { return nil }
func (f *lmpFake) OS() spec.OSKind                                        { return spec.OSLinux }
func (f *lmpFake) Host() string                                           { return "node-1" }
func (f *lmpFake) Upload(context.Context, io.Reader, int64, string) error { return nil }
func (f *lmpFake) Download(context.Context, string, string) error {
	return fmt.Errorf("not needed")
}

var _ transport.Transport = (*lmpFake)(nil)

var (
	lmpReadSh    = regexp.MustCompile(`base64 < '([^']+)'`)
	lmpCreateSh  = regexp.MustCompile(`ln "\$tmp" '([^']+)'`)
	lmpReplaceSh = regexp.MustCompile(`mv -f "\$tmp" '([^']+)'`)
	lmpDeleteSh  = regexp.MustCompile(`rm -f '([^']+)'`)
	lmpWriteSh   = regexp.MustCompile(`base64 -d > '([^']+)'`)
	lmpNewB64Sh  = regexp.MustCompile(`printf '%s' '([^']*)'`)
	lmpCurNeSh   = regexp.MustCompile(`\[ "\$cur" != '([^']*)' \]`)
)

func lmpB64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func lmpFirst(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// classify maps one emitted script to exactly one primitive. Order matters: the
// compare-and-REPLACE and compare-and-DELETE cases (which also embed a `base64 <`
// read and a `printf ... base64 -d` temp write) MUST be recognized before the
// plain read / create / write cases.
func (f *lmpFake) classify(s string) string {
	switch {
	case strings.Contains(s, `mv -f "$tmp"`):
		return "casreplace"
	case strings.Contains(s, "flock") && strings.Contains(s, "rm -f '"):
		return "casdelete"
	case strings.Contains(s, "base64 < '"):
		return "read"
	case strings.Contains(s, `ln "$tmp"`):
		return "create"
	case strings.Contains(s, "base64 -d > '"):
		return "write"
	case strings.Contains(s, "rm -f '"):
		return "delete"
	}
	return ""
}

func (f *lmpFake) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	if err := ctx.Err(); err != nil {
		return transport.Result{}, err
	}
	s := c.Script
	switch f.classify(s) {
	case "read":
		path := lmpFirst(lmpReadSh, s)
		f.mu.Lock()
		content, ok := f.files[path]
		f.mu.Unlock()
		if !ok {
			return transport.Result{ExitCode: 3}, nil
		}
		return transport.Result{ExitCode: 0, Stdout: lmpB64(content)}, nil

	case "create":
		path := lmpFirst(lmpCreateSh, s)
		newB64 := lmpFirst(lmpNewB64Sh, s)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, held := f.files[path]; held {
			return transport.Result{ExitCode: 48}, nil // EEXIST: contention
		}
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		f.files[path] = string(raw) // atomic publish
		return transport.Result{ExitCode: 0}, nil

	case "write":
		path := lmpFirst(lmpWriteSh, s)
		newB64 := lmpFirst(lmpNewB64Sh, s)
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		f.mu.Lock()
		f.files[path] = string(raw)
		f.mu.Unlock()
		return transport.Result{ExitCode: 0}, nil

	case "casreplace":
		path := lmpFirst(lmpReplaceSh, s)
		expB64 := lmpFirst(lmpCurNeSh, s)
		newB64 := lmpFirst(lmpNewB64Sh, s)
		f.mu.Lock()
		defer f.mu.Unlock()
		cur, ok := f.files[path]
		if !ok {
			return transport.Result{ExitCode: 48}, nil // slot vanished
		}
		if lmpB64(cur) != expB64 {
			return transport.Result{ExitCode: 10}, nil // a fresh successor won
		}
		raw, _ := base64.StdEncoding.DecodeString(newB64)
		f.files[path] = string(raw) // atomic swap stale -> successor
		return transport.Result{ExitCode: 0}, nil

	case "casdelete":
		path := lmpFirst(lmpDeleteSh, s)
		expB64 := lmpFirst(lmpCurNeSh, s)
		f.mu.Lock()
		defer f.mu.Unlock()
		cur, ok := f.files[path]
		if !ok {
			return transport.Result{ExitCode: 48}, nil // already gone
		}
		if lmpB64(cur) != expB64 {
			return transport.Result{ExitCode: 10}, nil // successor owns it: no-op
		}
		delete(f.files, path)
		return transport.Result{ExitCode: 0}, nil

	case "delete":
		path := lmpFirst(lmpDeleteSh, s)
		f.mu.Lock()
		delete(f.files, path)
		f.mu.Unlock()
		return transport.Result{ExitCode: 0}, nil
	}
	return transport.Result{ExitCode: 0}, nil
}

// lmpSeedLock writes a lock-metadata JSON (matching the engine's on-disk lockInfo
// shape) at p.Lock, aged `age` before now and owned by `owner`.
func lmpSeedLock(f *lmpFake, p layout.Paths, owner string, age time.Duration) {
	meta := map[string]string{
		"owner":       owner,
		"op":          "deploy",
		"started_utc": time.Now().Add(-age).UTC().Format(time.RFC3339),
		"token":       "seed-" + owner,
	}
	b, _ := json.Marshal(meta)
	f.seed(p.Lock, string(b))
}

// --- world -------------------------------------------------------------------

type lmpWorld struct {
	// locking scenario
	paths layout.Paths

	freshFake  *lmpFake
	freshLock  *engine.Lock
	freshWarn  string
	freshErr   error
	freshOwner string

	staleFake  *lmpFake
	staleLock  *engine.Lock
	staleWarn  string
	staleErr   error
	staleOwner string

	// manifest scenario
	orig      *engine.Manifest
	manFake   *lmpFake
	roundTrip *engine.Manifest
	rtErr     error
}

// --- Lock fresh vs stale -----------------------------------------------------

func (w *lmpWorld) freshAndAgedLocks(freshOwner, staleOwner string) error {
	w.paths = layout.NewPaths(spec.OSLinux, "/opt/deploy", "sample-svc", "1.0.0")
	w.freshOwner = freshOwner
	w.staleOwner = staleOwner

	w.freshFake = lmpNewFake()
	// Fresh: aged only a minute — well under the 900s (15m) timeout below.
	lmpSeedLock(w.freshFake, w.paths, freshOwner, time.Minute)

	w.staleFake = lmpNewFake()
	// Stale: aged two hours — comfortably past the 900s timeout.
	lmpSeedLock(w.staleFake, w.paths, staleOwner, 2*time.Hour)
	return nil
}

func (w *lmpWorld) acquireRunsAgainstEach() error {
	const timeoutSec = 900
	w.freshLock, w.freshWarn, w.freshErr =
		engine.AcquireLock(context.Background(), w.freshFake, w.paths, "me", "deploy", timeoutSec)
	w.staleLock, w.staleWarn, w.staleErr =
		engine.AcquireLock(context.Background(), w.staleFake, w.paths, "me", "deploy", timeoutSec)
	return nil
}

func (w *lmpWorld) freshLockedStaleOverridden() error {
	// Fresh: fail fast, no handle, ERR_LOCKED naming the owner, no WARN.
	if w.freshErr == nil {
		return fmt.Errorf("fresh lock: expected ERR_LOCKED, got nil error (lock=%v warn=%q)", w.freshLock, w.freshWarn)
	}
	if w.freshLock != nil {
		return fmt.Errorf("fresh lock: expected no lock handle, got %v", w.freshLock)
	}
	var ce *engine.CodedError
	if !errors.As(w.freshErr, &ce) {
		return fmt.Errorf("fresh lock: error is not *engine.CodedError: %T (%v)", w.freshErr, w.freshErr)
	}
	if ce.Code != "ERR_LOCKED" {
		return fmt.Errorf("fresh lock: code = %q, want ERR_LOCKED", ce.Code)
	}
	if !strings.Contains(w.freshErr.Error(), w.freshOwner) {
		return fmt.Errorf("fresh lock: ERR_LOCKED does not name owner %q: %v", w.freshOwner, w.freshErr)
	}
	if w.freshWarn != "" {
		return fmt.Errorf("fresh lock: expected no WARN, got %q", w.freshWarn)
	}
	// The peer's fresh lock must be left untouched (not overwritten).
	if _, held := w.freshFake.get(w.paths.Lock); !held {
		return fmt.Errorf("fresh lock: peer's .lock was removed")
	}

	// Stale: taken over — handle returned, WARN naming the stale owner, no error.
	if w.staleErr != nil {
		return fmt.Errorf("stale lock: expected takeover, got error %v", w.staleErr)
	}
	if w.staleLock == nil {
		return fmt.Errorf("stale lock: expected a *Lock handle, got nil")
	}
	if w.staleWarn == "" {
		return fmt.Errorf("stale lock: expected a WARN diagnostic, got empty")
	}
	if !strings.Contains(w.staleWarn, w.staleOwner) {
		return fmt.Errorf("stale lock: WARN does not name stale owner %q: %q", w.staleOwner, w.staleWarn)
	}
	// The canonical .lock must now hold OUR bytes (the stale owner overwritten).
	cur, held := w.staleFake.get(w.paths.Lock)
	if !held {
		return fmt.Errorf("stale lock: .lock absent after takeover")
	}
	if strings.Contains(cur, w.staleOwner) {
		return fmt.Errorf("stale lock: .lock still carries stale owner %q: %s", w.staleOwner, cur)
	}
	// A clean ownership-safe release must succeed and remove our lock.
	if err := engine.ReleaseLock(context.Background(), w.staleLock); err != nil {
		return fmt.Errorf("stale lock: ReleaseLock failed: %v", err)
	}
	if _, held := w.staleFake.get(w.paths.Lock); held {
		return fmt.Errorf("stale lock: .lock still present after release")
	}
	return nil
}

// --- Manifest round-trip -----------------------------------------------------

func (w *lmpWorld) populatedManifest() error {
	w.orig = &engine.Manifest{
		Schema:           1,
		App:              "sample-svc",
		Pattern:          "blue-green",
		CurrentVersion:   "1.2.3",
		PreviousVersion:  "1.2.2",
		CurrentRelease:   "/opt/deploy/sample-svc/releases/1.2.3",
		ArtifactChecksum: "sha256:abc123",
		ProviderVersion:  "0.9.0",
		Extra:            map[string]string{"region": "westus2", "tier": "prod"},
		LastOperation: engine.LastOp{
			Type:     "deploy",
			Result:   "success",
			Started:  "2026-07-17T10:00:00Z",
			Finished: "2026-07-17T10:05:00Z",
		},
	}
	w.manFake = lmpNewFake()
	return nil
}

func (w *lmpWorld) writtenThenReRead() error {
	p := layout.NewPaths(spec.OSLinux, "/opt/deploy", "sample-svc", "1.2.3")
	if err := engine.WriteManifest(context.Background(), w.manFake, p, w.orig); err != nil {
		return fmt.Errorf("WriteManifest: %w", err)
	}
	if _, ok := w.manFake.get(p.Manifest); !ok {
		return fmt.Errorf("manifest was not persisted at %s", p.Manifest)
	}
	w.roundTrip, w.rtErr = engine.ReadManifest(context.Background(), w.manFake, p)
	return nil
}

func (w *lmpWorld) allFieldsSurvive() error {
	if w.rtErr != nil {
		return fmt.Errorf("ReadManifest: %w", w.rtErr)
	}
	if w.roundTrip == nil {
		return fmt.Errorf("ReadManifest returned nil (manifest read as absent)")
	}
	origJSON, _ := json.Marshal(w.orig)
	rtJSON, _ := json.Marshal(w.roundTrip)
	if string(origJSON) != string(rtJSON) {
		return fmt.Errorf("manifest round-trip drift:\n---orig---\n%s\n---read---\n%s", origJSON, rtJSON)
	}
	// Explicitly assert last_operation (all sub-fields) survived unchanged.
	if w.roundTrip.LastOperation != w.orig.LastOperation {
		return fmt.Errorf("last_operation drift: got %+v, want %+v", w.roundTrip.LastOperation, w.orig.LastOperation)
	}
	if w.roundTrip.LastOperation.Type == "" || w.roundTrip.LastOperation.Result == "" ||
		w.roundTrip.LastOperation.Started == "" || w.roundTrip.LastOperation.Finished == "" {
		return fmt.Errorf("last_operation lost a field: %+v", w.roundTrip.LastOperation)
	}
	return nil
}

// InitializeScenario_engine_core_and_manifest_state_locking_and_manifest_persistence
// registers the step definitions for the Stage 3.2 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_engine_core_and_manifest_state_locking_and_manifest_persistence(ctx *godog.ScenarioContext) {
	w := &lmpWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = lmpWorld{}
		return c, nil
	})

	ctx.Step(`^a fake transport holding a fresh \.lock owned by "([^"]+)" and another holding an aged \.lock owned by "([^"]+)"$`, w.freshAndAgedLocks)
	ctx.Step(`^AcquireLock runs against each$`, w.acquireRunsAgainstEach)
	ctx.Step(`^the fresh case yields ERR_LOCKED naming the owner and the aged case overwrites with a WARN diagnostic naming the stale owner$`, w.freshLockedStaleOverridden)

	ctx.Step(`^a fully populated manifest struct including last_operation$`, w.populatedManifest)
	ctx.Step(`^it is written then re-read through a fake transport$`, w.writtenThenReRead)
	ctx.Step(`^all fields including last_operation survive unchanged$`, w.allFieldsSurvive)
}

// TestE2E_engine_core_and_manifest_state_locking_and_manifest_persistence is the
// go test entrypoint for the Stage 3.2 godog suite.
func TestE2E_engine_core_and_manifest_state_locking_and_manifest_persistence(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_engine_core_and_manifest_state_locking_and_manifest_persistence,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"engine_core_and_manifest_state_locking_and_manifest_persistence.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
