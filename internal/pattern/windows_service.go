package pattern

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// WindowsService implements DESIGN §9.2 (S-steps). Reused by DotnetAPI and
// (registration parts) by ClusterGeneric / NodeWebApp.
type WindowsService struct{}

// stopNeedsForceKill is the private exit sentinel the graceful S1 STOP script
// returns when the service is still running after stop_timeout_seconds, telling
// Stop to escalate to the separate S2 FORCE_KILL step (DESIGN §9.2 WSV-07).
const stopNeedsForceKill = 100

func (w *WindowsService) svcName(rc ReleaseCtx) string { return rc.Spec.Pattern.ServiceName }

func (w *WindowsService) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	// PS version gate is engine-global; nothing pattern-specific here for
	// wrapper=none. winsw needs .NET FX which ships in-box on Server 2019+.
	return nil
}

// binPath computes the sc.exe binPath= value. Uses the CURRENT junction so
// upgrades normally need no `sc config` (path is stable) — DESIGN §9.2 S4.
func (w *WindowsService) binPath(rc ReleaseCtx) string {
	p := rc.Spec.Pattern
	exe := rc.P.Current + `\` + strings.TrimLeft(p.Exe, `\/`)
	return `\"` + exe + `\"` + quoteArgs(p.Args)
}

func startTypeArg(st string) string {
	switch st {
	case "manual":
		return "demand"
	case "delayed":
		return "delayed-auto"
	default:
		return "auto"
	}
}

