package pattern

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ---------------------------------------------------------------------------
// console_app (DESIGN §9.3): files-only. Verify happens via verify_command.
// ---------------------------------------------------------------------------

type ConsoleApp struct{}

func (c *ConsoleApp) Preflight(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil
}

func (c *ConsoleApp) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	vc := rc.Spec.Pattern.VerifyCommand
	if vc == "" {
		return nil
	}
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(rc.P.Current), vc)
		r, err := runPS(ctx, t, t.Host(), "CONFIGURE", script, rc.Env, 300)
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return stepErr("ERR_HEALTH_CHECK", t.Host(), "CONFIGURE",
				fmt.Errorf("verify_command exit=%d: %s", r.ExitCode, truncOut(r)))
		}
		return nil
	}
	r, err := t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Env: rc.Env, TimeoutSec: 300,
		Script: fmt.Sprintf(`cd '%s' && %s`, rc.P.Current, vc)})
	if err != nil {
		return stepErr("ERR_CONNECT", t.Host(), "CONFIGURE", err)
	}
	if r.ExitCode != 0 {
		return stepErr("ERR_HEALTH_CHECK", t.Host(), "CONFIGURE",
			fmt.Errorf("verify_command exit=%d: %s", r.ExitCode, truncOut(r)))
	}
	return nil
}

func (c *ConsoleApp) Stop(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil
}
func (c *ConsoleApp) Start(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	return nil
}

func (c *ConsoleApp) Status(ctx context.Context, t transport.Transport, rc ReleaseCtx) (string, error) {
	return "n/a", nil
}

func (c *ConsoleApp) Uninstall(ctx context.Context, t transport.Transport, rc ReleaseCtx, purge bool) error {
	return nil // engine removes files for purge
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
	out.Env["PORT"] = strconv.Itoa(p.Port)
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

func (d *DotnetAPI) Configure(ctx context.Context, t transport.Transport, rc ReleaseCtx) error {
	w := d.wrapped(rc)
	p := rc.Spec.Pattern
	if p.Launcher == "dotnet_dll" && w.Spec.Pattern.Wrapper == "none" {
		return d.ws.configureWithBinPath(ctx, t, w,
			`\"`+w.Spec.Pattern.Exe+`\"`+quoteArgs(w.Spec.Pattern.Args), "")
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
