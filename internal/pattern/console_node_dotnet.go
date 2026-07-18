package pattern

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ---------------------------------------------------------------------------
// console_app (DESIGN §9.3): files-only, NO service registration. After the
// engine switches `current`, the pattern runs post_install then verify_command
// IN the current release dir; Start/Stop are no-ops; Status is `n/a` when the
// on-host `.labdeploy-release.json` version matches the manifest, else `drift`.
// ---------------------------------------------------------------------------

// consoleReleaseMarker is the per-release marker the engine writes; console_app
// Status reads it under `current` to detect version drift (DESIGN §9.3).
const consoleReleaseMarker = ".labdeploy-release.json"

type ConsoleApp struct{}

func (c *ConsoleApp) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	// console_app needs no target tooling of its own; the engine's global
	// preflight gates (PS/space) are sufficient. This satisfies the
	// pattern-specific preflight seam the engine invokes after connect.
	return nil
}

// shq single-quotes s for POSIX sh, escaping any embedded single quote so an
// UNVALIDATED install_root-derived path cannot break out of the quoted argument
// (`install_root` is not constrained against quotes in spec/validate.go). This
// is the linux analogue of psq for PowerShell; it mirrors engine/scripts.go's
// shq so pattern scripts quote paths exactly as the engine does.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// consoleScript renders the script that runs `cmd` in the CURRENT release dir
// with the caller's env, propagating the command's exit code (DESIGN §9.3).
// Deterministic output so Configure scripts can be golden-tested.
func consoleScript(os spec.OSKind, current, cmd string) string {
	if os == spec.OSWindows {
		return fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(current), cmd)
	}
	return fmt.Sprintf("cd %s && %s", shq(current), cmd)
}

// runInCurrent executes `cmd` in the current release dir, mapping a non-zero
// exit to `code` (post_install ⇒ ERR_SERVICE_INSTALL, verify ⇒ ERR_HEALTH_CHECK).
func (c *ConsoleApp) runInCurrent(ctx context.Context, t transport.Transport, rc ReleaseCtx, label, code, cmd string) error {
	script := consoleScript(t.OS(), rc.P.Current, cmd)
	var r transport.Result
	var err error
	if t.OS() == spec.OSWindows {
		r, err = runPS(ctx, t, t.Host(), "CONFIGURE", script, rc.Env, 300)
	} else {
		r, err = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Env: rc.Env, TimeoutSec: 300, Script: script})
		if err != nil {
			err = stepErr("ERR_CONNECT", t.Host(), "CONFIGURE", err)
		}
	}
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return stepErr(code, t.Host(), "CONFIGURE",
			fmt.Errorf("%s exit=%d: %s", label, r.ExitCode, truncOut(r)))
	}
	return nil
}

func (c *ConsoleApp) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	// DESIGN §9.3: after SWITCH, run post_install then verify_command in <cur>.
	if pi := rc.Spec.Pattern.PostInstall; pi != "" {
		if err := c.runInCurrent(ctx, t, rc, "post_install", "ERR_SERVICE_INSTALL", pi); err != nil {
			return err
		}
	}
	if vc := rc.Spec.Pattern.VerifyCommand; vc != "" {
		if err := c.runInCurrent(ctx, t, rc, "verify_command", "ERR_HEALTH_CHECK", vc); err != nil {
			return err
		}
	}
	return nil
}

func (c *ConsoleApp) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil
}
func (c *ConsoleApp) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil
}

