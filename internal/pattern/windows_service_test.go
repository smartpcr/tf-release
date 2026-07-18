package pattern

import (
	"context"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// winsvcRC builds a windows_service ReleaseCtx the same way engine.releaseCtx
// does: builtin env merged UNDER the spec environment (DESIGN §9.1).
func winsvcRC(pat spec.Pattern, env map[string]string) ReleaseCtx {
	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "payments-svc", "1.2.0")
	s := &spec.Deployment{
		Metadata:    spec.Metadata{Name: "payments-svc"},
		Target:      spec.Target{OS: spec.OSWindows},
		Artifact:    spec.Artifact{Version: "1.2.0"},
		Pattern:     pat,
		Environment: env,
	}
	merged := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return ReleaseCtx{App: s.Metadata.Name, Version: s.Artifact.Version, P: p, Spec: s, Env: merged}
}

// Scenario: S4 fresh vs update golden (DESIGN §9.2 S4/S5, plan Stage 4.2 line 259).
// The generated S4 configure script carries BOTH the fresh `sc.exe create` and
// the update `sc.exe config` branches, plus description + recovery actions; the
// S5 env-injection script writes the merged env as REG_MULTI_SZ. Both must match
// the committed goldens, including env injection and recovery actions.
func TestWindowsServiceS4Golden(t *testing.T) {
	var w WindowsService
	restart := true
	rc := winsvcRC(spec.Pattern{
		Type:        spec.PatternWindowsService,
		ServiceName: "PaymentsSvc",
		DisplayName: "Contoso Payments",
		Description: "Handles payments",
		Exe:         `bin\Payments.exe`,
		Args:        []string{"--config", "appsettings.json"},
		StartType:   "auto",
		Account:     spec.ServiceAccount{Username: `CONTOSO\svc-pay`, PasswordEnv: "PAY_PW"},
		Recovery:    spec.Recovery{RestartOnFailure: &restart},
	}, map[string]string{"ASPNETCORE_ENVIRONMENT": "Production"})

	f := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f, rc); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(f.scripts) != 2 {
		t.Fatalf("windows_service Configure should emit S4 configure + S5 env scripts, got %d", len(f.scripts))
	}
	checkGolden(t, "winsvc_s4_configure.golden", f.scripts[0])
	checkGolden(t, "winsvc_s5_env.golden", f.scripts[1])

	// The single S4 script embeds BOTH the fresh and update branches.
	if !strings.Contains(f.scripts[0], "sc.exe create") || !strings.Contains(f.scripts[0], "sc.exe config") {
		t.Error("S4 script must contain both fresh (create) and update (config) branches")
	}
	if !strings.Contains(f.scripts[0], "sc.exe failure") {
		t.Error("S4 script missing recovery (sc.exe failure) actions")
	}
	// Regression for the swallowed-exit defect: create, config, description and
	// recovery must EACH gate on $LASTEXITCODE (4 ERR_SERVICE_INSTALL checks).
	if got := strings.Count(f.scripts[0], "if($LASTEXITCODE -ne 0){ exit 46 }"); got < 4 {
		t.Errorf("S4 script must check exit codes for create/config/description/recovery, found %d checks:\n%s", got, f.scripts[0])
	}
	// S5 injects the merged env as a sorted REG_MULTI_SZ.
	if !strings.Contains(f.scripts[1], "REG_MULTI_SZ") && !strings.Contains(f.scripts[1], "MultiString") {
		t.Errorf("S5 script must write Environment as a MultiString:\n%s", f.scripts[1])
	}
	if !strings.Contains(f.scripts[1], "ASPNETCORE_ENVIRONMENT=Production") {
		t.Errorf("S5 script must inject the spec environment:\n%s", f.scripts[1])
	}
}

// Scenario: WinSW xml golden (DESIGN §9.2 S4 winsw, plan Stage 4.2 line 260).
// WinswXML must render <stopwait>...sec</stopwait> (NOT <stoptimeout>) and
// deterministic, sorted <env> entries.
func TestWindowsServiceWinswXMLGolden(t *testing.T) {
	env := map[string]string{
		"LD_APP":                 "payments-svc",
		"ASPNETCORE_ENVIRONMENT": "Production",
		"LD_VERSION":             "1.2.0",
	}
	xml := WinswXML(
		"PaymentsSvc", "Contoso Payments", "Handles payments",
		`C:\deploy\payments-svc\current\bin\Payments.exe`,
		[]string{"--config", "appsettings.json"},
		`C:\deploy\payments-svc\shared\logs`, 45, env)

	checkGolden(t, "winsvc_winsw.xml.golden", xml)

	if !strings.Contains(xml, "<stopwait>45sec</stopwait>") {
		t.Errorf("WinSW xml must use <stopwait>...sec</stopwait> per DESIGN §9.2, got:\n%s", xml)
	}
	if strings.Contains(xml, "stoptimeout") {
		t.Errorf("WinSW xml must NOT use <stoptimeout>, got:\n%s", xml)
	}
	// env entries are emitted in sorted key order: ASPNETCORE_ENVIRONMENT < LD_APP < LD_VERSION.
	iAsp := strings.Index(xml, "ASPNETCORE_ENVIRONMENT")
	iApp := strings.Index(xml, `name="LD_APP"`)
	iVer := strings.Index(xml, `name="LD_VERSION"`)
	if !(iAsp < iApp && iApp < iVer) {
		t.Errorf("WinSW <env> entries must be deterministically sorted, got order asp=%d app=%d ver=%d:\n%s", iAsp, iApp, iVer, xml)
	}
}

// Scenario: WinSW configure path emits the wrapper install/refresh flow that
// carries the <stopwait> xml (DESIGN §9.2 S4 winsw), captured as a golden.
func TestWindowsServiceWinswConfigureGolden(t *testing.T) {
	var w WindowsService
	rc := winsvcRC(spec.Pattern{
		Type:               spec.PatternWindowsService,
		ServiceName:        "WorkerSvc",
		DisplayName:        "Contoso Worker",
		Description:        "Background worker",
		Exe:                `bin\Worker.exe`,
		Wrapper:            "winsw",
		WinswExe:           `tools\WinSW.exe`,
		StopTimeoutSeconds: 60,
	}, map[string]string{"WORKER_MODE": "lab"})

	f := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f, rc); err != nil {
		t.Fatalf("Configure(winsw): %v", err)
	}
	if len(f.scripts) != 1 {
		t.Fatalf("winsw Configure should emit a single install/refresh script, got %d", len(f.scripts))
	}
	checkGolden(t, "winsvc_winsw_configure.golden", f.scripts[0])
	if !strings.Contains(f.scripts[0], "<stopwait>60sec</stopwait>") {
		t.Errorf("winsw configure script must embed <stopwait>60sec</stopwait>:\n%s", f.scripts[0])
	}
	if !strings.Contains(f.scripts[0], "refresh") || !strings.Contains(f.scripts[0], "install") {
		t.Errorf("winsw configure script must contain fresh install + update refresh:\n%s", f.scripts[0])
	}
}
