package pattern

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// WindowsService implements DESIGN §9.2 (S-steps). Reused by DotnetAPI and
// (registration parts) by ClusterGeneric / NodeWebApp.
type WindowsService struct{}

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
	if p.Recovery.RestartOnFailure == nil || *p.Recovery.RestartOnFailure {
		recovery = fmt.Sprintf(
			`& sc.exe failure "%s" reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null`, svc)
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
  & sc.exe config $svc binPath= $bin start= %s
  if($LASTEXITCODE -ne 0){ exit %d }
}
& sc.exe description $svc %s | Out-Null
%s
exit 0`,
		psq(svc), psq(binPath), pwLine,
		startTypeArg(st), psq(display),
		startTypeArg(st), psq(display),
		startTypeArg(st), psq(display),
		ExitSvcInstall,
		startTypeArg(st), ExitSvcInstall,
		psq(p.Description), recovery)
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
	return w.configure(ctx, t, rc, "")
}

// writeServiceEnv = S5: HKLM\SYSTEM\CurrentControlSet\Services\<svc>\Environment.
func (w *WindowsService) writeServiceEnv(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	if rc.Spec.Pattern.Wrapper == "winsw" {
		return nil // winsw xml carries <env> instead
	}
	svc := w.svcName(rc)
	script := fmt.Sprintf(`$k='HKLM:\SYSTEM\CurrentControlSet\Services\%s'
if(-not (Test-Path $k)){ exit %d }
Set-ItemProperty -Path $k -Name Environment -Type MultiString -Value %s
exit 0`, svc, ExitSvcInstall, envMultiString(rc.Env))
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
Set-Content -Path %s -Value %s -Encoding UTF8
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
exit 0`, psq(xmlPath), psq(xml),
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

// Stop = S1 + S2 (force-kill fallback). No-op when absent/stopped.
func (w *WindowsService) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	svc := w.svcName(rc)
	timeout := rc.Spec.Pattern.EffectiveStopTimeout()
	script := fmt.Sprintf(`$ErrorActionPreference='Continue'
$svc=%s
$s = Get-Service -Name $svc -ErrorAction SilentlyContinue
if($null -eq $s -or $s.Status -eq 'Stopped'){ exit 0 }
Stop-Service -Name $svc -ErrorAction SilentlyContinue
$deadline=(Get-Date).AddSeconds(%d)
while((Get-Date) -lt $deadline){
  if((Get-Service -Name $svc).Status -eq 'Stopped'){ exit 0 }
  Start-Sleep -Seconds 1
}
Write-Output 'FORCE_KILL'
$svcPid=(Get-CimInstance Win32_Service -Filter "Name='$svc'").ProcessId
if($svcPid -gt 0){ & taskkill /PID $svcPid /T /F | Out-Null }
Start-Sleep -Seconds 2
$deadline=(Get-Date).AddSeconds(10)
while((Get-Date) -lt $deadline){
  if((Get-Service -Name $svc).Status -eq 'Stopped'){ exit 0 }
  Start-Sleep -Seconds 1
}
exit %d`, psq(svc), timeout, ExitServiceStop)
	r, err := runPS(ctx, t, t.Host(), "STOP", script, nil, timeout+30)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "STOP", r)
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
if($LASTEXITCODE -ne 1060){ & sc.exe delete %s | Out-Null }
exit 0`, psq(wrapper), psq(wrapper), psq(xmlPath), psq(svc), psq(svc))
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
	for _, a := range args {
		fmt.Fprintf(&b, "  <argument>%s</argument>\n", xmlEsc(a))
	}
	fmt.Fprintf(&b, "  <logpath>%s</logpath>\n", xmlEsc(logPath))
	fmt.Fprintf(&b, "  <stoptimeout>%dsec</stoptimeout>\n", stopWaitSec)
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
