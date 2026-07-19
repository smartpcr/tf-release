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
	if len(f.scripts) != 3 {
		t.Fatalf("windows_service Configure should emit S4 configure + S5 env + sc-qc TRACE scripts, got %d", len(f.scripts))
	}
	checkGolden(t, "winsvc_s4_configure.golden", f.scripts[0])
	checkGolden(t, "winsvc_s5_env.golden", f.scripts[1])
	// The trailing script is the DESIGN §18.5 sc qc TRACE capture (WSV-08).
	if !strings.Contains(f.scripts[2], "sc.exe qc") {
		t.Errorf("Configure must end with an sc qc capture for the TRACE log:\n%s", f.scripts[2])
	}

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

// Scenario: scQCCapture branch matrix (DESIGN §18.5 WSV-08). traceServiceConfig must
// only log a SUCCESSFUL "sc qc capture" when the probe actually ran, sc.exe exited 0,
// and the output carries the SERVICE_NAME block — otherwise redaction would be
// asserted against a vacuous record. Each branch is exercised here — evaluator item 5.
func TestWindowsServiceSCQCCaptureBranches(t *testing.T) {
	const good = "SERVICE_NAME: SampleSvc\n        TYPE               : 10  WIN32_OWN_PROCESS\n        SERVICE_START_NAME : .\\svcuser\n"
	cases := []struct {
		name     string
		runErr   error
		exitCode int
		stdout   string
		wantOK   bool
	}{
		{"transport error", errors.New("winrm dial failed"), 0, good, false},
		{"nonzero exit (service absent, 1060)", nil, 1060, "", false},
		{"exit0 empty output", nil, 0, "", false},
		{"exit0 no SERVICE_NAME block", nil, 0, "some unrelated banner text", false},
		{"exit0 real config", nil, 0, good, true},
		{"exit0 real config with surrounding whitespace", nil, 0, "\r\n" + good + "  \n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture, ok := scQCCapture(tc.runErr, tc.exitCode, tc.stdout)
			if ok != tc.wantOK {
				t.Fatalf("scQCCapture ok=%v, want %v (capture=%q)", ok, tc.wantOK, capture)
			}
			if tc.wantOK {
				if !strings.Contains(capture, "SERVICE_NAME") {
					t.Errorf("accepted capture must carry the SERVICE_NAME block, got %q", capture)
				}
				if strings.HasPrefix(capture, "\r") || strings.HasPrefix(capture, "\n") || strings.HasSuffix(capture, " ") {
					t.Errorf("accepted capture must be trimmed, got %q", capture)
				}
			} else if capture != "" {
				t.Errorf("rejected capture must be empty, got %q", capture)
			}
		})
	}
}