// resolveExe: bare names (no separator) resolve via PATH (node, dotnet);
// otherwise relative paths anchor at the CURRENT junction; absolute kept.
func resolveExe(current, exe string) string {
	if !strings.ContainsAny(exe, `\/`) && !strings.Contains(exe, ":") {
		return exe
	}
	if strings.Contains(exe, ":") || strings.HasPrefix(exe, `\\`) {
		return exe // absolute (drive or UNC)
	}
	return current + `\` + strings.TrimLeft(exe, `\/`)
}

// Configure = S0 precheck + S4 create/update + S5 env (DESIGN §9.2).
// startTypeOverride lets the cluster pattern force `demand`.
func (w *WindowsService) configure(ctx context.Context, t transport.Transport, rc ReleaseCtx, startTypeOverride string) error {
	p := rc.Spec.Pattern
	if p.Wrapper == "winsw" {
		return w.configureWinsw(ctx, t, rc)
	}
	return w.configureWithBinPath(ctx, t, rc, w.binPath(rc), startTypeOverride)
}

// configureWithBinPath is the raw sc.exe create/config path with an explicit
// binPath (used by dotnet_dll launcher and the standard flow).
func (w *WindowsService) configureWithBinPath(ctx context.Context, t transport.Transport, rc ReleaseCtx, binPath, startTypeOverride string) error {
	p := rc.Spec.Pattern
	svc := w.svcName(rc)
	display := p.DisplayName
	if display == "" {
		display = svc
	}
	st := p.StartType
	if startTypeOverride != "" {
		st = startTypeOverride
	}
	account := p.Account.Username
	env := map[string]string{}
	pwLine := ""
	if account != "" && account != "LocalSystem" {
		obj := account
		pwLine = fmt.Sprintf(`$obj=%s`, psq(obj))
		if p.Account.PasswordEnv != "" {
			env["LD_SVC_PW"] = os.Getenv(p.Account.PasswordEnv)
		}
	}
	recovery := ""
	// S4 recovery actions: exit≠0 is a failed S4 configuration and MUST
	// surface as ERR_SERVICE_INSTALL rather than being swallowed. When
	// restart_on_failure is explicitly disabled we must CLEAR any recovery
	// actions a prior version installed (reset= 0 actions= "") rather than
	// leaving them in place.
	if p.Recovery.RestartOnFailure == nil || *p.Recovery.RestartOnFailure {
		recovery = fmt.Sprintf(
			"& sc.exe failure \"%s\" reset= 86400 actions= restart/5000/restart/5000/restart/5000\nif($LASTEXITCODE -ne 0){ exit %d }",
			svc, ExitSvcInstall)
	} else {
		// Windows PowerShell 5.1 DROPS an empty-string token ("") when splatting
		// to a native command, so `actions= ""` would reach sc.exe as a dangling
		// `actions=` and fail to clear. Pass the literal two-char token '""' so
		// sc.exe receives an explicit empty actions list and clears recovery.
		recovery = fmt.Sprintf(
			"& sc.exe failure \"%s\" reset= 0 actions= '\"\"'\nif($LASTEXITCODE -ne 0){ exit %d }",
			svc, ExitSvcInstall)
	}
	// sc.exe description with an empty value clears a prior description, but PS
	// 5.1 drops an empty '' token — pass the literal '""' so sc.exe gets an
	// explicit empty argument and actually clears it (same 5.1 quirk as above).
	descArg := psq(p.Description)
	if p.Description == "" {
		descArg = `'""'`
	}
	// S0 + S4: create when 1060 (service does not exist), else config.
	script := fmt.Sprintf(`$ErrorActionPreference='Continue'
$svc=%s
$bin=%s
%s
& sc.exe query $svc *> $null
$exists = ($LASTEXITCODE -ne 1060)
if(-not $exists){
  if($env:LD_SVC_PW){ & sc.exe create $svc binPath= $bin start= %s DisplayName= %s obj= $obj password= $env:LD_SVC_PW }
  elseif($obj){       & sc.exe create $svc binPath= $bin start= %s DisplayName= %s obj= $obj }
  else{               & sc.exe create $svc binPath= $bin start= %s DisplayName= %s }
  if($LASTEXITCODE -ne 0){ exit %d }
} else {
  if($env:LD_SVC_PW){ & sc.exe config $svc binPath= $bin start= %s DisplayName= %s obj= $obj password= $env:LD_SVC_PW }
  elseif($obj){       & sc.exe config $svc binPath= $bin start= %s DisplayName= %s obj= $obj }
  else{               & sc.exe config $svc binPath= $bin start= %s DisplayName= %s obj= LocalSystem }
  if($LASTEXITCODE -ne 0){ exit %d }
}
& sc.exe description $svc %s
if($LASTEXITCODE -ne 0){ exit %d }
%s
exit 0`,
		psq(svc), psq(binPath), pwLine,
		startTypeArg(st), psq(display),
		startTypeArg(st), psq(display),
		startTypeArg(st), psq(display),
		ExitSvcInstall,
		startTypeArg(st), psq(display),
		startTypeArg(st), psq(display),
		startTypeArg(st), psq(display),
		ExitSvcInstall,
		descArg, ExitSvcInstall, recovery)
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, env, 120)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return w.writeServiceEnv(ctx, t, rc)
}

func (w *WindowsService) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	if err := w.configure(ctx, t, rc, ""); err != nil {
		return err
	}
	w.traceServiceConfig(ctx, t, rc)
	return nil
}

// traceServiceConfig captures `sc.exe qc <svc>` and records it in the provider's
// TF log at TRACE (DESIGN §18.5 WSV-08: "secret absent from `sc qc` capture in TF
// logs at TRACE"). sc.exe qc reports the service's BINARY_PATH_NAME and
// SERVICE_START_NAME (ObjectName) but NEVER the account password, so emitting the
// capture makes the redaction of the service-account secret an OBSERVABLE fact in
// the Terraform TRACE log rather than an implicit one. Best-effort: a probe
// failure is logged but never fails the deploy (the service is already configured).
func (w *WindowsService) traceServiceConfig(ctx context.Context, t transport.Transport, rc ReleaseCtx) {
	svc := w.svcName(rc)
	script := fmt.Sprintf(`$ErrorActionPreference='Continue'
& sc.exe qc %s | Out-String
exit 0`, psq(svc))
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 60)
	if err != nil {
		tflog.Trace(ctx, "sc qc capture failed", map[string]interface{}{"service": svc, "host": t.Host(), "error": err.Error()})
		return
	}
	tflog.Trace(ctx, "sc qc capture", map[string]interface{}{
		"service": svc,
		"host":    t.Host(),
		"sc_qc":   strings.TrimSpace(r.Stdout),
	})
}

// writeServiceEnv = S5: HKLM\SYSTEM\CurrentControlSet\Services\<svc>\Environment.
func (w *WindowsService) writeServiceEnv(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	if rc.Spec.Pattern.Wrapper == "winsw" {
		return nil // winsw xml carries <env> instead
	}
	svc := w.svcName(rc)
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$k='HKLM:\SYSTEM\CurrentControlSet\Services\%s'
if(-not (Test-Path $k)){ exit %d }
try{ Set-ItemProperty -Path $k -Name Environment -Type MultiString -Value %s -ErrorAction Stop }catch{ exit %d }
exit 0`, svc, ExitSvcInstall, envMultiString(rc.Env), ExitSvcInstall)
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 60)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return nil
}

