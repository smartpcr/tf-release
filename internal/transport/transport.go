// Package transport abstracts remote execution + file transfer (DESIGN §8.1).
package transport

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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

// New builds a transport for one host from a merged spec.Target.
// Secrets are resolved here from the runner's environment by NAME.
func New(t *spec.Target, host string) (Transport, error) {
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
		return newWinRM(host, t.EffectivePort(), t.Credentials.Username, password,
			https, t.WinRM.InsecureSkipVerify, t.WinRM.TimeoutSeconds,
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

// withEnvPS prepends $env:K='V'; lines in sorted key order (deterministic for
// golden tests, DESIGN §17 transport).
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
		fmt.Fprintf(&b, "$env:%s=%s\n", k, psQuote(env[k]))
	}
	b.WriteString(script)
	return b.String()
}

func withEnvSh(script string, env map[string]string) string {
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
		fmt.Fprintf(&b, "export %s=%s\n", k, shQuote(env[k]))
	}
	b.WriteString(script)
	return b.String()
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
		return "sh -c " + shQuote(withEnvSh(c.Script, c.Env)), nil
	case ShellCmd:
		if osKind != spec.OSWindows {
			return "", fmt.Errorf("cmd shell requires windows target")
		}
		return "cmd.exe /c " + c.Script, nil
	}
	return "", fmt.Errorf("unknown shell")
}
