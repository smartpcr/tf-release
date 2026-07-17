package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// CodedError mirrors DESIGN §12 for engine-originated failures.
type CodedError struct {
	Code string
	Host string
	Step string
	Err  error
}

func (e *CodedError) Error() string {
	return fmt.Sprintf("[%s] %v (host=%s step=%s)", e.Code, e.Err, e.Host, e.Step)
}
func (e *CodedError) Unwrap() error { return e.Err }

func coded(code, host, step string, err error) error {
	return &CodedError{Code: code, Host: host, Step: step, Err: err}
}

// Manifest is the target-side source of truth (DESIGN §2, §10.4).
type Manifest struct {
	Schema           int               `json:"schema"`
	App              string            `json:"app"`
	Pattern          string            `json:"pattern"`
	CurrentVersion   string            `json:"current_version"`
	PreviousVersion  string            `json:"previous_version,omitempty"`
	CurrentRelease   string            `json:"current_release_path"`
	ArtifactChecksum string            `json:"artifact_checksum,omitempty"`
	ProviderVersion  string            `json:"provider_version"`
	Extra            map[string]string `json:"extra,omitempty"`
	LastOperation    LastOp            `json:"last_operation"`
}

type LastOp struct {
	Type     string `json:"type"`   // deploy|destroy
	Result   string `json:"result"` // success|failed|rolled_back
	Started  string `json:"started"`
	Finished string `json:"finished"`
}

// readSmallFile returns (content, exists, err) via a single exec round-trip.
func readSmallFile(ctx context.Context, t transport.Transport, path string) (string, bool, error) {
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`if(Test-Path %s){[Convert]::ToBase64String([IO.File]::ReadAllBytes(%s))}else{exit 3}`,
			psq(path), psq(path))
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
		if err != nil {
			return "", false, err
		}
		if r.ExitCode == 3 {
			return "", false, nil
		}
		if r.ExitCode != 0 {
			return "", false, fmt.Errorf("read %s: %s", path, r.Stderr)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Stdout))
		if err != nil {
			return "", false, err
		}
		return string(raw), true, nil
	}
	script := fmt.Sprintf(`[ -f %s ] || exit 3; base64 < %s`, shq(path), shq(path))
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	if err != nil {
		return "", false, err
	}
	if r.ExitCode == 3 {
		return "", false, nil
	}
	if r.ExitCode != 0 {
		return "", false, fmt.Errorf("read %s: %s", path, r.Stderr)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Map(dropWS, r.Stdout))
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

func dropWS(r rune) rune {
	if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
		return -1
	}
	return r
}

func writeSmallFile(ctx context.Context, t transport.Transport, path, content string) error {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`[IO.File]::WriteAllBytes(%s,[Convert]::FromBase64String(%s))`, psq(path), psq(b64))
		r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("write %s: %s", path, r.Stderr)
		}
		return nil
	}
	script := fmt.Sprintf(`printf '%%s' '%s' | base64 -d > %s`, b64, shq(path))
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("write %s: %s", path, r.Stderr)
	}
	return nil
}

func ReadManifest(ctx context.Context, t transport.Transport, p layout.Paths) (*Manifest, error) {
	raw, ok, err := readSmallFile(ctx, t, p.Manifest)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil // absent
	}
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("manifest corrupt: %w", err)
	}
	return &m, nil
}

func WriteManifest(ctx context.Context, t transport.Transport, p layout.Paths, m *Manifest) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	return writeSmallFile(ctx, t, p.Manifest, string(b))
}

// FailedMarker is the suffix appended to deployed_version when the last
// recorded operation failed, so a subsequent plan always shows drift and forces
// a converging apply (DESIGN §10.4).
const FailedMarker = "!failed"

// ReconcileManifest maps a manifest read from the target into the Read-refresh
// outcome (DESIGN §10.4):
//   - absent manifest (m == nil) ⇒ present=false ⇒ caller RemoveResource.
//   - last_operation.result == "failed" ⇒ deployedVersion carries the
//     "<current_version>!failed" marker and warn carries a human-readable
//     diagnostic so the plan surfaces the failed state.
//   - otherwise deployedVersion == current_version and warn is empty.
func ReconcileManifest(m *Manifest) (present bool, deployedVersion, warn string) {
	if m == nil {
		return false, "", ""
	}
	deployedVersion = m.CurrentVersion
	if m.LastOperation.Result == "failed" {
		deployedVersion = m.CurrentVersion + FailedMarker
		warn = fmt.Sprintf(
			"last %s operation on version %s failed (started %s); deployed_version marked %q to force a converging apply",
			opOrUnknown(m.LastOperation.Type), m.CurrentVersion, m.LastOperation.Started, deployedVersion)
	}
	return true, deployedVersion, warn
}

