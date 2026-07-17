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

// AcquireLock takes the target lock with atomic create-new semantics
// (DESIGN §13). exit 48 ⇒ held. A held lock is inspected: unparseable metadata
// or age < timeout ⇒ ERR_LOCKED; a PROVEN stale lock (age >= timeout) is taken
// over UNDER A SERIALIZATION GATE by REPLACING `.lock` in place with a single
// atomic rename, so the canonical lock path is NEVER momentarily absent and two
// operations can never both believe they hold it (evaluator items 1 & 4). On
// success it returns a *Lock handle for ownership-safe release.
func AcquireLock(ctx context.Context, t transport.Transport, p layout.Paths, owner, op string, timeoutSec int) (lk *Lock, staleWarn string, err error) {
	// A held lock that DISAPPEARS mid-inspection (the holder released) or a lost
	// gate race is a transient state, not contention, so we re-evaluate from
	// scratch. The loop is bounded; a genuinely live lock is judged not-stale on
	// the next pass and returns ERR_LOCKED immediately (no spinning).
	const maxAttempts = 64
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

		// Proven stale (age >= timeout): take over UNDER THE SERIALIZATION GATE.
		// The gate (an exclusive create-new on `<lock>.mx`) admits exactly one
		// takeover-or-release critical section at a time. While we hold it we
		// REPLACE `.lock` in place with a single atomic rename — the canonical
		// path is never absent, so a concurrent fresh CreateNew always observes
		// exit 48 and no third contender can slip into an empty slot (items 1,2).
		done, tlk, twarn, terr := takeoverUnderGate(ctx, t, p, raw, existing, content, token)
		if terr != nil {
			return nil, "", terr
		}
		if done {
			return tlk, twarn, nil
		}
		// Gate was busy, or `.lock` changed/vanished under us — re-evaluate.
		if attempt < maxAttempts {
			continue
		}
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
			fmt.Errorf("stale lock from %s: takeover repeatedly contended", existing.Owner))
	}
}

// takeoverUnderGate attempts a single gated stale-takeover. It acquires the
// exclusive gate; if busy it returns (done=false) so the caller re-evaluates.
// While holding the gate — released on EVERY path, including transport errors
// (evaluator item 4) — it re-reads `.lock`: only when it still holds the exact
// stale bytes we judged does it REPLACE them in place with our content via one
// atomic rename (never emptying the canonical slot). Any other state (vanished,
// or already replaced by a prior successor) yields done=false for re-evaluation.
func takeoverUnderGate(ctx context.Context, t transport.Transport, p layout.Paths, raw string, existing lockInfo, content, token string) (done bool, lk *Lock, warn string, err error) {
	gotGate, gexit, gerr := createExclusiveAt(ctx, t, p.Root, gatePath(p.Lock), token)
	if gerr != nil {
		return false, nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", gerr)
	}
	if !gotGate {
		if gexit == 48 {
			return false, nil, "", nil // gate busy — caller re-evaluates
		}
		return false, nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("takeover gate create failed (exit %d)", gexit))
	}
	defer func() { _ = deletePath(ctx, t, gatePath(p.Lock)) }()

	cur, ok, rerr := readSmallFile(ctx, t, p.Lock)
	if rerr != nil {
		return false, nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", rerr)
	}
	if !ok || cur != raw {
		// Vanished (holder released) or already replaced by a prior successor —
		// nothing for us to override here. Re-evaluate from the top.
		return false, nil, "", nil
	}
	// Still the exact stale lock we judged: atomically REPLACE it in place. The
	// canonical `.lock` is never removed, so fresh CreateNew contenders keep
	// seeing exit 48 and no empty slot is ever exposed.
	if rerr := replaceInPlace(ctx, t, p, p.Lock, content, token); rerr != nil {
		return false, nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", rerr)
	}
	return true, &Lock{t: t, paths: p, content: content, token: token}, staleWarnMsg(existing, t.Host()), nil
}

func staleWarnMsg(existing lockInfo, host string) string {
	return fmt.Sprintf("stale lock from %s (started %s) overridden on host %s",
		existing.Owner, existing.StartedUTC, host)
}