// TestWindowsServiceConfigureEmitsSCQCScript proves Configure ends with the sc qc
// TRACE-capture probe script (the wire that feeds scQCCapture) — DESIGN §18.5.
func TestWindowsServiceConfigureEmitsSCQCScript(t *testing.T) {
	var w WindowsService
	rc := winsvcRC(spec.Pattern{
		Type:        spec.PatternWindowsService,
		ServiceName: "SampleSvc",
		Exe:         `bin\sample.exe`,
		StartType:   "auto",
	}, nil)
	f := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f, rc); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	last := f.scripts[len(f.scripts)-1]
	if !strings.Contains(last, "sc.exe qc") || !strings.Contains(last, "exit $LASTEXITCODE") {
		t.Errorf("Configure must end with an sc qc probe that propagates the exit code:\n%s", last)
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
	// DESIGN §9.2 line 541: a SINGLE <arguments> element, not repeated <argument>.
	if !strings.Contains(xml, "<arguments>--config appsettings.json</arguments>") {
		t.Errorf("WinSW xml must use a single <arguments> element per DESIGN §9.2, got:\n%s", xml)
	}
	if strings.Contains(xml, "<argument>") {
		t.Errorf("WinSW xml must NOT emit repeated <argument> elements, got:\n%s", xml)
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
	if len(f.scripts) != 2 {
		t.Fatalf("winsw Configure should emit a single install/refresh script + sc-qc TRACE capture, got %d", len(f.scripts))
	}
	checkGolden(t, "winsvc_winsw_configure.golden", f.scripts[0])
	if !strings.Contains(f.scripts[1], "sc.exe qc") {
		t.Errorf("winsw Configure must end with an sc qc capture for the TRACE log:\n%s", f.scripts[1])
	}
	if !strings.Contains(f.scripts[0], "<stopwait>60sec</stopwait>") {
		t.Errorf("winsw configure script must embed <stopwait>60sec</stopwait>:\n%s", f.scripts[0])
	}
	if !strings.Contains(f.scripts[0], "refresh") || !strings.Contains(f.scripts[0], "install") {
		t.Errorf("winsw configure script must contain fresh install + update refresh:\n%s", f.scripts[0])
	}
}

// Scenario: stop escalates to a STRUCTURED FORCE_KILL step (DESIGN §9.2 S2,
// WSV-07). The graceful S1 STOP script and the S2 taskkill script must be
// emitted as two distinct steps; when the kill still can't stop the service
// the error must be tagged step=FORCE_KILL / code=ERR_SERVICE_STOP (43), NOT
// hidden inside the STOP step.
func TestWindowsServiceStopForceKill(t *testing.T) {
	var w WindowsService
	rc := winsvcRC(spec.Pattern{
		Type:               spec.PatternWindowsService,
		ServiceName:        "SlowSvc",
		StopTimeoutSeconds: 10,
	}, nil)

	// graceful stop times out (sentinel 100) → force-kill succeeds (0).
	fk := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{{ExitCode: 100}, {ExitCode: 0}}}
	if err := w.Stop(context.Background(), fk, rc); err != nil {
		t.Fatalf("Stop with successful force-kill should succeed: %v", err)
	}
	if len(fk.scripts) != 2 {
		t.Fatalf("Stop must emit a graceful STOP script then a separate FORCE_KILL script, got %d", len(fk.scripts))
	}
	if !strings.Contains(fk.scripts[0], "Stop-Service") || strings.Contains(fk.scripts[0], "taskkill") {
		t.Errorf("first script must be the graceful STOP (no taskkill):\n%s", fk.scripts[0])
	}
	if !strings.Contains(fk.scripts[1], "taskkill /PID") || !strings.Contains(fk.scripts[1], "/T /F") {
		t.Errorf("second script must be the FORCE_KILL taskkill step:\n%s", fk.scripts[1])
	}
	// The old defect emitted a bare `Write-Output 'FORCE_KILL'` inside the STOP
	// step instead of a real step; guard against its return.
	if strings.Contains(fk.scripts[0], "Write-Output 'FORCE_KILL'") {
		t.Errorf("FORCE_KILL must be a structured step, not text inside STOP:\n%s", fk.scripts[0])
	}

	// graceful stop times out, force-kill also fails (exit 43) → structured error.
	fkFail := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{{ExitCode: 100}, {ExitCode: ExitServiceStop}}}
	err := w.Stop(context.Background(), fkFail, rc)
	if err == nil {
		t.Fatal("Stop must fail when force-kill cannot stop the service")
	}
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StepError, got %T: %v", err, err)
	}
	if se.Step != "FORCE_KILL" {
		t.Errorf("force-kill failure must be tagged step=FORCE_KILL, got %q", se.Step)
	}
	if se.Code != "ERR_SERVICE_STOP" {
		t.Errorf("force-kill failure must map to ERR_SERVICE_STOP, got %q", se.Code)
	}

	// already stopped/absent → single graceful STOP, no force-kill step.
	fkNoop := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{{ExitCode: 0}}}
	if err := w.Stop(context.Background(), fkNoop, rc); err != nil {
		t.Fatalf("Stop no-op should succeed: %v", err)
	}
	if len(fkNoop.scripts) != 1 {
		t.Errorf("stopped service must not run the FORCE_KILL step, got %d scripts", len(fkNoop.scripts))
	}
}

// Scenario: start polls to Running and, on failure, captures the SCM System
// event-log entries and returns ERR_SERVICE_START (exit 44) — DESIGN §9.2 S6.
func TestWindowsServiceStartEventLog(t *testing.T) {
	var w WindowsService
	rc := winsvcRC(spec.Pattern{Type: spec.PatternWindowsService, ServiceName: "PaymentsSvc"}, nil)

	fs := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{{ExitCode: ExitServiceStart, Stderr: "start failed"}}}
	err := w.Start(context.Background(), fs, rc)
	if err == nil {
		t.Fatal("Start must fail when the service never reaches Running")
	}
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StepError, got %T: %v", err, err)
	}
	if se.Code != "ERR_SERVICE_START" || se.Step != "START" {
		t.Errorf("start failure must be step=START code=ERR_SERVICE_START, got step=%q code=%q", se.Step, se.Code)
	}
	if len(fs.scripts) != 1 {
		t.Fatalf("Start should emit one script, got %d", len(fs.scripts))
	}
	if !strings.Contains(fs.scripts[0], "Get-WinEvent") {
		t.Errorf("Start must capture System event-log on failure:\n%s", fs.scripts[0])
	}
	for _, id := range []string{"7000", "7009", "7031", "7034"} {
		if !strings.Contains(fs.scripts[0], id) {
			t.Errorf("Start event-log capture must include SCM event id %s:\n%s", id, fs.scripts[0])
		}
	}
}

