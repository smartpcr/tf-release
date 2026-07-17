package engine

import (
	"context"
	"encoding/base64"
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

// ------------------------------- locking (DESIGN §13) ----------------------

type lockInfo struct {
	Owner      string `json:"owner"`
	Op         string `json:"op"`
	StartedUTC string `json:"started_utc"`
}

// AcquireLock performs an atomic create-new; exit 48 signals held. Stale locks (age ≥
// timeout) are overridden with staleWarn=true.
func AcquireLock(ctx context.Context, t transport.Transport, p layout.Paths, owner, op string, timeoutSec int) (staleWarn string, err error) {
	li, _ := json.Marshal(lockInfo{Owner: owner, Op: op, StartedUTC: time.Now().UTC().Format(time.RFC3339)})
	b64 := base64.StdEncoding.EncodeToString(li)
	var r transport.Result
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null
try { $fs=[IO.File]::Open(%s,'CreateNew'); $b=[Convert]::FromBase64String(%s); $fs.Write($b,0,$b.Length); $fs.Close(); exit 0 }
catch [System.IO.IOException] { exit 48 }`, psq(p.Root), psq(p.Lock), psq(b64))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, TimeoutSec: 60})
	} else {
		script := fmt.Sprintf(`mkdir -p %s; (set -C; printf '%%s' '%s' | base64 -d > %s) 2>/dev/null || exit 48`, shq(p.Root), b64, shq(p.Lock))
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, TimeoutSec: 60})
	}
	if err != nil {
		return "", coded("ERR_CONNECT", t.Host(), "LOCK", err)
	}
	if r.ExitCode == 0 {
		return "", nil
	}
	if r.ExitCode != 48 {
		return "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock create failed: %s", r.Stderr))
	}
	// Held: inspect age.
	raw, ok, rerr := readSmallFile(ctx, t, p.Lock)
	if rerr != nil || !ok {
		return "", coded("ERR_LOCKED", t.Host(), "LOCK", fmt.Errorf("lock held (unreadable)"))
	}
	var existing lockInfo
	_ = json.Unmarshal([]byte(raw), &existing)
	started, perr := time.Parse(time.RFC3339, existing.StartedUTC)
	if perr == nil && time.Since(started) < time.Duration(timeoutSec)*time.Second {
		return "", coded("ERR_LOCKED", t.Host(), "LOCK",
			fmt.Errorf("held by %s since %s (op=%s)", existing.Owner, existing.StartedUTC, existing.Op))
	}
	// stale: overwrite
	if werr := writeSmallFile(ctx, t, p.Lock, string(li)); werr != nil {
		return "", coded("ERR_LOCKED", t.Host(), "LOCK", werr)
	}
	return fmt.Sprintf("stale lock from %s (started %s) overridden on host %s",
		existing.Owner, existing.StartedUTC, t.Host()), nil
}

func ReleaseLock(ctx context.Context, t transport.Transport, p layout.Paths) {
	if t.OS() == spec.OSWindows {
		_, _ = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell,
			Script: fmt.Sprintf(`Remove-Item -Force -ErrorAction SilentlyContinue %s`, psq(p.Lock)), TimeoutSec: 30})
		return
	}
	_, _ = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh,
		Script: fmt.Sprintf(`rm -f %s`, shq(p.Lock)), TimeoutSec: 30})
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