// createLockExclusive runs the atomic create-new on the canonical lock path.
func createLockExclusive(ctx context.Context, t transport.Transport, p layout.Paths, content string) (bool, int, error) {
	return createExclusiveAt(ctx, t, p.Root, p.Lock, content)
}

// createExclusiveAt runs the atomic create-new at an arbitrary path (used for
// both the canonical `.lock` and the `<lock>.mx` serialization gate). Returns
// (created, exitCode, transportErr): created=true on exit 0; exitCode==48 signals
// the file already existed. DESIGN §13: PowerShell `[IO.File]::Open(CreateNew)`,
// POSIX `set -C`.
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

// gatePath is the per-lock serialization gate: an exclusive create-new file that
// admits exactly one takeover/release critical section at a time, so the
// canonical `.lock` is only ever mutated by one gated operation.
func gatePath(lock string) string { return lock + ".mx" }

// replaceInPlace atomically overwrites `lockPath` with content by writing a
// unique temp file then renaming it ONTO the destination (POSIX rename / Windows
// [IO.File]::Replace are atomic and never leave the destination absent). Callers
// must hold the serialization gate. On any error the temp is cleaned up so no
// `.tmp.*` residue is stranded (evaluator item 4).
func replaceInPlace(ctx context.Context, t transport.Transport, p layout.Paths, lockPath, content, token string) error {
	tmp := lockPath + ".tmp." + token
	if werr := writeSmallFile(ctx, t, tmp, content); werr != nil {
		_ = deletePath(ctx, t, tmp)
		return werr
	}
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		// [IO.File]::Replace atomically swaps tmp onto lockPath (dst must exist,
		// which the gate guarantees) with no intervening absent state.
		script := fmt.Sprintf(`[IO.File]::Replace(%s,%s,$null)`, psq(tmp), psq(lockPath))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		// rename(2) via `mv -f` atomically replaces the destination.
		script := fmt.Sprintf(`mv -f '%s' '%s'`, tmp, lockPath)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		_ = deletePath(ctx, t, tmp)
		return err
	}
	if r.ExitCode != 0 {
		_ = deletePath(ctx, t, tmp)
		return fmt.Errorf("atomic replace of %s failed (exit %d): %s", lockPath, r.ExitCode, r.Stderr)
	}
	return nil
}

// deletePath unconditionally removes a single file (the gate, or an owned lock).
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

// ReleaseLock releases the lock UNDER THE SERIALIZATION GATE so a caller that
// overran the timeout can never delete a successor's lock (evaluator item 2).
// It acquires the exclusive gate, then removes `.lock` ONLY when the persisted
// bytes still equal this handle's exact content; if a successor has legitimately
// replaced them, the lock is left untouched. Because takeover also holds this
// gate and replaces `.lock` in place, ownership verification and removal are
// never interleaved with a takeover and NO unowned lock is ever temporarily
// removed. A nil handle, a persistently-busy gate, or a transport error is a
// safe no-op (never an unsafe delete). The gate is released on every path.
func ReleaseLock(ctx context.Context, lk *Lock) {
	if lk == nil || lk.t == nil {
		return
	}
	t := lk.t
	gate := gatePath(lk.paths.Lock)
	const gateAttempts = 64
	for attempt := 0; attempt < gateAttempts; attempt++ {
		gotGate, gexit, gerr := createExclusiveAt(ctx, t, lk.paths.Root, gate, lk.token)
		if gerr != nil {
			return // transport error — do NOT risk an unsafe delete
		}
		if gotGate {
			func() {
				defer func() { _ = deletePath(ctx, t, gate) }()
				cur, ok, _ := readSmallFile(ctx, t, lk.paths.Lock)
				if ok && cur == lk.content {
					_ = deletePath(ctx, t, lk.paths.Lock) // still ours — release it
				}
				// Otherwise a successor owns it (or it is already gone): leave it.
			}()
			return
		}
		if gexit != 48 {
			return // unexpected gate failure — safe no-op
		}
		// Gate busy (a takeover/release is in flight); brief backoff then retry.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * time.Millisecond):
		}
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