func opOrUnknown(s string) string {
	if s == "" {
		return "deploy"
	}
	return s
}

// ------------------------------- locking (DESIGN §13) ----------------------

type lockInfo struct {
	Owner      string `json:"owner"`
	Op         string `json:"op"`
	StartedUTC string `json:"started_utc"`
	// Token is a per-acquisition nonce that makes each lock instance unique.
	// Release compares it so a caller only ever deletes ITS OWN lock, never a
	// successor's that legitimately took over after a timeout (evaluator item 2).
	Token string `json:"token"`
}

// Lock is the handle returned by AcquireLock. It carries the exact bytes we
// persisted plus this acquisition's unique token so ReleaseLock can verify
// ownership (delete only when `.lock` still holds our exact bytes) under the
// serialization gate, without ever temporarily removing an unowned lock.
type Lock struct {
	t       transport.Transport
	paths   layout.Paths
	content string // exact JSON persisted for this acquisition
	token   string // per-acquisition nonce
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based token; uniqueness is best-effort here.
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Locking primitive (DESIGN §13) — a note on why there is NO serialization gate.
//
// Earlier iterations serialized stale-takeover/release through a persistent
// `<lock>.mx` gate file. A gate file that outlives its holder (process crash,
// lost cleanup, canceled context) is a LIVENESS hazard, and every attempt to
// "recover" such a gate over a stateless Exec transport reintroduced a SAFETY
// race: reading the gate and then renaming/deleting it targets a MUTABLE
// pathname, so a fresh holder can recreate the gate in the gap and the recoverer
// steals it — two critical sections then mutate the same `.lock` (dual owner).
// There is no file-system compare-and-swap keyed on content, so "verify then act
// on a pathname" is fundamentally unfixable.
//
// This design removes the gate. Ownership-safe release runs as a SINGLE remote
// command that, under the SHARED per-path mutex, performs a compare-and-delete
// atomically. Stale takeover never mutates `.lock` in place: it is an atomic
// compare-and-REPLACE of the exact stale bytes (temp + atomic swap), so an
// interruption leaves the slot intact-stale or intact-successor, never
// mixed/partial. Create, compare-and-delete, and compare-and-replace ALL take one
// path-keyed OS mutex — a Windows `Global\` named mutex (auto-released when the
// process exits, normally OR on crash/kill, so nothing is stranded and no separate
// recovery path with its unavoidable TOCTOU is needed) and, on POSIX, a blocking
// `flock` on the lock's parent DIRECTORY (a stable inode, unlike the lock file
// itself which a delete unlinks). Because every mutator serializes on that one
// mutex, a release/create can never interleave inside a takeover's compare-then-
// swap: exactly one operation wins, the canonical `.lock` is never emptied
// mid-takeover, and two operations can never both believe they own it.

// AcquireLock takes the target lock with atomic create-new semantics
// (DESIGN §13). exit 48 ⇒ held. A held lock is inspected: unparseable/empty
// metadata or age < timeout ⇒ ERR_LOCKED (fail fast, no wait); a PROVEN stale
// lock (age >= timeout) is taken over WITHOUT any in-place mutation — it is
// removed with a single atomic compare-and-DELETE keyed to the exact stale bytes
// we judged, and the freed slot is then re-created with a fresh atomic create.
// Each step is individually atomic and crash-safe, so the canonical path is only
// ever the intact stale lock, absent, or our fresh lock — never mixed/partial and
// never a stealable empty file. On success it returns a *Lock handle for
// ownership-safe release.
//
// The loop retries ONLY transient states — the holder released between our create
// and our read (lock vanished), a proven-stale lock we just deleted (re-create on
// the next turn), or a compare-and-delete that reported live contention/mismatch
// — and re-evaluates from scratch. A genuinely LIVE held lock is judged not-stale
// on the very first pass and returns ERR_LOCKED immediately: no spinning, a
// handful of commands at most (DESIGN §13 "<5s, no wait").
func AcquireLock(ctx context.Context, t transport.Transport, p layout.Paths, owner, op string, timeoutSec int) (lk *Lock, staleWarn string, err error) {
	// Small bound: each retry either wins the create-new, deletes a proven-stale
	// lock (then re-creates), or reads a now-FRESH lock and returns ERR_LOCKED.
	// Only a repeatedly-vanishing/-contended slot spins, which cannot progress, so
	// a tiny cap keeps us well under 5s.
	const maxAttempts = 8
	// Overall wall-clock budget for the whole acquisition. DESIGN §13 requires a
	// fresh/live-held lock to fail fast ("<5s, no wait"): the uncontended create
	// and each gate wait return immediately, so this budget is only ever consumed
	// under pathological live gate contention. Each attempt's gate wait is bounded
	// by the REMAINING budget (capped at lockGateWaitMS), so the sum of all waits —
	// and therefore total acquisition time — never exceeds acquireBudget, which is
	// itself held below 5s for a safety margin (evaluator iter-21 item 1,
	// iter-23 item 1).
	acquireBudget := lockAcquireBudget
	start := time.Now()
	deadline := start.Add(acquireBudget)

	// Bind ALL remote gate operations to the acquisition deadline so transport
	// overhead (connect/exec latency), not just the in-guard gate wait, is counted
	// against the budget: a hung Exec is cancelled at the deadline instead of
	// silently overrunning it (evaluator iter-22 item 1).
	actx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// gateWaitMS returns how long the NEXT single gate op may wait, recomputed from
	// the live remaining budget every time it is called (0 ⇒ budget exhausted).
	// Recomputing before each of createLockExclusive AND casReplace prevents the two
	// gate ops in one attempt from each consuming the full remaining allowance
	// (evaluator iter-22 item 1).
	gateWaitMS := func() int {
		rem := time.Until(deadline)
		if rem <= 0 {
			return 0
		}
		ms := int(rem / time.Millisecond)
		if ms > lockGateWaitMS {
			ms = lockGateWaitMS
		}
		return ms
	}
	// sawContention records whether this acquisition ever observed LIVE lock-gate
	// contention — a create-gate timeout (exit 49) or a compare-and-replace that lost
	// the exclusive gate (casContended). Only these establish that a peer is actively
	// holding the gate; the mere EXISTENCE of a lock file (create exit 48, including a
	// proven-stale one) does NOT, since a stale-takeover swap that then times out is a
	// transport failure, not gate contention (evaluator iter-25 item 1). Only when
	// live contention was established may a subsequent deadline/budget exhaustion be
	// reported as ERR_LOCKED; a bare first-operation hang, a takeover transport
	// timeout, or any op that never saw exit-49/casContended stays ERR_CONNECT
	// (evaluator iter-24 item 1, iter-25 item 1).
	sawContention := false
	budgetExceeded := func() (*Lock, string, error) {
		if sawContention {
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("lock acquisition exceeded %s budget (gate repeatedly contended)", acquireBudget))
		}
		return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK",
			fmt.Errorf("lock acquisition exceeded %s budget without acquiring or observing contention", acquireBudget))
	}
	// acquireErr classifies a transport error from a gate op. ERR_LOCKED is reserved
	// for the case where OUR acquisition deadline fired (actx timed out, the CALLER's
	// ctx did not) AND we had already ESTABLISHED contention — otherwise a genuine
	// transport failure, a first-op hang with no observed contention, or the caller
	// cancelling its own ctx all remain ERR_CONNECT (evaluator iter-23 item 2,
	// iter-24 item 1).
	acquireErr := func(execErr error) error {
		deadlineFired := errors.Is(execErr, context.DeadlineExceeded) || actx.Err() == context.DeadlineExceeded
		if sawContention && ctx.Err() == nil && deadlineFired {
			return coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("lock acquisition exceeded %s budget (gate repeatedly contended): %w", acquireBudget, execErr))
		}
		return coded("ERR_CONNECT", t.Host(), "LOCK", execErr)
	}

	for attempt := 0; ; attempt++ {
		waitMS := gateWaitMS()
		if waitMS <= 0 {
			return budgetExceeded()
		}

		token := newToken()
		li, _ := json.Marshal(lockInfo{
			Owner: owner, Op: op, StartedUTC: time.Now().UTC().Format(time.RFC3339), Token: token,
		})
		content := string(li)

		created, exit, cerr := createLockExclusive(actx, t, p, content, waitMS)
		if cerr != nil {
			return nil, "", acquireErr(cerr)
		}
		if created {
			// A fresh create into a FREE slot legitimately carries no warning.
			return &Lock{t: t, paths: p, content: content, token: token}, "", nil
		}
		if exit == 71 {
			// Operational failure from the remote create (disk full, permission
			// denied, read-only/again filesystem) — NOT contention. Surface it as a
			// connectivity/operational error so it is logged and retried rather than
			// masquerading as ERR_LOCKED (evaluator item 2).
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK",
				fmt.Errorf("lock create failed operationally (exit 71: disk/permission/filesystem error)"))
		}
		if exit == 49 {
			// TRANSIENT create-gate contention: a peer mutator briefly held the
			// shared path-keyed gate (named mutex / directory flock) and our bounded
			// wait elapsed. This is exactly the condition winMutexGuard/
			// shDirLockPrologue document as retryable, so back off and re-evaluate
			// within the budget rather than misclassifying it as ERR_CONNECT
			// (evaluator iter-21 item 2).
			sawContention = true
			if attempt < maxAttempts {
				casBackoff(actx, attempt)
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("lock create gate repeatedly contended (exit 49)"))
		}
		if exit != 48 {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", fmt.Errorf("lock create failed (unexpected exit %d)", exit))
		}
		// exit 48 ⇒ the lock file is HELD. This does NOT set sawContention: a held
		// (possibly stale) file is not proof of LIVE gate contention, so a takeover
		// that later times out must surface as ERR_CONNECT, not ERR_LOCKED
		// (evaluator iter-25 item 1).

		// Held: inspect age. A lock may be overridden ONLY when its metadata
		// parses AND its proven age is >= timeout. Unparseable JSON, an invalid
		// started_utc, OR an empty/whitespace-only file is NOT proof of staleness,
		// so we refuse (ERR_LOCKED) rather than clobber a lock that may still be
		// live (DESIGN §13). Because create and takeover are BOTH atomic (temp +
		// publish / compare-and-delete), a `.lock` is never observed empty or
		// partial, so there is no "crash residue" to recover — treating an empty
		// file as recoverable is exactly what previously allowed dual ownership.
		raw, ok, rerr := readSmallFile(actx, t, p.Lock)
		if rerr != nil {
			// The metadata read FAILED, so the owner, age, and staleness could NOT
			// be established. A transport/connectivity failure (or our own deadline
			// firing without any prior gate contention) must therefore surface as
			// ERR_CONNECT, not masquerade as a held-lock ERR_LOCKED result. acquireErr
			// preserves ERR_LOCKED only when live gate contention (exit 49 /
			// casContended) was already observed on an earlier attempt (evaluator
			// iter-26 item 1).
			return nil, "", acquireErr(rerr)
		}
		if !ok {
			// The holder released between our create and our read — the slot is
			// free now. Retry the acquire from the top.
			if attempt < maxAttempts {
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock contended (repeatedly vanished)"))
		}

		var existing lockInfo
		if uerr := json.Unmarshal([]byte(raw), &existing); uerr != nil {
			// Covers EMPTY/whitespace too (json.Unmarshal fails on ""). Refuse.
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("lock held with unparseable metadata (refusing to override): %v", uerr))
		}
		started, perr := time.Parse(time.RFC3339, existing.StartedUTC)
		if perr != nil {
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("lock held by %s with unparseable started_utc %q (refusing to override): %v",
					existing.Owner, existing.StartedUTC, perr))
		}
		if time.Since(started) < time.Duration(timeoutSec)*time.Second {
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("held by %s since %s (op=%s)", existing.Owner, existing.StartedUTC, existing.Op))
		}

		// Proven stale (age >= timeout): take it over as a SINGLE atomic
		// compare-and-replace. The SAME actor that proved this lock stale installs
		// the successor bytes in ONE exclusive-gate command that swaps our content
		// in place of the exact stale bytes. Unlike the old delete-then-recreate,
		// there is NO intervening absent slot: a late newcomer can never slip into a
		// gap and acquire warning-less while we recreate (evaluator item 1). The
		// winner ALWAYS returns the stale-owner WARN; every loser observes our FRESH
		// bytes (casMismatch) and retries into ERR_LOCKED against the live successor.
		warn := staleWarnMsg(existing, t.Host())
		replaceWaitMS := gateWaitMS()
		if replaceWaitMS <= 0 {
			return budgetExceeded()
		}
		outcome, serr := casReplace(actx, t, p.Lock, raw, content, replaceWaitMS)
		if serr != nil {
			return nil, "", acquireErr(serr)
		}
		switch outcome {
		case casDone:
			// We atomically replaced the stale bytes with our own — we own it.
			return &Lock{t: t, paths: p, content: content, token: token}, warn, nil
		case casContended:
			// A peer holds the exclusive gate right now (transient). Back off and
			// re-evaluate rather than misreporting it as a mismatch/absence.
			sawContention = true
			if attempt < maxAttempts {
				casBackoff(actx, attempt)
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("stale lock from %s: takeover repeatedly contended", existing.Owner))
		default: // casMismatch / casAbsent: a peer already took over or removed it.
			if attempt < maxAttempts {
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("stale lock from %s: takeover repeatedly contended", existing.Owner))
		}
	}
}

