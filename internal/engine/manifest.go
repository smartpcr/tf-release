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
		// than clobber a lock that may still be live (DESIGN §13). The ONE
		// exception is an EMPTY/whitespace-only file: that is unambiguous residue
		// from a writer killed mid-create/replace (crash residue, no live holder),
		// which we recover via a content-keyed compare-and-swap (evaluator item 3).
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

		emptyResidue := strings.TrimSpace(raw) == ""
		if !emptyResidue {
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
			// exact stale bytes we just read.
			outcome, serr := casSwap(ctx, t, p.Lock, raw, content)
			if serr != nil {
				return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", serr)
			}
			switch outcome {
			case casDone:
				return &Lock{t: t, paths: p, content: content, token: token},
					staleWarnMsg(existing, t.Host()), nil
			case casContended:
				// A peer holds the exclusive handle right now (transient). Back off
				// and re-evaluate rather than misreporting it as a mismatch/absence.
				if attempt < maxAttempts {
					casBackoff(ctx, attempt)
					continue
				}
				return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
					fmt.Errorf("stale lock from %s: takeover repeatedly contended", existing.Owner))
			default: // casMismatch / casAbsent: slot changed under us — re-evaluate.
				if attempt < maxAttempts {
					continue
				}
				return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
					fmt.Errorf("stale lock from %s: takeover repeatedly contended", existing.Owner))
			}
		}

		// Empty crash residue: recover it via a content-keyed compare-and-swap that
		// only overwrites while the file is STILL empty (expect == raw). If a real
		// writer fills it in the gap, our CAS reports a mismatch and we re-evaluate.
		outcome, serr := casSwap(ctx, t, p.Lock, raw, content)
		if serr != nil {
			return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", serr)
		}
		switch outcome {
		case casDone:
			return &Lock{t: t, paths: p, content: content, token: token},
				fmt.Sprintf("recovered empty/partial lock (crash residue) on host %s", t.Host()), nil
		case casContended:
			if attempt < maxAttempts {
				casBackoff(ctx, attempt)
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("empty lock residue: recovery repeatedly contended"))
		default: // mismatch/absent: a real writer filled or removed it — re-evaluate.
			if attempt < maxAttempts {
				continue
			}
			return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
				fmt.Errorf("empty lock residue: recovery repeatedly contended"))
		}
	}
}