// Scenario: when restart_on_failure is explicitly disabled the S4 script must
// CLEAR any recovery actions a prior version installed (reset= 0 actions= "")
// rather than emitting no command and leaving stale restart actions in place.
func TestWindowsServiceRecoveryClear(t *testing.T) {
	var w WindowsService
	off := false
	rc := winsvcRC(spec.Pattern{
		Type:        spec.PatternWindowsService,
		ServiceName: "NoRestartSvc",
		Exe:         `bin\App.exe`,
		StartType:   "auto",
		Recovery:    spec.Recovery{RestartOnFailure: &off},
	}, nil)

	f := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f, rc); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if len(f.scripts) == 0 {
		t.Fatal("Configure emitted no scripts")
	}
	s4 := f.scripts[0]
	if !strings.Contains(s4, `reset= 0 actions= '""'`) {
		t.Errorf("disabled restart_on_failure must clear recovery actions with a PS 5.1-safe empty arg (reset= 0 actions= '\"\"'):\n%s", s4)
	}
	if strings.Contains(s4, "restart/5000") {
		t.Errorf("disabled restart_on_failure must NOT install restart actions:\n%s", s4)
	}
	// clearing recovery is still gated so a failed sc.exe failure surfaces.
	if !strings.Contains(s4, "if($LASTEXITCODE -ne 0){ exit 46 }") {
		t.Errorf("recovery-clear must be exit-code gated:\n%s", s4)
	}
}

// Scenario: an empty description must be CLEARED, not left stale. PS 5.1 drops an
// empty single-quoted native-command token, so the S4 script must pass the
// literal '""' to sc.exe description so it receives an explicit empty argument.
func TestWindowsServiceDescriptionClear(t *testing.T) {
	var w WindowsService
	rc := winsvcRC(spec.Pattern{
		Type:        spec.PatternWindowsService,
		ServiceName: "NoDescSvc",
		Exe:         `bin\App.exe`,
		StartType:   "auto",
		Description: "",
	}, nil)

	f := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f, rc); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	s4 := f.scripts[0]
	if !strings.Contains(s4, `& sc.exe description $svc '""'`) {
		t.Errorf("empty description must be cleared with a PS 5.1-safe empty arg (description $svc '\"\"'):\n%s", s4)
	}
	// A non-empty description must still be single-quoted normally (regression).
	rc2 := winsvcRC(spec.Pattern{
		Type: spec.PatternWindowsService, ServiceName: "DescSvc", Exe: `bin\App.exe`,
		StartType: "auto", Description: "Hello",
	}, nil)
	f2 := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Configure(context.Background(), f2, rc2); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if !strings.Contains(f2.scripts[0], `& sc.exe description $svc 'Hello'`) {
		t.Errorf("non-empty description must be passed normally:\n%s", f2.scripts[0])
	}
}

// Scenario: WinSW uninstall must POLL for the service to disappear because
// sc.exe delete is asynchronous — an immediate re-query can still see the
// service and falsely report ERR_SERVICE_INSTALL (DESIGN §9.2 winsw uninstall).
func TestWindowsServiceWinswUninstallPolls(t *testing.T) {
	var w WindowsService
	rc := winsvcRC(spec.Pattern{
		Type:        spec.PatternWindowsService,
		ServiceName: "WorkerSvc",
		Exe:         `bin\Worker.exe`,
		Wrapper:     "winsw",
		WinswExe:    `tools\WinSW.exe`,
	}, nil)

	f := &scriptTransport{osKind: spec.OSWindows}
	if err := w.Uninstall(context.Background(), f, rc, false); err != nil {
		t.Fatalf("Uninstall(winsw): %v", err)
	}
	// Uninstall runs Stop (graceful, exit 0) first, then the winsw uninstall.
	var uninstall string
	for _, s := range f.scripts {
		if strings.Contains(s, "sc.exe delete") {
			uninstall = s
		}
	}
	if uninstall == "" {
		t.Fatalf("no winsw uninstall script emitted; scripts=%v", f.scripts)
	}
	if !strings.Contains(uninstall, "(Get-Date).AddSeconds(30)") || !strings.Contains(uninstall, "if($LASTEXITCODE -eq 1060){ exit 0 }") {
		t.Errorf("winsw uninstall must poll for service disappearance after async delete:\n%s", uninstall)
	}
	// The fallback delete failure is still propagated.
	if !strings.Contains(uninstall, "if($LASTEXITCODE -ne 0){ exit 46 }") {
		t.Errorf("winsw uninstall must still propagate sc.exe delete failure:\n%s", uninstall)
	}
}