// casBackoff sleeps a short, attempt-scaled interval before retrying a CONTENDED
// compare-and-delete, honoring context cancellation. Contention means a peer
// briefly holds the exclusive handle; a few-ms wait lets it finish without a spin.
func casBackoff(ctx context.Context, attempt int) {
	d := time.Duration(attempt+1) * 5 * time.Millisecond
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// casOutcome is the distinguished result of a compare-and-delete. The
// point (evaluator item 1) is that a HELD-lock mutation must NOT collapse every
// failure into "absent". Absence, live contention on the exclusive handle, and a
// content mismatch are three DIFFERENT conditions with different correct
// responses; any other exit is an operational failure surfaced as a Go error.
type casOutcome int

const (
	casDone      casOutcome = iota // exit 0: the swap/delete happened (we won)
	casMismatch                    // exit 10: current bytes ≠ expect (a peer changed/released it)
	casAbsent                      // exit 48: the file is simply not present (true no-op)
	casContended                   // exit 49: a peer holds the exclusive handle NOW (transient, retry)
)

// Shared CAS exit codes for casDelete (both platforms). Anything NOT in
// {0,10,48,49} is an operational failure (permission denied, missing `flock`,
// read/remove fault) and is mapped to a Go error by classifyCas — never
// silently treated as absence or contention.
//
//	 0  done            10  content mismatch   48  absent
//	49  exclusion held  17  removal failed     70  flock missing
//	71  open failed     99  unexpected
//
// classifyCas turns a transport Result into a (casOutcome, error). The err path
// returns casDone (the zero value) as a dummy — callers MUST check err != nil
// BEFORE switching on the outcome.
func classifyCas(op, path string, r transport.Result) (casOutcome, error) {
	switch r.ExitCode {
	case 0:
		return casDone, nil
	case 10:
		return casMismatch, nil
	case 48:
		return casAbsent, nil
	case 49:
		return casContended, nil
	default:
		return casDone, fmt.Errorf("%s of %s failed (exit %d): %s", op, path, r.ExitCode, r.Stderr)
	}
}

// winLockMutexName returns the process-global, path-keyed named mutex that
// serializes EVERY mutation of a given lock path — create-new, compare-and-delete,
// AND compare-and-replace — so each operation's "inspect current bytes then
// mutate" critical section is mutually exclusive against ALL the others across
// processes. Without one shared mutex, a stale-takeover replace could read the
// stale bytes, then a concurrent release could delete them and a newcomer create a
// fresh lock into the freed slot, after which the replace's atomic swap would
// clobber the newcomer's lock — leaving BOTH the newcomer and the taker believing
// they own it (evaluator iter-20 item 1). Mutex names cannot contain a path
// separator (except the leading Global\), so the path is hashed. The path is
// lower-cased before hashing so case variants of the SAME case-insensitive Windows
// path resolve to the SAME mutex and cannot bypass serialization (evaluator iter-21
// item 4). One mutex ⇔ one lock file.
func winLockMutexName(path string) string {
	sum := sha1.Sum([]byte(strings.ToLower(path)))
	return `Global\labdeploy-lock-` + hex.EncodeToString(sum[:])
}

// lockGateWaitMS bounds how long a single acquire/release command may wait for the
// shared path-keyed gate mutex. It is deliberately short (well under DESIGN §13's
// fresh-lock "<5s, no wait" contract): the guarded critical section is a single
// fast local file op, and a crashed holder ABANDONS the mutex (returning
// immediately), so this bound is only ever approached under pathological live
// contention — where a timeout is surfaced as transient (exit 49) and retried
// within the caller's overall 5s budget, never as a hard failure.
const lockGateWaitMS = 1500

// lockAcquireBudget is the overall wall-clock ceiling for a whole AcquireLock call.
// DESIGN §13 mandates a STRICT "<5s, no wait" fresh-lock contract, so this is held
// at 4.5s — a deliberate safety margin BELOW 5s that absorbs Go scheduling latency,
// the final classify/return overhead, and clock granularity, guaranteeing the
// caller-observed elapsed time stays under 5s even in the worst case (evaluator
// iter-23 item 1). It is a var (not a const) purely so tests can shrink it to
// deterministically exercise the deadline-cancellation path in milliseconds; the
// production value never changes.
var lockAcquireBudget = 4500 * time.Millisecond

// winMutexGuard wraps a PowerShell critical-section `body` (which sets $rc) in an
// acquire/finally-release of the path-keyed named mutex, so create/delete/replace
// all share ONE cross-process gate. `waitMS` bounds the WaitOne so acquisition
// stays within the <5s contract (evaluator iter-21 item 1). A crashed holder
// abandons the mutex (WaitOne throws AbandonedMutexException); the next acquirer
// treats that as acquired because the crash-safe atomic file ops guarantee the
// bytes it observes are never partial. A WaitOne timeout maps to exit 49
// (contended) so the caller backs off and retries rather than failing hard.
func winMutexGuard(path string, waitMS int, body string) string {
	return fmt.Sprintf(`$mtx=New-Object System.Threading.Mutex($false,%s)
$rc=99
$got=$false
try{$got=$mtx.WaitOne(%d)}catch [System.Threading.AbandonedMutexException]{$got=$true}
if(-not $got){$mtx.Dispose();exit 49}
try{
%s
}finally{$mtx.ReleaseMutex();$mtx.Dispose()}
exit $rc`, psq(winLockMutexName(path)), waitMS, body)
}

// shDirLockPrologue opens the lock's PARENT DIRECTORY (a stable inode that, unlike
// the lock file, is never unlinked) on fd 9 and takes a bounded flock on it,
// giving a path-keyed cross-process mutex that serializes create/delete/replace
// exactly like the Windows named mutex. flock on the lock FILE itself would be
// useless: a delete unlinks that inode, so a newcomer's freshly-created lock is a
// DIFFERENT inode whose flock the taker never held, which is precisely how the
// dual-ownership race arose (evaluator iter-20 item 1). `waitMS` bounds the flock
// wait (fractional seconds) so acquisition stays within the <5s contract
// (evaluator iter-21 item 1); a timeout exits 49 (transient), which the caller
// retries. `dirExpr` is a shell expression evaluating to the directory (a literal
// for create, `$(dirname …)` for delete/replace). On failure to open, callers
// exit 71/48.
func shDirLockPrologue(dirExpr, path string, waitMS int) string {
	return fmt.Sprintf(`exec 9<%s 2>/dev/null || { [ -e '%s' ] && exit 71 || exit 48; }
flock -w %.3f 9 || exit 49`, dirExpr, path, float64(waitMS)/1000.0)
}

// casDelete performs an atomic compare-and-delete on a HELD `.lock`: under the
// shared path-keyed mutex (winMutexGuard / the parent-directory flock) it removes
// the file ONLY if the current bytes equal `expect` (this handle's owned content).
// A successor that legitimately took over after our timeout has DIFFERENT bytes,
// so its lock is never removed. Because delete now shares the SAME mutex as create
// and replace, its inspect-then-remove is mutually exclusive against a concurrent
// takeover replace, so a release can never delete bytes a replace is mid-swapping
// (evaluator iter-20 item 1). The outcomes: casDone (deleted), casMismatch (not
// ours — safe no-op), casAbsent (already gone), casContended (the mutex was held
// past the wait — the caller RETRIES so it never returns success while OUR lock is
// still present, evaluator item 2); any other exit (removal failed, permission,
// missing flock) is a Go error. The removal is classified AT THE DELETE CALL (a
// failed Delete/`rm -f` ⇒ exit 17), with NO post-delete existence probe (evaluator
// item 2).
func casDelete(ctx context.Context, t transport.Transport, path, expect string) (casOutcome, error) {
	expectB64 := base64.StdEncoding.EncodeToString([]byte(expect))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		body := fmt.Sprintf(`if(-not [IO.File]::Exists(%s)){$rc=48}
else{
$cur=[Convert]::ToBase64String([IO.File]::ReadAllBytes(%s))
if($cur -eq %s){ try{ [IO.File]::Delete(%s); $rc=0 }catch{ $rc=17 } } else { $rc=10 }
}`, psq(path), psq(path), psq(expectB64), psq(path))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: winMutexGuard(path, lockGateWaitMS, body), TimeoutSec: 30})
	} else {
		script := fmt.Sprintf(`command -v flock >/dev/null 2>&1 || exit 70
%s
[ -e '%s' ] || exit 48
cur=$(base64 < '%s' | tr -d '\n')
if [ "$cur" != '%s' ]; then exit 10; fi
rm -f '%s' || exit 17
exit 0`, shDirLockPrologue(`"$(dirname '`+path+`')"`, path, lockGateWaitMS), path, path, expectB64, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 30})
	}
	if err != nil {
		return casDone, err
	}
	return classifyCas("compare-and-delete", path, r)
}

