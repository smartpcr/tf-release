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
// with the initial-delay/interval/total-budget loop from §6.5. timeout_seconds
// is the TOTAL budget (incl. the initial delay): the initial sleep, each poll
// interval, AND every probe command's own timeout are bounded by the remaining
// deadline so the loop can never exceed the budget. Returns nil on healthy;
// ERR_HEALTH_CHECK once the budget is exhausted (or ctx is cancelled).
func RunHealthCheck(ctx context.Context, t transport.Transport, hc *spec.HealthCheck, workDir string, env map[string]string) error {
	if hc.EffectiveType() == "none" {
		return nil
	}
	initial, interval, budget := hc.Budget()
	deadline := time.Now().Add(time.Duration(budget) * time.Second)

	// Enforce timeout_seconds as a HARD context deadline: every probe Exec is
	// cancelled the instant the total budget is exhausted, so no probe can
	// succeed (or even keep running) past the deadline (DESIGN §6.5).
	hctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Initial delay, bounded by the total deadline.
	if initial > 0 && !sleepBounded(hctx, time.Duration(initial)*time.Second, deadline) && hctx.Err() != nil {
		return coded("ERR_HEALTH_CHECK", t.Host(), "HEALTH", hctx.Err())
	}

	var lastDetail string
	attempt := 0
	// Probe only while budget remains: the loop guard guarantees we never START
	// a probe at/after the deadline.
	for time.Now().Before(deadline) {
		attempt++
		ok, detail := probeOnce(hctx, t, hc, workDir, env, time.Until(deadline))
		if ok {
			tflog.Debug(ctx, "health check passed", map[string]interface{}{
				"host": t.Host(), "step": "HEALTH", "attempts": attempt})
			return nil
		}
		lastDetail = detail
		if hctx.Err() != nil {
			break
		}
		// Poll interval, bounded so we never sleep past the deadline.
		if !sleepBounded(hctx, time.Duration(interval)*time.Second, deadline) && hctx.Err() != nil {
			break
		}
	}
	if attempt == 0 {
		lastDetail = "budget exhausted before first probe"
	}
	return coded("ERR_HEALTH_CHECK", t.Host(), "HEALTH",
		fmt.Errorf("budget %ds exhausted after %d attempts; last: %s", budget, attempt, lastDetail))
}

// sleepBounded sleeps for at most d, and never past deadline. Returns false if
// the sleep was cut short by ctx cancellation OR by the deadline (so callers
// can distinguish "budget consumed" from "keep looping"); true if it slept the
// full requested duration.
func sleepBounded(ctx context.Context, d time.Duration, deadline time.Time) bool {
	if rem := time.Until(deadline); d > rem {
		d = rem
	}
	if d <= 0 {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// probeOnce runs a single probe attempt, capping the probe command's own
// timeout to the remaining health budget so a slow HTTP/exec call cannot blow
// past timeout_seconds.
func probeOnce(ctx context.Context, t transport.Transport, hc *spec.HealthCheck, workDir string, env map[string]string, budget time.Duration) (bool, string) {
	cmd := buildProbeCmd(t.OS(), hc, workDir, env)
	// Cap the probe command's transport timeout to whole seconds of remaining
	// budget. The hard bound is the caller's context deadline (hctx): even if
	// this cap rounds up, the Exec is cancelled exactly at the deadline so a
	// slow call can never complete past timeout_seconds.
	capSec := int((budget + time.Second - 1) / time.Second)
	if capSec < 1 {
		capSec = 1
	}
	if cmd.TimeoutSec == 0 || capSec < cmd.TimeoutSec {
		cmd.TimeoutSec = capSec
	}
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
				body = fmt.Sprintf("if($body -notmatch %s){ Write-Output \"body mismatch\"; exit 1 }\n", psq(hc.HTTP.ExpectBodyRegex))
			}
			// Invoke-WebRequest throws a WebException on any non-2xx/3xx status,
			// so we MUST catch it and read StatusCode/body off the response to be
			// able to assert expect_status values like 503 (DESIGN §6.5).
			return transport.Cmd{Shell: transport.ShellPowerShell, TimeoutSec: 30, Script: fmt.Sprintf(
				`$code=0; $body=''
try {
  $r = Invoke-WebRequest -UseBasicParsing -Uri %s -TimeoutSec 10
  $code = [int]$r.StatusCode; $body = [string]$r.Content
} catch [System.Net.WebException] {
  if ($_.Exception.Response -ne $null) {
    $resp = $_.Exception.Response
    $code = [int]$resp.StatusCode
    $sr = New-Object System.IO.StreamReader($resp.GetResponseStream())
    $body = $sr.ReadToEnd(); $sr.Close()
  } else { Write-Output $_.Exception.Message; exit 1 }
} catch { Write-Output $_.Exception.Message; exit 1 }
if($code -ne %d){ Write-Output ("status " + $code); exit 1 }
%sexit 0`, psq(hc.HTTP.URL), exp, body)}
		}
		// curl WITHOUT -f returns the body and %{http_code} even for 4xx/5xx, so
		// expect_status comparison works. URL and regex are shell-quoted to
		// prevent script breakage / injection from spec values.
		grep := ""
		if hc.HTTP.ExpectBodyRegex != "" {
			grep = fmt.Sprintf(` && grep -Eq %s /tmp/.ld_hc_body`, shq(hc.HTTP.ExpectBodyRegex))
		}
		return transport.Cmd{Shell: transport.ShellSh, TimeoutSec: 30, Script: fmt.Sprintf(
			`code=$(curl -sS -o /tmp/.ld_hc_body -w '%%{http_code}' --max-time 10 %s) && [ "$code" = "%d" ]%s`,
			shq(hc.HTTP.URL), exp, grep)}
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
			Script: fmt.Sprintf(`cd %s && %s`, shq(workDir), hc.Exec.Command)}
	}
	return transport.Cmd{}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
