package pattern

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// dotnetRC builds a dotnet_api ReleaseCtx the same way engine.releaseCtx does:
// builtin env merged UNDER the spec environment (DESIGN §9.1).
func dotnetRC(pat spec.Pattern, env map[string]string) ReleaseCtx {
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "web-api", "2.1.0")
	s := &spec.Deployment{
		Metadata:    spec.Metadata{Name: "web-api"},
		Target:      spec.Target{OS: spec.OSWindows},
		Artifact:    spec.Artifact{Version: "2.1.0"},
		Pattern:     pat,
		Environment: env,
	}
	merged := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: merged}
}

// Scenario: Dotnet binPath golden (DESIGN §9.4 line 566). With launcher=dotnet_dll
// the generated S4 configure script's binPath must be the quoted
// `\"<dotnet_exe>\" \"<cur>\<dll>\" <args>` form — the dll path is ALWAYS quoted
// (even when it contains no spaces) so the SCM parses launcher and dll as two
// distinct tokens. The whole S4 script is captured as a committed golden.
func TestDotnetDllBinPathGolden(t *testing.T) {
	var d DotnetAPI
	rc := dotnetRC(spec.Pattern{
		Type:        spec.PatternDotnetAPI,
		ServiceName: "WebApi",
		DisplayName: "Contoso Web API",
		Description: "Web API",
		Launcher:    "dotnet_dll",
		DLL:         "WebApi.dll",
		DotnetExe:   `C:\Program Files\dotnet\dotnet.exe`,
		Args:        []string{"--environment", "Production"},
		Hosting:     "windows_service_native",
		StartType:   "auto",
		URLs:        "http://+:8088",
	}, nil)

	f := &scriptTransport{osKind: spec.OSWindows}
	if err := d.Configure(context.Background(), f, rc); err != nil {
		t.Fatalf("Configure(dotnet_dll): %v", err)
	}
	if len(f.scripts) != 2 {
		t.Fatalf("dotnet_dll Configure should emit S4 configure + S5 env scripts, got %d", len(f.scripts))
	}
	checkGolden(t, "dotnet_dll_s4_configure.golden", f.scripts[0])

	// The dll path must be quoted regardless of embedded spaces (DESIGN §9.4).
	wantBin := `\"C:\Program Files\dotnet\dotnet.exe\" \"C:\deploy\web-api\current\WebApi.dll\" --environment Production`
	if !strings.Contains(f.scripts[0], wantBin) {
		t.Errorf("S4 binPath must be the quoted `\"<dotnet_exe>\" \"<cur>\\<dll>\" <args>` form.\nwant substring: %q\ngot script:\n%s", wantBin, f.scripts[0])
	}
	// The dll must NOT appear unquoted (regression for the old quoteArgs-only path
	// that dropped the quotes when the dll path had no spaces).
	if strings.Contains(f.scripts[0], `dotnet.exe\" C:\deploy\web-api\current\WebApi.dll`) {
		t.Errorf("dll path must be quoted, found bare dll token:\n%s", f.scripts[0])
	}
	// launcher=dotnet_dll native hosting registers via raw sc.exe (no winsw xml).
	if !strings.Contains(f.scripts[0], "sc.exe create") {
		t.Errorf("dotnet_dll native hosting must register via sc.exe create:\n%s", f.scripts[0])
	}
	// urls is exported as ASPNETCORE_URLS in the S5 env injection.
	if !strings.Contains(f.scripts[1], "ASPNETCORE_URLS=http://+:8088") {
		t.Errorf("S5 env must inject ASPNETCORE_URLS from urls:\n%s", f.scripts[1])
	}
}

// Scenario: dotnet_dll with default dotnet_exe (PATH `dotnet`) and no user args
// still quotes the dll and emits no trailing space.
func TestDotnetDllBinPathDefaults(t *testing.T) {
	var d DotnetAPI
	rc := dotnetRC(spec.Pattern{
		Type:        spec.PatternDotnetAPI,
		ServiceName: "WebApi",
		Launcher:    "dotnet_dll",
		DLL:         "WebApi.dll",
		StartType:   "auto",
	}, nil)
	got := d.dllBinPath(rc)
	want := `\"dotnet\" \"C:\deploy\web-api\current\WebApi.dll\"`
	if got != want {
		t.Errorf("dllBinPath default: got %q want %q", got, want)
	}
}