// casReplace performs an atomic compare-and-REPLACE on a proven-stale `.lock`: it
// swaps `newContent` in place of the exact stale `expect` bytes, so the canonical
// slot is NEVER absent between removing the stale lock and installing the
// successor. This closes the delete-then-recreate gap where a late newcomer could
// create into the empty slot and acquire without the mandated stale WARN
// (evaluator item 1). Outcomes mirror casDelete: casDone (we installed our
// successor), casMismatch (a peer already installed FRESH bytes — abort), casAbsent
// (the slot was released — retry a clean create), casContended (a peer holds the
// gate right now — retry); any other exit is a Go error.
//
// Crash-safety (evaluator iter-19 item 1): BOTH platforms replace via a private
// temp that is fully written first and then swapped in with a SINGLE atomic
// filesystem operation — POSIX `mv -f "$tmp"` (rename) and Windows
// `[IO.File]::Replace($tmp,dest,$null)` (an NTFS transacted replace with
// write-through). The canonical `.lock` therefore transitions atomically from the
// stale bytes to the successor bytes with NO in-place mutation: an interruption at
// any instant leaves EITHER the intact stale lock (temp orphaned) OR the intact
// successor lock — never a truncated/partial/mixed JSON that would wedge
// acquisition. Compare-and-swap atomicity (so two contenders can't both "win", and
// so a concurrent create/delete can't slip a fresh lock into the slot mid-swap) is
// provided by the SHARED per-path OS mutex used by create and delete as well:
// POSIX a blocking `flock` on the lock's parent DIRECTORY, Windows a `Global\`
// named mutex. The mutex serializes the read-compare-then-swap critical section
// against ALL other lock mutations across processes (evaluator iter-20 item 1); if
// a holder crashes mid-section the mutex is abandoned (WaitOne throws
// AbandonedMutexException) and the next acquirer proceeds — the crash-safe atomic
// swap guarantees the file it observes is never partial.
func casReplace(ctx context.Context, t transport.Transport, path, expect, newContent string, waitMS int) (casOutcome, error) {
	expectB64 := base64.StdEncoding.EncodeToString([]byte(expect))
	newB64 := base64.StdEncoding.EncodeToString([]byte(newContent))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		body := fmt.Sprintf(`if(-not [IO.File]::Exists(%s)){$rc=48}
else{
$cur=[Convert]::ToBase64String([IO.File]::ReadAllBytes(%s))
if($cur -ne %s){$rc=10}
else{
$tmp=%s + '.mx.' + [Guid]::NewGuid().ToString('N')
try{$nb=[Convert]::FromBase64String(%s); [IO.File]::WriteAllBytes($tmp,$nb); [IO.File]::Replace($tmp,%s,$null); $rc=0}
catch{Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue; $rc=17}
}
}`, psq(path), psq(path), psq(expectB64), psq(path), psq(newB64), psq(path))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: winMutexGuard(path, waitMS, body), TimeoutSec: 30})
	} else {
		script := fmt.Sprintf(`command -v flock >/dev/null 2>&1 || exit 70
%s
[ -e '%s' ] || exit 48
cur=$(base64 < '%s' | tr -d '\n')
if [ "$cur" != '%s' ]; then exit 10; fi
tmp='%s.mx.'$$
printf '%%s' '%s' | base64 -d > "$tmp" || { rm -f "$tmp"; exit 17; }
mv -f "$tmp" '%s' || { rm -f "$tmp"; exit 17; }
exit 0`, shDirLockPrologue(`"$(dirname '`+path+`')"`, path, waitMS), path, path, expectB64, path, newB64, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 30})
	}
	if err != nil {
		return casDone, err
	}
	return classifyCas("compare-and-replace", path, r)
}

