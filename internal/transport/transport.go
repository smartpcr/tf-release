// Package transport abstracts remote execution + file transfer (DESIGN §8.1).
package transport

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

type Shell int

const (
	ShellPowerShell Shell = iota
	ShellSh
	ShellCmd
)

type Cmd struct {
	Shell      Shell
	Script     string
	TimeoutSec int
	// Env values are injected inside the encoded script / prefix, never on a
	// visible command line (DESIGN §11 secret hygiene).
	Env map[string]string
}

type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Transport contract: Exec returns error ONLY for transport-level failures;
// application exit codes are surfaced via Result.ExitCode.
type Transport interface {
	Connect(ctx context.Context) error
	Close() error
	OS() spec.OSKind
	Host() string
	Exec(ctx context.Context, c Cmd) (Result, error)
	Upload(ctx context.Context, local io.Reader, size int64, remote string) error
	Download(ctx context.Context, remote, local string) error
}

// CodedError is a coded error (DESIGN §12) — the engine maps these to diagnostics.
type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return fmt.Sprintf("[%s] %v", e.Code, e.Err) }
func (e *CodedError) Unwrap() error { return e.Err }

func ErrConnect(err error) error { return &CodedError{Code: "ERR_CONNECT", Err: err} }
func ErrAuth(err error) error    { return &CodedError{Code: "ERR_AUTH", Err: err} }

// nonRetryable wraps an error that the connect retry loop must surface
// immediately without further attempts (e.g. ERR_AUTH, host-key mismatch).
type nonRetryable struct{ err error }

func (n *nonRetryable) Error() string { return n.err.Error() }
func (n *nonRetryable) Unwrap() error { return n.err }

// noRetry marks err so retryConnect returns it as-is without retrying.
func noRetry(err error) error { return &nonRetryable{err: err} }

// retryConnect runs attempt up to retries+1 times with a FIXED backoff between
// attempts (DESIGN §8.1). The backoff is applied ONLY between attempts — never
// after the final attempt — so an exhausted loop does not waste a trailing
// sleep. An attempt may short-circuit the loop by returning a noRetry-wrapped
// error (e.g. ERR_AUTH), which is returned verbatim. Any other non-nil error is
// retryable; once attempts are exhausted the last one is wrapped in ERR_CONNECT.
// A negative retries (nothing bounds target.connect_retries >= 0 in the spec
// validators) is clamped so the helper always performs at least one real attempt
// rather than returning a garbled "after 0 attempts" error wrapping a nil error.
func retryConnect(ctx context.Context, retries int, backoff time.Duration, attempt func() error) error {
	attempts := retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		err := attempt()
		if err == nil {
			return nil
		}
		var nr *nonRetryable
		if errors.As(err, &nr) {
			return nr.err // ERR_AUTH / fatal: not retried
		}
		lastErr = err
		if i == attempts-1 {
			break // no backoff after the final attempt
		}
		select {
		case <-ctx.Done():
			return ErrConnect(ctx.Err())
		case <-time.After(backoff):
		}
	}
	return ErrConnect(fmt.Errorf("after %d attempts: %w", attempts, lastErr))
}

