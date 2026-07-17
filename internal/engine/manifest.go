package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	script := fmt.Sprintf(`[ -f '%s' ] || exit 3; base64 < '%s'`, path, path)
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
	script := fmt.Sprintf(`printf '%%s' '%s' | base64 -d > '%s'`, b64, path)
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
// This design removes the gate. Every mutation of a HELD `.lock` (stale takeover
// and ownership-safe release) runs as a SINGLE remote command that holds an
// EXCLUSIVE OS file handle for the command's entire lifetime and performs the
// compare-and-swap / compare-and-delete atomically before releasing the handle.
// The handle is a true process-lifetime primitive: the OS drops it when the
// command process exits — normally OR on crash/kill — so nothing can be stranded
// and no separate recovery path (with its unavoidable TOCTOU) is ever needed.
// Windows uses `[IO.File]::Open(..., FileShare.None/Delete)`; POSIX uses
// `flock -n` on the lock fd. Concurrent mutators serialize on that handle, so
// exactly one compare-and-swap can win — the canonical `.lock` is never emptied
// mid-takeover and two operations can never both believe they own it.

// AcquireLock takes the target lock with atomic create-new semantics
// (DESIGN §13). exit 48 ⇒ held. A held lock is inspected: unparseable metadata
// or age < timeout ⇒ ERR_LOCKED (fail fast, no wait); a PROVEN stale lock
// (age >= timeout) is taken over with a SINGLE atomic compare-and-swap that
// overwrites `.lock` in place only while it still holds the exact stale bytes we
// judged, so the canonical path is NEVER momentarily absent and two operations
// can never both believe they hold it. On success it returns a *Lock handle for
// ownership-safe release.
//
// The loop retries ONLY the transient, non-contention states — the holder
// released between our create and our read (lock vanished) or another contender
// won the compare-and-swap for the SAME stale bytes (our CAS reported a mismatch)
// — and re-evaluates from scratch. A genuinely LIVE held lock is judged not-stale
// on the very first pass and returns ERR_LOCKED immediately: no spinning, a
// handful of commands at most (DESIGN §13 "<5s, no wait").
func AcquireLock(ctx context.Context, t transport.Transport, p layout.Paths, owner, op string, timeoutSec int) (lk *Lock, staleWarn string, err error) {
	// Small bound: each retry either wins the create-new, wins the CAS, or reads a
	// now-FRESH lock and returns ERR_LOCKED. Only a repeatedly-vanishing/-contended
	// slot spins, which cannot progress, so a tiny cap keeps us well under 5s.
	const maxAttempts = 8
	for attempt := 0; ; attempt++ {
		token := newToken()
		li, _ := json.Marshal(lockInfo{
			Owner: owner, Op: op, StartedUTC: time.Now().UTC().Format(time.RFC3339), Token: token,
		})
		content := string(li)

		created, exit, cerr := createLockExclusive(ctx, t, p, content)
		if cerr != nil {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", cerr)
		}
		if created {
			return &Lock{t: t, paths: p, content: content, token: token}, "", nil
		}
		if exit != 48 {
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock create failed (exit %d)", exit))
		}

		// Held: inspect age. A lock may be overridden ONLY when its metadata
		// parses AND its proven age is >= timeout. Unparseable JSON or an invalid
		// started_utc is NOT proof of staleness, so we refuse (ERR_LOCKED) rather
		// than clobber a lock that may still be live (DESIGN §13).
		raw, ok, rerr := readSmallFile(ctx, t, p.Lock)
		if rerr != nil {
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock held (unreadable): %v", rerr))
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

		// Proven stale (age >= timeout): take it over with a SINGLE atomic
		// compare-and-swap. casSwap holds an exclusive OS handle for the whole
		// command and overwrites `.lock` in place ONLY while it still holds the
		// exact stale bytes we just read — the canonical path is never emptied, so
		// a concurrent fresh CreateNew always observes exit 48 and no third
		// contender can slip into an empty slot. Exactly one CAS can win.
		swapped, serr := casSwap(ctx, t, p.Lock, raw, content)
		if serr != nil {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", serr)
		}
		if swapped {
			return &Lock{t: t, paths: p, content: content, token: token},
				staleWarnMsg(existing, t.Host()), nil
		}
		// Another contender changed/released `.lock` under us (CAS mismatch) — the
		// slot now holds either a fresh successor or nothing. Re-evaluate: the next
		// pass reads the fresh lock and returns ERR_LOCKED, or races the free slot.
		if attempt < maxAttempts {
			continue
		}
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
			fmt.Errorf("stale lock from %s: takeover repeatedly contended", existing.Owner))
	}
}

// casSwap performs an atomic compare-and-swap on a HELD `.lock`: in a SINGLE
// remote command it opens the file with an EXCLUSIVE OS handle, and only if the
// current bytes equal `expect` does it overwrite them with `content`, all before
// the handle is released. Because the exclusive handle is held for the command's
// whole lifetime and dropped by the OS when the command exits (normally or on
// crash), concurrent takeovers serialize on it and exactly one can win — the
// canonical path is never emptied. Exit map: 0 ⇒ swapped; 10 ⇒ current bytes no
// longer equal `expect` (a peer changed/released it); 48 ⇒ absent or momentarily
// locked by a peer's CAS; anything else ⇒ error. (10/48 ⇒ swapped=false, no error
// — the caller re-evaluates.)
func casSwap(ctx context.Context, t transport.Transport, path, expect, content string) (bool, error) {
	expectB64 := base64.StdEncoding.EncodeToString([]byte(expect))
	newB64 := base64.StdEncoding.EncodeToString([]byte(content))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`try{$fs=[IO.File]::Open(%s,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)}catch{exit 48}
$rc=99
try{
$len=[int]$fs.Length; $b=New-Object byte[] $len; [void]$fs.Read($b,0,$len)
$cur=[Convert]::ToBase64String($b)
if($cur -eq %s){ $nb=[Convert]::FromBase64String(%s); $fs.SetLength(0); $fs.Position=0; $fs.Write($nb,0,$nb.Length); $rc=0 } else { $rc=10 }
} finally { $fs.Close() }
exit $rc`, psq(path), psq(expectB64), psq(newB64))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`exec 9<'%s' 2>/dev/null || exit 48
flock -n 9 || exit 48
cur=$(base64 < '%s' | tr -d '\n')
if [ "$cur" != '%s' ]; then exit 10; fi
printf '%%s' '%s' | base64 -d > '%s'
exit 0`, path, path, expectB64, newB64, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, err
	}
	switch r.ExitCode {
	case 0:
		return true, nil
	case 10, 48:
		return false, nil
	default:
		return false, fmt.Errorf("compare-and-swap of %s failed (exit %d): %s", path, r.ExitCode, r.Stderr)
	}
}