// Status: `n/a` when the on-host release marker under `current` reports the
// manifest version, else `drift` (a missing/malformed/mismatched marker all
// mean the deployed tree no longer matches the manifest) — DESIGN §9.3/§5.2.
func (c *ConsoleApp) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	sep := `\`
	if t.OS() == spec.OSLinux {
		sep = "/"
	}
	marker := rc.P.Current + sep + consoleReleaseMarker
	raw, ok, err := readMarker(ctx, t, marker)
	if err != nil {
		return "", err
	}
	if !ok {
		return "drift", nil
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil || m.Version == "" {
		return "drift", nil
	}
	if m.Version == rc.Version {
		return "n/a", nil
	}
	return "drift", nil
}

// readMarker reads a small on-host file as text, returning ok=false when it does
// not exist (exit 3). Mirrors the engine marker read (base64 over the channel so
// arbitrary bytes survive) but lives here so the pattern has no engine import.
func readMarker(ctx context.Context, t transport.Transport, path string) (string, bool, error) {
	var script string
	shell := transport.ShellPowerShell
	if t.OS() == spec.OSWindows {
		script = fmt.Sprintf(`if(Test-Path %s){[Convert]::ToBase64String([IO.File]::ReadAllBytes(%s))}else{exit 3}`,
			psq(path), psq(path))
	} else {
		shell = transport.ShellSh
		script = fmt.Sprintf(`[ -f %s ] || exit 3; base64 < %s`, shq(path), shq(path))
	}
	r, err := t.Exec(ctx, transport.Cmd{Shell: shell, Script: script, TimeoutSec: 60})
	if err != nil {
		return "", false, stepErr("ERR_CONNECT", t.Host(), "STATUS", err)
	}
	if r.ExitCode == 3 {
		return "", false, nil
	}
	if r.ExitCode != 0 {
		return "", false, stepErr("ERR_CONNECT", t.Host(), "STATUS",
			fmt.Errorf("read %s exit=%d: %s", path, r.ExitCode, truncOut(r)))
	}
	decoded, derr := base64.StdEncoding.DecodeString(strings.Map(stripWS, r.Stdout))
	if derr != nil {
		return "", false, stepErr("ERR_CONNECT", t.Host(), "STATUS", derr)
	}
	return string(decoded), true, nil
}

func stripWS(r rune) rune {
	if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
		return -1
	}
	return r
}

func (c *ConsoleApp) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	return nil // no service registration; engine removes files for purge
}

// ---------------------------------------------------------------------------
// node_web_app (DESIGN §9.4): winsw-wrapped node.exe. Reuses WindowsService by
// rewriting the pattern into a wrapper spec.
// ---------------------------------------------------------------------------

type NodeWebApp struct{ ws WindowsService }

func (n *NodeWebApp) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	p := rc.Spec.Pattern
	nodeExe := p.NodeExe
	if nodeExe == "" {
		nodeExe = "node"
	}
	checks := fmt.Sprintf(`& %s --version *> $null
if($LASTEXITCODE -ne 0){ Write-Error 'node not found on PATH'; exit 1 }`, psq(nodeExe))
	if p.InstallDeps {
		checks += "\n& npm --version *> $null\nif($LASTEXITCODE -ne 0){ Write-Error 'npm not found on PATH'; exit 1 }"
	}
	r, err := runPS(ctx, t, t.Host(), "PREFLIGHT", checks+"\nexit 0", nil, 60)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return stepErr("ERR_PREFLIGHT", t.Host(), "PREFLIGHT", fmt.Errorf("%s", truncOut(r)))
	}
	return nil
}

// InstallDeps runs `npm ci --omit=dev` in the RELEASE dir (before switch —
// engine calls this from the STAGE phase, DESIGN §9.4).
func (n *NodeWebApp) InstallDeps(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	if !rc.Spec.Pattern.InstallDeps {
		return nil
	}
	script := fmt.Sprintf(`Set-Location %s
if(-not (Test-Path 'package-lock.json')){ Write-Error 'package-lock.json missing (required for npm ci)'; exit %d }
& npm ci --omit=dev 2>&1 | Select-Object -Last 50
if($LASTEXITCODE -ne 0){ exit %d }
exit 0`, psq(rc.P.Release), ExitSvcInstall, ExitSvcInstall)
	r, err := runPS(ctx, t, t.Host(), "STAGE", script, nil, 900)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return failFrom(t.Host(), "STAGE", r)
	}
	return nil
}

// wrapped rewrites the node spec into a winsw WindowsService ReleaseCtx.
func (n *NodeWebApp) wrapped(rc ReleaseCtx) ReleaseCtx {
	p := rc.Spec.Pattern
	nodeExe := p.NodeExe
	if nodeExe == "" {
		nodeExe = "node"
	}
	clone := *rc.Spec
	cp := clone.Pattern
	cp.Wrapper = "winsw"
	cp.Exe = nodeExe // absolute-or-PATH executable; winsw resolves PATH
	cp.Args = []string{rc.P.Current + `\` + strings.TrimLeft(p.Entry, `\/`)}
	clone.Pattern = cp
	out := rc
	out.Spec = &clone
	out.Env = map[string]string{}
	for k, v := range rc.Env {
		out.Env[k] = v
	}
	// rc.Env already carries the §9.1-merged PORT (builtin node port, or the
	// spec environment value which wins). Only synthesize a fallback when the
	// merged map has none — never overwrite the spec-provided value.
	if _, ok := out.Env["PORT"]; !ok && p.Port > 0 {
		out.Env["PORT"] = strconv.Itoa(p.Port)
	}
	return out
}

func (n *NodeWebApp) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return n.ws.configureWinsw(ctx, t, n.wrapped(rc))
}
func (n *NodeWebApp) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return n.ws.Stop(ctx, t, n.wrapped(rc))
}
func (n *NodeWebApp) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return n.ws.Start(ctx, t, n.wrapped(rc))
}
func (n *NodeWebApp) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	return n.ws.Status(ctx, t, n.wrapped(rc))
}
func (n *NodeWebApp) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	return n.ws.Uninstall(ctx, t, n.wrapped(rc), purge)
}

// ---------------------------------------------------------------------------
// dotnet_api (DESIGN §9.4): windows_service with computed binPath, ASPNETCORE_URLS.
// ---------------------------------------------------------------------------

type DotnetAPI struct{ ws WindowsService }

func (d *DotnetAPI) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	p := rc.Spec.Pattern
	if p.Launcher != "dotnet_dll" {
		return nil
	}
	dotnet := p.DotnetExe
	if dotnet == "" {
		dotnet = "dotnet"
	}
	script := fmt.Sprintf(`$rt = & %s --list-runtimes 2>$null