// NewTransport builds a transport for one host from a merged spec.Target
// (DESIGN §8.1, implementation-plan Stage 2.1 step 4). It dispatches on the
// transport kind (local/ssh/winrm) and is the seam the engine swaps for a
// fake transport in unit tests (see engine.Engine.NewTransport).
// Secrets are resolved here from the runner's environment by NAME.
func NewTransport(t *spec.Target, host string) (Transport, error) {
	password := ""
	if t.Credentials.PasswordEnv != "" {
		password = os.Getenv(t.Credentials.PasswordEnv)
	}
	switch t.Transport {
	case spec.TransportWinRM:
		if t.OS != spec.OSWindows {
			return nil, fmt.Errorf("winrm requires os windows")
		}
		https := t.WinRM.UseHTTPS == nil || *t.WinRM.UseHTTPS
		insecure := t.WinRM.InsecureSkipVerify != nil && *t.WinRM.InsecureSkipVerify
		return newWinRM(host, t.EffectivePort(), t.Credentials.Username, password,
			https, insecure, t.WinRM.TimeoutSeconds,
			t.EffectiveConnectRetries()), nil
	case spec.TransportSSH:
		key := ""
		if t.Credentials.PrivateKeyEnv != "" {
			key = os.Getenv(t.Credentials.PrivateKeyEnv)
		}
		return newSSH(host, t.EffectivePort(), t.Credentials.Username, password, key,
			t.SSH.HostKey, t.SSH.TimeoutSeconds, t.EffectiveConnectRetries(), t.OS), nil
	case spec.TransportLocal:
		return newLocal(t.OS), nil
	default:
		return nil, fmt.Errorf("unknown transport %q", t.Transport)
	}
}

// ---------------------------------------------------------------------------
// Windows PowerShell command construction — identical over WinRM and SSH.
// ---------------------------------------------------------------------------

// psQuote single-quotes for PowerShell (” escapes ').
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// shQuote single-quotes for POSIX sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// withEnvPS prepends `$env:K='V';` lines in sorted key order (deterministic for
// golden tests; DESIGN §8.1/§17 transport). Each assignment is single-quote
// escaped and semicolon-terminated per DESIGN §8.1 line 420.
func withEnvPS(script string, env map[string]string) string {
	if len(env) == 0 {
		return script
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "$env:%s=%s;\n", k, psQuote(env[k]))
	}
	b.WriteString(script)
	return b.String()
}

// shEnvPrefix renders sorted `K='V' ` inline assignments that PRECEDE `sh -c`
// so the values populate the environment of the sh process and the script's
// `$K` expansions observe them (DESIGN §8.1: `sh -c '<script>'` with env
// prepended `K='V' `). The assignments MUST come before `sh -c`, never inside
// the quoted script: in a `K=V cmd $K` simple command the shell expands `$K`
// with the OLD value before the assignment takes effect, so an in-script prefix
// silently loses the injected value. Sorted order keeps golden output
// deterministic (DESIGN §17 transport). Values are single-quote escaped so they
// never reach a command line in clear that a shell could re-interpret.
func shEnvPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s ", k, shQuote(env[k]))
	}
	return b.String()
}

// envKV renders env as sorted `K=V` entries for exec.Cmd.Env (local transport).
// Setting the child process environment directly avoids any shell-quoting or
// expansion-ordering pitfalls that a string prefix would introduce.
func envKV(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// EncodePS builds `powershell.exe ... -EncodedCommand <b64(UTF-16LE)>`.
func EncodePS(script string, env map[string]string) string {
	full := withEnvPS(script, env)
	codes := utf16.Encode([]rune(full))
	raw := make([]byte, len(codes)*2)
	for i, c := range codes {
		raw[i*2] = byte(c)
		raw[i*2+1] = byte(c >> 8)
	}
	return "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " +
		base64.StdEncoding.EncodeToString(raw)
}

// BuildCommandLine renders a Cmd into the single string a shell channel runs.
func BuildCommandLine(osKind spec.OSKind, c Cmd) (string, error) {
	switch c.Shell {
	case ShellPowerShell:
		if osKind != spec.OSWindows {
			return "", fmt.Errorf("powershell shell requires windows target")
		}
		return EncodePS(c.Script, c.Env), nil
	case ShellSh:
		if osKind != spec.OSLinux {
			return "", fmt.Errorf("sh shell requires linux target")
		}
		// Env assignments precede `sh -c` so the whole script inherits them.
		return shEnvPrefix(c.Env) + "sh -c " + shQuote(c.Script), nil
	case ShellCmd:
		if osKind != spec.OSWindows {
			return "", fmt.Errorf("cmd shell requires windows target")
		}
		return "cmd.exe /c " + c.Script, nil
	}
	return "", fmt.Errorf("unknown shell")
}