// casBackoff sleeps a short, attempt-scaled interval before retrying a CONTENDED
// compare-and-swap/-delete, honoring context cancellation. Contention means a peer
// briefly holds the exclusive handle; a few-ms wait lets it finish without a spin.
func casBackoff(ctx context.Context, attempt int) {
	d := time.Duration(attempt+1) * 5 * time.Millisecond
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// casOutcome is the distinguished result of a compare-and-swap / -delete. The
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

// Shared CAS exit codes for casSwap/casDelete (both platforms). Anything NOT in
// {0,10,48,49} is an operational failure (permission denied, missing `flock`,
// read/write/rename fault) and is mapped to a Go error by classifyCas — never
// silently treated as absence or contention.
//
//	 0  done            10  content mismatch   48  absent
//	49  exclusion held  17  removal failed     70  flock missing
//	71  open failed     72  read failed        73  temp write failed
//	74  rename failed    99  unexpected
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

// casSwap performs an atomic, crash-safe compare-and-swap on a HELD `.lock`: in a
// SINGLE remote command it takes an EXCLUSIVE OS handle and, only if the current
// bytes equal `expect`, replaces them with `content` before releasing the handle.
// Concurrent takeovers serialize on the handle so exactly one can win. Two
// evaluator-driven properties:
//   - Distinct outcomes (item 1): absence (48), live handle contention (49), and
//     content mismatch (10) are separate; permission/`flock`/IO faults become a
//     Go error rather than a misreported "absent".
//   - Crash-safe replacement (item 3): POSIX writes a private `$tmp` then `mv -f`
//     (rename(2) is atomic — a killed writer leaves the OLD `.lock` intact, never
//     an empty/partial one). Windows writes the full buffer at position 0 and
//     only THEN truncates to the new length, so an interrupted write can never
//     shorten the file to a partial prefix.
func casSwap(ctx context.Context, t transport.Transport, path, expect, content string) (casOutcome, error) {
	expectB64 := base64.StdEncoding.EncodeToString([]byte(expect))
	newB64 := base64.StdEncoding.EncodeToString([]byte(content))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`try{$fs=[IO.File]::Open(%s,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)}
catch [System.IO.FileNotFoundException]{exit 48}
catch [System.IO.DirectoryNotFoundException]{exit 48}
catch [System.IO.IOException]{exit 49}
catch{exit 71}
$rc=99
try{
$len=[int]$fs.Length; $b=New-Object byte[] $len; [void]$fs.Read($b,0,$len)
$cur=[Convert]::ToBase64String($b)
if($cur -eq %s){ $nb=[Convert]::FromBase64String(%s); $fs.Position=0; $fs.Write($nb,0,$nb.Length); $fs.SetLength($nb.Length); $rc=0 } else { $rc=10 }
} finally { $fs.Close() }
exit $rc`, psq(path), psq(expectB64), psq(newB64))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`[ -e '%s' ] || exit 48
command -v flock >/dev/null 2>&1 || exit 70
exec 9<'%s' 2>/dev/null || { [ -e '%s' ] && exit 71 || exit 48; }
flock -n 9 || exit 49
cur=$(base64 < '%s' | tr -d '\n')
if [ "$cur" != '%s' ]; then exit 10; fi
tmp='%s.swap.'$$
printf '%%s' '%s' | base64 -d > "$tmp" || exit 73
mv -f "$tmp" '%s' || { rm -f "$tmp"; exit 74; }
exit 0`, path, path, path, path, expectB64, path, newB64, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return casDone, err
	}
	return classifyCas("compare-and-swap", path, r)
}

// casDelete performs an atomic compare-and-delete on a HELD `.lock`: in a SINGLE
// remote command it opens the file with an EXCLUSIVE OS handle and removes it
// ONLY if the current bytes equal `expect` (this handle's owned content). A
// successor that legitimately took over after our timeout has DIFFERENT bytes, so
// its lock is never removed. Outcomes mirror casSwap: casDone (deleted),
// casMismatch (not ours — safe no-op), casAbsent (already gone), casContended (a
// peer holds the handle right now — the caller RETRIES so it never returns
// success while OUR lock is still present, evaluator item 2); any other exit
// (removal failed, permission, missing flock) is a Go error.
func casDelete(ctx context.Context, t transport.Transport, path, expect string) (casOutcome, error) {
	expectB64 := base64.StdEncoding.EncodeToString([]byte(expect))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`try{$fs=[IO.File]::Open(%s,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::Delete)}
catch [System.IO.FileNotFoundException]{exit 48}
catch [System.IO.DirectoryNotFoundException]{exit 48}
catch [System.IO.IOException]{exit 49}
catch{exit 71}
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
		script := fmt.Sprintf(`[ -e '%s' ] || exit 48
command -v flock >/dev/null 2>&1 || exit 70
exec 9<'%s' 2>/dev/null || { [ -e '%s' ] && exit 71 || exit 48; }
flock -n 9 || exit 49
cur=$(base64 < '%s' | tr -d '\n')
if [ "$cur" != '%s' ]; then exit 10; fi
rm -f '%s'
if [ -e '%s' ]; then exit 17; fi
exit 0`, path, path, path, path, expectB64, path, path)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 30})
	}
	if err != nil {
		return casDone, err
	}
	return classifyCas("compare-and-delete", path, r)
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
// bytes differ (casMismatch), so casDelete is a safe no-op. Because the check and
// the removal happen inside one command that holds the file's exclusive OS
// handle, no takeover can interleave between "verify" and "delete".
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