// casDelete performs an atomic compare-and-delete on a HELD `.lock`: in a SINGLE
// remote command it opens the file with an EXCLUSIVE OS handle and removes it
// ONLY if the current bytes equal `expect` (this handle's owned content). A
// successor that legitimately took over after our timeout has DIFFERENT bytes, so
// its lock is never removed. Exit map: 0 ⇒ deleted (we owned it); 10 ⇒ not ours
// (no-op); 48 ⇒ already absent or momentarily locked by a peer (no-op); anything
// else ⇒ the removal itself FAILED and is surfaced as an error so the caller can
// retry/log it rather than silently strand the lock.
func casDelete(ctx context.Context, t transport.Transport, path, expect string) (bool, error) {
	expectB64 := base64.StdEncoding.EncodeToString([]byte(expect))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`try{$fs=[IO.File]::Open(%s,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::Delete)}catch{exit 48}
$rc=99
try{
$len=[int]$fs.Length; $b=New-Object byte[] $len; [void]$fs.Read($b,0,$len)
$cur=[Convert]::ToBase64String($b)
if($cur -eq %s){ [IO.File]::Delete(%s); $rc=0 } else { $rc=10 }
} finally { $fs.Close() }
if($rc -eq 0 -and (Test-Path -LiteralPath %s)){ exit 17 }
exit $rc`, psq(path), psq(expectB64), psq(path), psq(path))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 30})
	} else {
		script := fmt.Sprintf(`exec 9<'%s' 2>/dev/null || exit 48
flock -n 9 || exit 48
cur=$(base64 < '%s' | tr -d '\n')
if [ "$cur" != '%s' ]; then exit 10; fi
rm -f '%s'
if [ -e '%s' ]; then exit 17; fi
exit 0`, path, path, expectB64, path, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 30})
	}
	if err != nil {
		return false, err
	}
	switch r.ExitCode {
	case 0:
		return true, nil
	case 10, 48:
		return false, nil
	default:
		return false, fmt.Errorf("compare-and-delete of %s failed (exit %d): %s", path, r.ExitCode, r.Stderr)
	}
}

func staleWarnMsg(existing lockInfo, host string) string {
	return fmt.Sprintf("stale lock from %s (started %s) overridden on host %s",
		existing.Owner, existing.StartedUTC, host)
}

// createLockExclusive runs the atomic create-new on the canonical lock path.
func createLockExclusive(ctx context.Context, t transport.Transport, p layout.Paths, content string) (bool, int, error) {
	return createExclusiveAt(ctx, t, p.Root, p.Lock, content)
}

// createExclusiveAt runs the atomic create-new on the canonical lock path.
// Returns (created, exitCode, transportErr): created=true on exit 0;
// exitCode==48 signals the file already existed. DESIGN §13: PowerShell
// `[IO.File]::Open(CreateNew)`, POSIX `set -C`.
func createExclusiveAt(ctx context.Context, t transport.Transport, root, path, content string) (bool, int, error) {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null
try { $fs=[IO.File]::Open(%s,'CreateNew'); $b=[Convert]::FromBase64String(%s); $fs.Write($b,0,$b.Length); $fs.Close(); exit 0 }
catch [System.IO.IOException] { exit 48 }`, psq(root), psq(path), psq(b64))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`mkdir -p '%s'; (set -C; printf '%%s' '%s' | base64 -d > '%s') 2>/dev/null || exit 48`, root, b64, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, 0, err
	}
	return r.ExitCode == 0, r.ExitCode, nil
}

// ReleaseLock releases the lock with a SINGLE atomic compare-and-delete
// (casDelete): in one exclusive-handle command it removes `.lock` ONLY when the
// persisted bytes still equal this handle's exact content. A caller that overran
// the timeout can therefore never delete a successor's lock — the successor's
// bytes differ, so casDelete is a safe no-op. Because the check and the removal
// happen inside one command that holds the file's exclusive OS handle, no
// takeover can interleave between "verify" and "delete", and there is no
// persistent gate that could be stranded by a crash. A nil handle, an already
// absent/foreign lock, or a momentary peer lock is a safe no-op. A removal that
// actually FAILS (permissions/IO) is retried a few times and, if still failing,
// surfaced to the caller so it is logged rather than silently stranding the lock
// (evaluator item 3).
func ReleaseLock(ctx context.Context, lk *Lock) error {
	if lk == nil || lk.t == nil {
		return nil
	}
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		_, err := casDelete(ctx, lk.t, lk.paths.Lock, lk.content)
		if err == nil {
			return nil // deleted, or safely left a successor's/absent lock alone
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(time.Duration(i+1) * 5 * time.Millisecond):
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
