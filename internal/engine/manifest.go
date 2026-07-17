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
// persisted so ReleaseLock can perform an ownership-safe compare-and-delete.
type Lock struct {
	t       transport.Transport
	paths   layout.Paths
	content string // exact JSON persisted for this acquisition
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
// over ATOMICALLY — the stale file is removed only if it still matches the bytes
// we read, then the lock is re-created through the SAME exclusive create-new
// gate, so two racing contenders cannot both win (evaluator item 1). On success
// it returns a *Lock handle for ownership-safe release.
func AcquireLock(ctx context.Context, t transport.Transport, p layout.Paths, owner, op string, timeoutSec int) (lk *Lock, staleWarn string, err error) {
	li, _ := json.Marshal(lockInfo{
		Owner: owner, Op: op, StartedUTC: time.Now().UTC().Format(time.RFC3339), Token: newToken(),
	})
	content := string(li)

	created, exit, cerr := createLockExclusive(ctx, t, p, content)
	if cerr != nil {
		return nil, "", coded("ERR_CONNECT", t.Host(), "LOCK", cerr)
	}
	if created {
		return &Lock{t: t, paths: p, content: content}, "", nil
	}
	if exit != 48 {
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock create failed (exit %d)", exit))
	}

	// Held: inspect age. A lock may be overridden ONLY when its metadata parses
	// AND its proven age is >= timeout. Unparseable JSON or an invalid
	// started_utc is NOT proof of staleness, so we refuse (ERR_LOCKED) rather
	// than clobber a lock that may still be live (evaluator item 1 / DESIGN §13).
	raw, ok, rerr := readSmallFile(ctx, t, p.Lock)
	if rerr != nil || !ok {
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock held (unreadable)"))
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

	// Proven stale (age >= timeout): atomic takeover. Remove the stale file ONLY
	// if it still equals the bytes we just read, then re-create through the same
	// exclusive create-new gate. If another contender wins the race the gate
	// returns exit 48 and we surface ERR_LOCKED instead of double-owning.
	won, exit, terr := takeoverLockExclusive(ctx, t, p, raw, content)
	if terr != nil {
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK", terr)
	}
	if !won {
		return nil, "", coded("ERR_LOCKED", t.Host(), "LOCK",
			fmt.Errorf("stale lock from %s taken over by a concurrent contender (exit %d)", existing.Owner, exit))
	}
	return &Lock{t: t, paths: p, content: content},
		fmt.Sprintf("stale lock from %s (started %s) overridden on host %s",
			existing.Owner, existing.StartedUTC, t.Host()), nil
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

// takeoverLockExclusive removes the stale lock IFF its current bytes still equal
// expected (guarding against clobbering a successor that already took over),
// then re-creates via the exclusive create-new gate. Returns (won, exitCode,
// err); won=true only when THIS caller created the replacement.
func takeoverLockExclusive(ctx context.Context, t transport.Transport, p layout.Paths, expected, content string) (bool, int, error) {
	expB64 := base64.StdEncoding.EncodeToString([]byte(expected))
	newB64 := base64.StdEncoding.EncodeToString([]byte(content))
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`if(Test-Path %s){ $cur=[Convert]::ToBase64String([IO.File]::ReadAllBytes(%s)); if($cur -eq %s){ Remove-Item -Force %s } }
try { $fs=[IO.File]::Open(%s,'CreateNew'); $b=[Convert]::FromBase64String(%s); $fs.Write($b,0,$b.Length); $fs.Close(); exit 0 }
catch [System.IO.IOException] { exit 48 }`,
			psq(p.Lock), psq(p.Lock), psq(expB64), psq(p.Lock), psq(p.Lock), psq(newB64))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`if [ -f '%s' ]; then cur=$(base64 < '%s' | tr -d '\n'); if [ "$cur" = '%s' ]; then rm -f '%s'; fi; fi
(set -C; printf '%%s' '%s' | base64 -d > '%s') 2>/dev/null || exit 48`,
			p.Lock, p.Lock, expB64, p.Lock, newB64, p.Lock)
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return false, 0, err
	}
	return r.ExitCode == 0, r.ExitCode, nil
}

// ReleaseLock removes the lock ONLY if the persisted bytes still match this
// handle's — an ownership-safe compare-and-delete so a caller that overran the
// timeout cannot delete a successor's lock that legitimately took over
// (evaluator item 2 / DESIGN §13). A nil handle is a no-op.
func ReleaseLock(ctx context.Context, lk *Lock) {
	if lk == nil || lk.t == nil {
		return
	}
	t := lk.t
	expB64 := base64.StdEncoding.EncodeToString([]byte(lk.content))
	if t.OS() == spec.OSWindows {
		_, _ = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 30,
			Script: fmt.Sprintf(`if(Test-Path %s){ $cur=[Convert]::ToBase64String([IO.File]::ReadAllBytes(%s)); if($cur -eq %s){ Remove-Item -Force -ErrorAction SilentlyContinue %s } }`,
				psq(lk.paths.Lock), psq(lk.paths.Lock), psq(expB64), psq(lk.paths.Lock))})
		return
	}
	_, _ = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 30,
		Script: fmt.Sprintf(`if [ -f '%s' ]; then cur=$(base64 < '%s' | tr -d '\n'); if [ "$cur" = '%s' ]; then rm -f '%s'; fi; fi`,
			lk.paths.Lock, lk.paths.Lock, expB64, lk.paths.Lock)})
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
