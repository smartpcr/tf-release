package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// RunHealthCheck executes the configured probe ON THE TARGET (DESIGN D4/D6)
// with the initial-delay/interval/total-budget loop from §6.5. Returns nil on
// healthy; ERR_HEALTH_CHECK otherwise.
func RunHealthCheck(ctx context.Context, t transport.Transport, hc *spec.HealthCheck, workDir string, env map[string]string) error {
	if hc.EffectiveType() == "none" {
		return nil
	}
	initial, interval, budget := hc.Budget()
	deadline := time.Now().Add(time.Duration(budget) * time.Second)

	select {
	case <-ctx.Done():
		return coded("ERR_HEALTH_CHECK", t.Host(), "HEALTH", ctx.Err())
	case <-time.After(time.Duration(initial) * time.Second):
	}

	var lastDetail string
	attempt := 0
	for {
		attempt++
		ok, detail := probeOnce(ctx, t, hc, workDir, env)
		if ok {
			tflog.Debug(ctx, "health check passed", map[string]interface{}{
				"host": t.Host(), "step": "HEALTH", "attempts": attempt})
			return nil
		}
		lastDetail = detail
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return coded("ERR_HEALTH_CHECK", t.Host(), "HEALTH", ctx.Err())
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
	return coded("ERR_HEALTH_CHECK", t.Host(), "HEALTH",
		fmt.Errorf("budget %ds exhausted after %d attempts; last: %s", budget, attempt, lastDetail))
}

func probeOnce(ctx context.Context, t transport.Transport, hc *spec.HealthCheck, workDir string, env map[string]string) (bool, string) {
	cmd := buildProbeCmd(t.OS(), hc, workDir, env)
	r, err := t.Exec(ctx, cmd)
	if err != nil {
		return false, err.Error()
	}
	if r.ExitCode == 0 {
		return true, ""
	}
	detail := r.Stdout
	if detail == "" {
		detail = r.Stderr
	}
	return false, fmt.Sprintf("exit=%d %s", r.ExitCode, truncate(detail, 300))
}

// buildProbeCmd renders the single-attempt probe command for the given target
// OS. It is a pure function (no I/O) so the generated win/linux scripts can be
// golden-tested (DESIGN §6.5 / §17 "health script golden"). For type "none" it
// returns the zero Cmd; callers short-circuit before invoking it.
func buildProbeCmd(os spec.OSKind, hc *spec.HealthCheck, workDir string, env map[string]string) transport.Cmd {
	switch hc.EffectiveType() {
	case "http":
		exp := hc.HTTP.ExpectStatus
		if exp == 0 {
			exp = 200
		}
		if os == spec.OSWindows {
			body := ""
			if hc.HTTP.ExpectBodyRegex != "" {
				body = fmt.Sprintf(`if($r.Content -notmatch %s){ Write-Output "body mismatch"; exit 1 }`, psq(hc.HTTP.ExpectBodyRegex))
			}
			return transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 30, Script: fmt.Sprintf(
				`try { $r=Invoke-WebRequest -UseBasicParsing -Uri %s -TimeoutSec 10 } catch { Write-Output $_.Exception.Message; exit 1 }
if([int]$r.StatusCode -ne %d){ Write-Output ("status " + $r.StatusCode); exit 1 }
%s
exit 0`, psq(hc.HTTP.URL), exp, body)}
		}
		grep := ""
		if hc.HTTP.ExpectBodyRegex != "" {
			grep = fmt.Sprintf(` && grep -Eq '%s' /tmp/.ld_hc_body`, hc.HTTP.ExpectBodyRegex)
		}
		return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 30, Script: fmt.Sprintf(
			`code=$(curl -sS -o /tmp/.ld_hc_body -w '%%{http_code}' --max-time 10 '%s') && [ "$code" = "%d" ]%s`,
			hc.HTTP.URL, exp, grep)}
	case "tcp":
		if os == spec.OSWindows {
			return transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 30, Script: fmt.Sprintf(
				`$c=New-Object Net.Sockets.TcpClient
try { $c.Connect('localhost', %d); if($c.Connected){exit 0} exit 1 } catch { exit 1 } finally { $c.Close() }`,
				hc.TCP.Port)}
		}
		return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 30,
			Script: fmt.Sprintf(`nc -z localhost %d`, hc.TCP.Port)}
	case "exec":
		if os == spec.OSWindows {
			return transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 60, Env: env,
				Script: fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(workDir), hc.Exec.Command)}
		}
		return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 60, Env: env,
			Script: fmt.Sprintf(`cd '%s' && %s`, workDir, hc.Exec.Command)}
	}
	return transport.Cmd{}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