// Scenario: Node install_deps preflight (DESIGN §9.4 line 561, NOD-02). With
// install_deps=true and no package-lock.json in the artifact, the npm ci step
// must fail with ERR_SERVICE_INSTALL naming the lockfile, using a fake Transport
// driving a scripted Result queue (exit 46 = ERR_SERVICE_INSTALL). No service is
// created because the install runs BEFORE the switch/configure.
func TestNodeInstallDepsMissingLockfile(t *testing.T) {
	var n NodeWebApp
	rc := nodeRC(3000, nil)
	// force install_deps=true on the node spec.
	rc.Spec.Pattern.InstallDeps = true

	// The remote script's `Test-Path 'package-lock.json'` guard exits 46 when the
	// lockfile is absent; the fake transport replays that scripted result.
	f := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{
		{ExitCode: ExitSvcInstall, Stderr: "package-lock.json missing (required for npm ci)"},
	}}
	err := n.InstallDeps(context.Background(), f, rc)
	if err == nil {
		t.Fatal("InstallDeps must fail when package-lock.json is missing")
	}
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StepError, got %T: %v", err, err)
	}
	if se.Code != "ERR_SERVICE_INSTALL" {
		t.Errorf("missing lockfile must map to ERR_SERVICE_INSTALL, got %q", se.Code)
	}
	if len(f.scripts) != 1 {
		t.Fatalf("InstallDeps should emit a single npm ci script, got %d", len(f.scripts))
	}
	// The script must name the lockfile it requires, and run npm ci --omit=dev
	// in the RELEASE dir (before switch).
	if !strings.Contains(f.scripts[0], "package-lock.json") {
		t.Errorf("npm ci preflight must name the required lockfile:\n%s", f.scripts[0])
	}
	if !strings.Contains(f.scripts[0], "npm ci --omit=dev") {
		t.Errorf("install must run `npm ci --omit=dev`:\n%s", f.scripts[0])
	}
	if !strings.Contains(f.scripts[0], `Set-Location 'C:\deploy\web\releases\1.0.0'`) {
		t.Errorf("npm ci must run in the release dir (before switch):\n%s", f.scripts[0])
	}
	// The lockfile guard exits with the ERR_SERVICE_INSTALL sentinel (46).
	if !strings.Contains(f.scripts[0], "exit 46") {
		t.Errorf("missing lockfile guard must exit with the ERR_SERVICE_INSTALL sentinel (46):\n%s", f.scripts[0])
	}

	// install_deps=false ⇒ InstallDeps is a no-op that never touches the transport.
	rc2 := nodeRC(3000, nil)
	f2 := &scriptTransport{osKind: spec.OSWindows}
	if err := n.InstallDeps(context.Background(), f2, rc2); err != nil {
		t.Fatalf("InstallDeps(install_deps=false): %v", err)
	}
	if len(f2.scripts) != 0 {
		t.Errorf("install_deps=false must be a no-op, ran %d script(s)", len(f2.scripts))
	}
}

// Scenario: dotnet_api aspnet-runtime preflight (DESIGN §9.4, NET-02). A box
// whose `dotnet --list-runtimes` lacks Microsoft.AspNetCore.App must fail
// Preflight with ERR_PREFLIGHT naming the runtime, BEFORE any file is copied.
func TestDotnetPreflightAspNetRuntime(t *testing.T) {
	var d DotnetAPI
	rc := dotnetRC(spec.Pattern{
		Type:        spec.PatternDotnetAPI,
		ServiceName: "WebApi",
		Launcher:    "dotnet_dll",
		DLL:         "WebApi.dll",
	}, nil)

	// runtime missing ⇒ script exits 1 with the naming error.
	fMissing := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{
		{ExitCode: 1, Stderr: "Microsoft.AspNetCore.App runtime missing"},
	}}
	err := d.Preflight(context.Background(), fMissing, rc)
	if err == nil {
		t.Fatal("Preflight must fail when Microsoft.AspNetCore.App is absent")
	}
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StepError, got %T: %v", err, err)
	}
	if se.Code != "ERR_PREFLIGHT" {
		t.Errorf("aspnet runtime miss must map to ERR_PREFLIGHT, got %q", se.Code)
	}
	if len(fMissing.scripts) != 1 || !strings.Contains(fMissing.scripts[0], "Microsoft") {
		t.Errorf("Preflight must probe `dotnet --list-runtimes` for Microsoft.AspNetCore.App:\n%v", fMissing.scripts)
	}
	if !strings.Contains(fMissing.scripts[0], "list-runtimes") {
		t.Errorf("Preflight must run `dotnet --list-runtimes`:\n%s", fMissing.scripts[0])
	}

	// launcher=exe skips the aspnet-runtime probe (self-contained/apphost).
	rcExe := dotnetRC(spec.Pattern{
		Type: spec.PatternDotnetAPI, ServiceName: "WebApi", Launcher: "exe", Exe: `WebApi.exe`,
	}, nil)
	fExe := &scriptTransport{osKind: spec.OSWindows}
	if err := d.Preflight(context.Background(), fExe, rcExe); err != nil {
		t.Fatalf("Preflight(launcher=exe): %v", err)
	}
	if len(fExe.scripts) != 0 {
		t.Errorf("launcher=exe must skip the aspnet-runtime probe, ran %d script(s)", len(fExe.scripts))
	}
}