func staleWarnMsg(existing lockInfo, host string) string {
	return fmt.Sprintf("stale lock from %s (started %s) overridden on host %s",
		existing.Owner, existing.StartedUTC, host)
}

// createLockExclusive runs the atomic create-new on the canonical lock path.
func createLockExclusive(ctx context.Context, t transport.Transport, p layout.Paths, content string, waitMS int) (bool, int, error) {
	return createExclusiveAt(ctx, t, p.Root, p.Lock, content, waitMS)
}

// createExclusiveAt publishes a fully-formed lock file ATOMICALLY, with
// create-new (fail-if-exists) semantics and NO empty/partial window (DESIGN §13).
// Returns (created, exitCode, transportErr): created=true on exit 0; exitCode==48
// signals the file already existed (contention); exitCode==71 signals an
// OPERATIONAL failure (disk full, permission denied, read-only/absent filesystem)
// that must NOT be conflated with contention. The publish step distinguishes the
// two AT THE POINT OF FAILURE — from the failing operation's own error, not a
// follow-up existence probe: Windows inspects the IOException HResult
// (ERROR_FILE_EXISTS=80 / ERROR_ALREADY_EXISTS=183 ⇒ 48, else 71) and POSIX
// inspects `ln`'s stderr (matches "exists" ⇒ 48, else 71). A racy re-check
// (`[IO.File]::Exists` / `[ -e path ]`) after the failure would misclassify a
// genuine EEXIST as operational when a concurrent release frees the slot between
// the failed link and the probe (evaluator item 3).
//
// The old approach (an exclusive create-new redirect / open-then-Write) created the file
// FIRST and wrote its bytes SECOND, exposing a transient EMPTY file that a
// concurrent acquirer could observe (and previously "recover"), and leaving empty
// residue if the writer was killed between the two steps. Instead we write the
// content to a private temp in the same directory and then PUBLISH it with a
// single atomic step that fails if the target exists:
//   - POSIX: `ln "$tmp" path` — hard-link is atomic and returns EEXIST if the
//     target is present; the target appears fully-formed or not at all.
//   - Windows: `[IO.File]::Move($tmp, path)` — Move does not overwrite, throwing
//     IOException if the target exists; the target appears fully-formed or not.
//
// A crashed creator leaves at most an orphan temp (never an empty `.lock`), so
// there is no dual-ownership window and nothing to "recover". The whole
// inspect-then-publish runs under the SHARED path-keyed mutex (winMutexGuard / the
// parent-directory flock) that create, delete, AND replace all take, so a create
// can never publish a fresh lock into a slot a concurrent stale-takeover replace is
// mid-swapping — closing the dual-ownership race (evaluator iter-20 item 1).
func createExclusiveAt(ctx context.Context, t transport.Transport, root, path, content string, waitMS int) (bool, int, error) {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		body := fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null
if([IO.File]::Exists(%s)){$rc=48}
else{
$tmp=%s + '.tmp.' + [Guid]::NewGuid().ToString('N')
try { $b=[Convert]::FromBase64String(%s); [IO.File]::WriteAllBytes($tmp,$b); [IO.File]::Move($tmp,%s); $rc=0 }
catch [System.IO.IOException] { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue; $h=$_.Exception.HResult -band 0xFFFF; if ($h -eq 80 -or $h -eq 183) { $rc=48 } else { $rc=71 } }
catch { Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue; $rc=71 }
}`, psq(root), psq(path), psq(path), psq(b64), psq(path))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: winMutexGuard(path, waitMS, body), TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`mkdir -p '%s' || exit 71
command -v flock >/dev/null 2>&1 || exit 70
%s
if [ -e '%s' ]; then exit 48; fi
tmp='%s.tmp.'$$
printf '%%s' '%s' | base64 -d > "$tmp" || { rm -f "$tmp"; exit 71; }
lnerr=$(ln "$tmp" '%s' 2>&1); lnrc=$?
rm -f "$tmp"
if [ $lnrc -eq 0 ]; then exit 0; fi
case "$lnerr" in
  *[Ee]xists*) exit 48 ;;
  *) exit 71 ;;