if($LASTEXITCODE -ne 0){ Write-Error 'dotnet not found on PATH'; exit 1 }
if(-not ($rt -match 'Microsoft\.AspNetCore\.App')){ Write-Error 'Microsoft.AspNetCore.App runtime missing'; exit 1 }
exit 0`, psq(dotnet))
	r, err := runPS(ctx, t, t.Host(), "PREFLIGHT", script, nil, 60)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return stepErr("ERR_PREFLIGHT", t.Host(), "PREFLIGHT", fmt.Errorf("%s", truncOut(r)))
	}
	return nil
}

func (d *DotnetAPI) wrapped(rc ReleaseCtx) ReleaseCtx {
	p := rc.Spec.Pattern
	clone := *rc.Spec
	cp := clone.Pattern
	if p.Hosting == "winsw" {
		cp.Wrapper = "winsw"
	} else {
		cp.Wrapper = "none"
	}
	if p.Launcher == "dotnet_dll" {
		dotnet := p.DotnetExe
		if dotnet == "" {
			dotnet = "dotnet"
		}
		cp.Exe = dotnet
		cp.Args = append([]string{rc.P.Current + `\` + strings.TrimLeft(p.DLL, `\/`)}, p.Args...)
	}
	clone.Pattern = cp
	out := rc
	out.Spec = &clone
	out.Env = map[string]string{}
	for k, v := range rc.Env {
		out.Env[k] = v
	}
	if p.URLs != "" {
		out.Env["ASPNETCORE_URLS"] = p.URLs
	}
	return out
}

// dllBinPath renders the launcher=dotnet_dll service binPath in the normative
// DESIGN §9.4 form `\"<dotnet_exe>\" \"<cur>\<dll>\" <args>`. The dll path is
// ALWAYS quoted (independent of whether it contains spaces) so the SCM parses
// `<dotnet_exe> <dll>` as two distinct tokens; user args keep the standard
// quote-only-when-needed rendering via quoteArgs.
func (d *DotnetAPI) dllBinPath(rc ReleaseCtx) string {
	p := rc.Spec.Pattern
	dotnet := p.DotnetExe
	if dotnet == "" {
		dotnet = "dotnet"
	}
	dll := rc.P.Current + `\` + strings.TrimLeft(p.DLL, `\/`)
	return `\"` + dotnet + `\" \"` + dll + `\"` + quoteArgs(p.Args)
}

func (d *DotnetAPI) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	w := d.wrapped(rc)
	p := rc.Spec.Pattern
	if p.Launcher == "dotnet_dll" && w.Spec.Pattern.Wrapper == "none" {
		return d.ws.configureWithBinPath(ctx, t, w, d.dllBinPath(rc), "")
	}
	if w.Spec.Pattern.Wrapper == "winsw" {
		return d.ws.configureWinsw(ctx, t, w)
	}
	return d.ws.Configure(ctx, t, w)
}
func (d *DotnetAPI) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return d.ws.Stop(ctx, t, d.wrapped(rc))
}
func (d *DotnetAPI) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return d.ws.Start(ctx, t, d.wrapped(rc))
}
func (d *DotnetAPI) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	return d.ws.Status(ctx, t, d.wrapped(rc))
}
func (d *DotnetAPI) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	return d.ws.Uninstall(ctx, t, d.wrapped(rc), purge)
}

func truncOut(r transport.Result) string {
	s := strings.TrimSpace(r.Stderr)
	if s == "" {
		s = strings.TrimSpace(r.Stdout)
	}
	if len(s) > 800 {
		s = s[:800] + "…"
	}
	return s
}
