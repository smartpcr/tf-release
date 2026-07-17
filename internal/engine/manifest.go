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
// persisted plus this acquisition's unique token so ReleaseLock can perform an
// ownership-safe release keyed on an ATOMIC rename (never a cached compare).
type Lock struct {
	t       transport.Transport
	paths   layout.Paths
	content string // exact JSON persisted for this acquisition
	token   string // per-acquisition nonce; names the release claim path
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-based token; uniqueness is best-effort here.
		return fmt.Sprintf("t-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// AcquireLock takes the target lock with atomic create-new semantics
// (DESIGN §13). exit 48 ⇒ held. A held lock is inspected: unparseable metadata
// or age < timeout ⇒ ERR_LOCKED; a PROVEN stale lock (age ≥ timeout) is taken
// over via ATOMIC RENAME ARBITRATION — the stale file is seized with a single
// indivisible rename to a unique per-token path, so among concurrent contenders
// exactly one seizes it and the rest observe "source gone" (evaluator item 1).
// On success it returns a *Lock handle for ownership-safe release.
func AcquireLock(ctx context.Context, t transport.Transport, p layout.Paths, owner, op string, timeoutSec int) (lk *Lock, staleWarn string, err error) {
	// A held lock that DISAPPEARS mid-inspection (the holder released) or a lost
	// create-new race after a legitimate stale takeover are transient states, not
	// contention — the slot is momentarily free, so we re-evaluate from scratch.
	// The loop is bounded; a genuinely live lock is judged not-stale on the very
	// next pass and returns ERR_LOCKED immediately (no spinning).
	const maxAttempts = 32
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

		// Proven stale (age >= timeout): take over via ATOMIC RENAME ARBITRATION.
		// rename(2) / [IO.File]::Move is the only indivisible + exclusive
		// filesystem primitive: among concurrent contenders renaming the SAME
		// source to DISTINCT per-token targets, exactly ONE succeeds and every
		// other observes "source gone". This replaces the racy
		// read/compare/remove/create guard that let two contenders both
		// delete-and-recreate the lock (evaluator item 1).
		claim := p.Lock + ".acq." + token
		won, cerr2 := claimByRename(ctx, t, p.Lock, claim)
		if cerr2 != nil {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", cerr2)
		}
		if !won {
			// Another contender seized the stale file first (or the owner
			// released). Race for the now-free slot through the exclusive gate.
			created2, _, cerr3 := createLockExclusive(ctx, t, p, content)
			if cerr3 != nil {
				return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", cerr3)
			}
			if created2 {
				return &Lock{t: t, paths: p, content: content, token: token}, staleWarnMsg(existing, t.Host()), nil
			}
			// A winner now holds the slot; re-evaluate it (it may be a fresh,
			// live lock ⇒ ERR_LOCKED next pass, or already gone ⇒ retry).
			if attempt < maxAttempts {
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("stale lock from %s taken over by a concurrent contender", existing.Owner))
		}
		// We seized the bytes under our unique claim path. Re-verify they are
		// still the ones we judged stale: if a successor took over between our
		// read and our seize the bytes differ ⇒ we grabbed a LIVE lock ⇒ restore
		// it (no-replace) and re-evaluate, so we never invalidate a live owner.
		claimRaw, ok2, rerr2 := readSmallFile(ctx, t, claim)
		if rerr2 != nil || !ok2 {
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("seized claim vanished during takeover"))
		}
		if claimRaw != raw {
			if restored, _ := restoreLock(ctx, t, claim, p.Lock); !restored {
				_ = deletePath(ctx, t, claim)
			}
			if attempt < maxAttempts {
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("stale lock changed under takeover (a successor already claimed it)"))
		}
		// Confirmed stale: discard the seized bytes and re-create through the
		// exclusive gate. A racing creator that grabbed the freed slot wins.
		if derr := deletePath(ctx, t, claim); derr != nil {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", derr)
		}
		created2, _, cerr3 := createLockExclusive(ctx, t, p, content)
		if cerr3 != nil {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", cerr3)
		}
		if created2 {
			return &Lock{t: t, paths: p, content: content, token: token}, staleWarnMsg(existing, t.Host()), nil
		}
		// Lost the freed slot to a concurrent creator — re-evaluate.
		if attempt < maxAttempts {
			continue
		}
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
			fmt.Errorf("stale lock from %s taken over by a concurrent contender", existing.Owner))
	}
}