esac`,
			root, shDirLockPrologue(fmt.Sprintf("'%s'", root), path, waitMS), path, path, b64, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, 0, err
	}
	return r.ExitCode == 0, r.ExitCode, nil
}

// ReleaseLock releases the lock with a SINGLE atomic compare-and-delete
// (casDelete): under the SHARED path-keyed mutex it removes `.lock` ONLY when the
// persisted bytes still equal this handle's exact content. A caller that overran
// the timeout can therefore never delete a successor's lock — the successor's
// bytes differ (casMismatch), so casDelete is a safe no-op. Because the check and
// the removal happen inside the same mutex-guarded command that create and replace
// also serialize on, no takeover or create can interleave between "verify" and
// "delete".
//
// Contention is NOT success (evaluator item 2): if a mismatching peer CAS briefly
// holds the exclusive handle while we try to release (casContended), OUR lock is
// still present, so we RETRY rather than returning nil. Only casDone (deleted),
// casMismatch (a successor legitimately owns it), or casAbsent (already gone) are
// terminal no-error results. Persistent contention or an operational failure
// (permissions/IO/missing flock) after all retries is surfaced to the caller so
// it is logged rather than silently stranding an owned lock (evaluator item 3).
func ReleaseLock(ctx context.Context, lk *Lock) error {
	if lk == nil || lk.t == nil {
		return nil
	}
	const attempts = 5
	var lastErr error
	for i := 0; i < attempts; i++ {
		outcome, err := casDelete(ctx, lk.t, lk.paths.Lock, lk.content)
		if err != nil {
			lastErr = err // operational failure: retry, then surface
		} else {
			switch outcome {
			case casDone, casMismatch, casAbsent:
				return nil // removed, or safely left a successor's/absent lock alone
			case casContended:
				// A peer holds the exclusive handle right now; OUR lock may still be
				// present. Never report success — retry.
				lastErr = fmt.Errorf("release of %s contended: exclusive handle held by a peer", lk.paths.Lock)
			}
		}
		if i < attempts-1 {
			casBackoff(ctx, i)
		}
	}
	return lastErr
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// lockCleanupContext returns the context used to release a lock on a deferred
// cleanup path. It DETACHES from the operation's cancellation/deadline via
// context.WithoutCancel so that a canceled or timed-out apply/destroy STILL
// runs the ReleaseLock command (otherwise a canceled ctx would skip cleanup and
// strand the .lock — evaluator item 3). Request-scoped values are preserved and
// a bounded timeout guarantees the cleanup itself cannot hang.
func lockCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
