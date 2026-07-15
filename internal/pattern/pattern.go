// Package pattern implements the per-deployment-pattern verbs (DESIGN §8.4, §9).
// Engine owns files/junction/health/manifest; patterns own service/SCM/docker/
// cluster registration and start/stop.
package pattern

import (
	"context"
	"fmt"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ReleaseCtx carries everything a pattern verb needs for one host.
type ReleaseCtx struct {
	App         string
	Version     string
	PrevVersion string
	P           layout.Paths
	Spec        *spec.Deployment
	Env         map[string]string // merged builtin+user (DESIGN §9.1)
}

// Coded exit markers a pattern's remote scripts use (DESIGN §12 table).
const (
	ExitChecksum     = 41
	ExitSwitch       = 42
	ExitServiceStop  = 43
	ExitServiceStart = 44
	ExitHealth       = 45
	ExitSvcInstall   = 46
	ExitClusterMove  = 47
)

type Pattern interface {
	// Configure creates or updates service registration / wrapper config /
	// container definition. Files are already switched to `current` when called.
	Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error
	// Stop must be a no-op when the unit is absent or already stopped.
	Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error
	// Start blocks until running (or returns ERR_SERVICE_START-coded error).
	Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error
	// Status returns one of the DESIGN §5.2 service_status values.
	Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error)
	// Uninstall removes registration; purge additionally implies engine will
	// delete the tree afterwards.
	Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error
	// Preflight validates target tooling for this pattern (DESIGN §8.1).
	Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error
}

// For returns the implementation for a validated spec.
func For(p spec.PatternType) (Pattern, error) {
	switch p {
	case spec.PatternConsoleApp:
		return &ConsoleApp{}, nil
	case spec.PatternWindowsService:
		return &WindowsService{}, nil
	case spec.PatternNodeWebApp:
		return &NodeWebApp{}, nil
	case spec.PatternDotnetAPI:
		return &DotnetAPI{}, nil
	case spec.PatternClusterGeneric:
		return &ClusterGeneric{}, nil
	case spec.PatternDockerCont:
		return &DockerContainer{}, nil
	}
	return nil, fmt.Errorf("[ERR_UNSUPPORTED] pattern %q", p)
}

// ---------------------------------------------------------------------------

type StepError struct {
	Code string
	Host string
	Step string
	Err  error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("[%s] %v (host=%s step=%s)", e.Code, e.Err, e.Host, e.Step)
}
func (e *StepError) Unwrap() error { return e.Err }

func stepErr(code, host, step string, err error) error {
	return &StepError{Code: code, Host: host, Step: step, Err: err}
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// runPS executes a PowerShell script and maps well-known exit markers.
func runPS(ctx context.Context, t transport.Transport, host, step, script string, env map[string]string, timeout int) (transport.Result, error) {
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, Env: env, TimeoutSec: timeout})
	if err != nil {
		return r, stepErr("ERR_CONNECT", host, step, err)
	}
	return r, nil
}

func codeFor(exit int) string {
	switch exit {
	case ExitChecksum:
		return "ERR_CHECKSUM_MISMATCH"
	case ExitSwitch:
		return "ERR_SWITCH"
	case ExitServiceStop:
		return "ERR_SERVICE_STOP"
	case ExitServiceStart:
		return "ERR_SERVICE_START"
	case ExitHealth:
		return "ERR_HEALTH_CHECK"
	case ExitSvcInstall:
		return "ERR_SERVICE_INSTALL"
	case ExitClusterMove:
		return "ERR_CLUSTER_MOVE"
	default:
		return "ERR_SERVICE_INSTALL"
	}
}

func failFrom(host, step string, r transport.Result) error {
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(r.Stdout)
	}
	if len(msg) > 800 {
		msg = msg[:800] + "…"
	}
	return stepErr(codeFor(r.ExitCode), host, step, fmt.Errorf("exit=%d: %s", r.ExitCode, msg))
}

// quoteArgs renders args for a Windows binPath.
func quoteArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			parts[i] = `\"` + strings.ReplaceAll(a, `"`, `\"`) + `\"`
		} else {
			parts[i] = a
		}
	}
	return " " + strings.Join(parts, " ")
}

// envMultiString renders the HKLM Services\<svc>\Environment REG_MULTI_SZ
// value list in deterministic (sorted) order — DESIGN §9.2 S5.
func envMultiString(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sortStrings(keys)
	items := make([]string, len(keys))
	for i, k := range keys {
		items[i] = psq(k + "=" + env[k])
	}
	return "@(" + strings.Join(items, ",") + ")"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