// configureWinsw writes <current>\<svc>.winsw.xml then install|refresh (S4 winsw).
func (w *WindowsService) configureWinsw(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	p := rc.Spec.Pattern
	svc := w.svcName(rc)
	display := p.DisplayName
	if display == "" {
		display = svc
	}
	exe := resolveExe(rc.P.Current, p.Exe)
	xml := WinswXML(svc, display, p.Description, exe, p.Args, rc.P.SharedLogs,
		p.EffectiveStopTimeout(), rc.Env)
	xmlPath := rc.P.Current + `\` + svc + `.winsw.xml`
	wrapper := rc.P.Current + `\` + strings.TrimLeft(p.WinswExe, `\/`)
	script := fmt.Sprintf(`$ErrorActionPreference='Continue'
try{ Set-Content -Path %s -Value %s -Encoding UTF8 -ErrorAction Stop }catch{ exit %d }
& sc.exe query %s *> $null
if($LASTEXITCODE -eq 1060){
  & %s install %s
  if($LASTEXITCODE -ne 0){ exit %d }
} else {
  & %s refresh %s
  if($LASTEXITCODE -ne 0){
    & %s uninstall %s | Out-Null
    & %s install %s
    if($LASTEXITCODE -ne 0){ exit %d }
  }
}
exit 0`, psq(xmlPath), psq(xml), ExitSvcInstall,
		psq(svc),
		psq(wrapper), psq(xmlPath), ExitSvcInstall,
		psq(wrapper), psq(xmlPath),
		psq(wrapper), psq(xmlPath),
		psq(wrapper), psq(xmlPath), ExitSvcInstall)
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 180)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return nil
}

// Stop = S1 graceful stop + S2 force-kill fallback (DESIGN §9.2). The two
// phases run as DISTINCT steps so the force-kill surfaces as a structured
// `FORCE_KILL` step in logs/errors (DESIGN WSV-07) rather than being hidden
// inside the STOP step. No-op when the service is absent/already stopped.
func (w *WindowsService) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	svc := w.svcName(rc)
	timeout := rc.Spec.Pattern.EffectiveStopTimeout()
	// S1 STOP: graceful. exit 0 = stopped/absent, exit 100 = still running
	// (⇒ escalate to FORCE_KILL), any other exit = a real STOP failure.
	stopScript := fmt.Sprintf(`$ErrorActionPreference='Continue'
$svc=%s
$s = Get-Service -Name $svc -ErrorAction SilentlyContinue
if($null -eq $s -or $s.Status -eq 'Stopped'){ exit 0 }
Stop-Service -Name $svc -ErrorAction SilentlyContinue
$deadline=(Get-Date).AddSeconds(%d)
while((Get-Date) -lt $deadline){
  if((Get-Service -Name $svc).Status -eq 'Stopped'){ exit 0 }
  Start-Sleep -Seconds 1
}
exit 100`, psq(svc), timeout)
	r, err := runPS(ctx, t, t.Host(), "STOP", stopScript, nil, timeout+30)
	if err != nil {
		return err
	}
	if r.ExitCode == 0 {
		return nil
	}
	if r.ExitCode != stopNeedsForceKill {
		return failFrom(t.Host(), "STOP", r)
	}
	// S2 FORCE_KILL: taskkill the service process tree, re-wait 10s; still
	// running ⇒ exit 43 (ERR_SERVICE_STOP). Emitted as its own structured step
	// (via rc.emitStep) so the escalation is observable on success too, not just
	// as an error's step tag — DESIGN WSV-07.
	killStart := time.Now()
	killScript := fmt.Sprintf(`$ErrorActionPreference='Continue'
$svc=%s
$svcPid=(Get-CimInstance Win32_Service -Filter "Name='$svc'").ProcessId
if($svcPid -gt 0){ & taskkill /PID $svcPid /T /F | Out-Null }
Start-Sleep -Seconds 2
$deadline=(Get-Date).AddSeconds(10)
while((Get-Date) -lt $deadline){
  if((Get-Service -Name $svc).Status -eq 'Stopped'){ exit 0 }
  Start-Sleep -Seconds 1
}
exit %d`, psq(svc), ExitServiceStop)
	rk, err := runPS(ctx, t, t.Host(), "FORCE_KILL", killScript, nil, 60)
	// Record the FORCE_KILL step regardless of outcome (matches engine `timed`
	// semantics): escalation happened and must appear in the deploy step log.
	rc.emitStep(ctx, "FORCE_KILL", killStart)
	if err != nil {
		return err
	}
	if rk.ExitCode != 0 {
		return failFrom(t.Host(), "FORCE_KILL", rk)
	}
	return nil
}

// Start = S6: wait Running ≤60s; on failure attach recent SCM error events.
func (w *WindowsService) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	svc := w.svcName(rc)
	script := fmt.Sprintf(`$ErrorActionPreference='Continue'
$svc=%s
Start-Service -Name $svc -ErrorAction SilentlyContinue
$deadline=(Get-Date).AddSeconds(60)
while((Get-Date) -lt $deadline){
  if((Get-Service -Name $svc).Status -eq 'Running'){ exit 0 }
  Start-Sleep -Seconds 1
}
try {
  Get-WinEvent -FilterHashtable @{LogName='System'; Id=@(7000,7009,7031,7034); StartTime=(Get-Date).AddMinutes(-5)} -MaxEvents 20 -ErrorAction SilentlyContinue |
    Where-Object { $_.Message -match [regex]::Escape($svc) } |
    ForEach-Object { Write-Output ("EVT " + $_.Id + ": " + $_.Message) }
} catch {}
exit %d`, psq(svc), ExitServiceStart)
	r, err := runPS(ctx, t, t.Host(), "START", script, nil, 120)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "START", r)
	}
	return nil
}

func (w *WindowsService) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	svc := w.svcName(rc)
	script := fmt.Sprintf(`$s=Get-Service -Name %s -ErrorAction SilentlyContinue
if($null -eq $s){ Write-Output 'not_installed'; exit 0 }
if($s.Status -eq 'Running'){ Write-Output 'running' } else { Write-Output 'stopped' }
exit 0`, psq(svc))
	r, err := runPS(ctx, t, t.Host(), "STATUS", script, nil, 60)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

func (w *WindowsService) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	if err := w.Stop(ctx, t, rc); err != nil {
		return err
	}
	svc := w.svcName(rc)
	p := rc.Spec.Pattern
	var script string
	if p.Wrapper == "winsw" {
		wrapper := rc.P.Current + `\` + strings.TrimLeft(p.WinswExe, `\/`)
		xmlPath := rc.P.Current + `\` + svc + `.winsw.xml`
		script = fmt.Sprintf(`if(Test-Path %s){ & %s uninstall %s | Out-Null }
& sc.exe query %s *> $null
if($LASTEXITCODE -ne 1060){
  & sc.exe delete %s
  if($LASTEXITCODE -ne 0){ exit %d }
}
$deadline=(Get-Date).AddSeconds(30)
while((Get-Date) -lt $deadline){
  & sc.exe query %s *> $null
  if($LASTEXITCODE -eq 1060){ exit 0 }
  Start-Sleep -Seconds 1
}
exit %d`, psq(wrapper), psq(wrapper), psq(xmlPath),
			psq(svc), psq(svc), ExitSvcInstall,
			psq(svc), ExitSvcInstall)
	} else {
		script = fmt.Sprintf(`& sc.exe query %s *> $null
if($LASTEXITCODE -eq 1060){ exit 0 }
& sc.exe delete %s
if($LASTEXITCODE -ne 0){ exit %d }
exit 0`, psq(svc), psq(svc), ExitSvcInstall)
	}
	r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, nil, 120)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "CONFIGURE", r)
	}
	return nil
}

// WinswXML renders the wrapper config (DESIGN §9.2 S4 winsw). Deterministic
// env order for golden tests.
func WinswXML(id, name, desc, executable string, args []string, logPath string, stopWaitSec int, env map[string]string) string {
	var b strings.Builder
	b.WriteString("<service>\n")
	fmt.Fprintf(&b, "  <id>%s</id>\n", xmlEsc(id))
	fmt.Fprintf(&b, "  <name>%s</name>\n", xmlEsc(name))
	fmt.Fprintf(&b, "  <description>%s</description>\n", xmlEsc(desc))
	fmt.Fprintf(&b, "  <executable>%s</executable>\n", xmlEsc(executable))
	fmt.Fprintf(&b, "  <arguments>%s</arguments>\n", xmlEsc(winswArgs(args)))
	fmt.Fprintf(&b, "  <logpath>%s</logpath>\n", xmlEsc(logPath))
	fmt.Fprintf(&b, "  <stopwait>%dsec</stopwait>\n", stopWaitSec)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  <env name=\"%s\" value=\"%s\"/>\n", xmlEsc(k), xmlEsc(env[k]))
	}
	b.WriteString("</service>\n")
	return b.String()
}

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// winswArgs joins service arguments into the single space-separated command
// line WinSW's <arguments> element expects (DESIGN §9.2 line 541). Tokens that
// contain whitespace or quotes are wrapped in double quotes so they survive
// WinSW's own re-parse; the result is XML-escaped by the caller.
func winswArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			parts[i] = `"` + strings.ReplaceAll(a, `"`, `""`) + `"`
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}