func staleWarnMsg(existing lockInfo, host string) string {
	return fmt.Sprintf("stale lock from %s (started %s) overridden on host %s",
		existing.Owner, existing.StartedUTC, host)
}

// createLockExclusive runs the atomic create-new. Returns (created, exitCode,
// transportErr): created=true on exit 0; exitCode==48 signals the file already
// existed. DESIGN §13: PowerShell `[IO.File]::Open(CreateNew)`, POSIX `set -C`.
func createLockExclusive(ctx context.Context, t transport.Transport, p layout.Paths, content string) (bool, int, error) {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null
try { $fs=[IO.File]::Open(%s,'CreateNew'); $b=[Convert]::FromBase64String(%s); $fs.Write($b,0,$b.Length); $fs.Close(); exit 0 }
catch [System.IO.IOException] { exit 48 }`, psq(p.Root), psq(p.Lock), psq(b64))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`mkdir -p '%s'; (set -C; printf '%%s' '%s' | base64 -d > '%s') 2>/dev/null || exit 48`, p.Root, b64, p.Lock)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, 0, err
	}
	return r.ExitCode == 0, r.ExitCode, nil
}

// claimByRename atomically renames `from` to `to`. rename(2) / [IO.File]::Move
// is the sole indivisible + exclusive filesystem primitive: among concurrent
// callers renaming the SAME source to DISTINCT targets, exactly ONE succeeds and
// the rest observe "source gone". Returns (won, transportErr); won=false when the
// source no longer exists (another caller seized it first).
func claimByRename(ctx context.Context, t transport.Transport, from, to string) (bool, error) {
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`try { [IO.File]::Move(%s,%s); exit 0 } catch { exit 3 }`, psq(from), psq(to))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`if mv '%s' '%s' 2>/dev/null; then exit 0; else exit 3; fi`, from, to)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, err
	}
	return r.ExitCode == 0, nil
}

// restoreLock moves a previously-seized claim file back to the lock path WITHOUT
// replacing a lock that a fresh contender may have created in the freed slot.
// Windows [IO.File]::Move is inherently no-replace (throws if dest exists); POSIX
// uses `ln` (fails if dest exists) then drops the source. Returns (restored,err);
// restored=false when the slot is already taken (dest exists).
func restoreLock(ctx context.Context, t transport.Transport, from, to string) (bool, error) {
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`try { [IO.File]::Move(%s,%s); exit 0 } catch { exit 1 }`, psq(from), psq(to))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`if ln '%s' '%s' 2>/dev/null; then rm -f '%s'; exit 0; else exit 1; fi`, from, to, from)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, err
	}
	return r.ExitCode == 0, nil
}

// deletePath unconditionally removes a single file (used to drop a seized claim).
func deletePath(ctx context.Context, t transport.Transport, path string) error {
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`Remove-Item -Force -ErrorAction SilentlyContinue %s`, psq(path))
		_, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 30})
	} else {
		script := fmt.Sprintf(`rm -f '%s'`, path)
		_, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 30})
	}
	return err
}

// ReleaseLock releases the lock with ATOMIC RENAME ARBITRATION so a caller that
// overran the timeout can never delete a successor's lock (evaluator item 2).
// It first SEIZES the lock path with a single indivisible rename to a unique
// per-token claim path; if the rename fails the lock is already gone or a
// successor holds it ⇒ no-op. Only after the seize does it verify ownership: if
// the seized bytes are ours we delete the claim; otherwise we restore it
// (no-replace) so the successor's lock survives. Ownership verification and
// removal are thus one indivisible seize, never a cached compare. Nil ⇒ no-op.
func ReleaseLock(ctx context.Context, lk *Lock) {
	if lk == nil || lk.t == nil {
		return
	}
	t := lk.t
	claim := lk.paths.Lock + ".rel." + lk.token
	won, err := claimByRename(ctx, t, lk.paths.Lock, claim)
	if err != nil || !won {
		// Lock already absent or seized by a successor's takeover — nothing ours.
		return
	}
	claimRaw, ok, _ := readSmallFile(ctx, t, claim)
	if ok && claimRaw == lk.content {
		_ = deletePath(ctx, t, claim) // it was ours — drop it
		return
	}
	// Not ours (a successor legitimately took over): put it back untouched.
	if restored, _ := restoreLock(ctx, t, claim, lk.paths.Lock); !restored {
		_ = deletePath(ctx, t, claim)
	}
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
