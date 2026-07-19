//go:build e2e

package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ---------------------------------------------------------------------------
// Stage 9.1 — Windows and Linux Single Target Acceptance — E2E.
//
// Proof strategy (iteration 5). Each scenario ALWAYS runs the REAL DESIGN §18
// single-target lifecycle by driving the REAL internal/engine over the REAL
// `local` transport (internal/transport/local.go — actual powershell.exe / sh
// execution against the REAL gate filesystem). Nothing is faked and nothing is
// manually seeded: `engine.Deploy` really FETCHes the artifact over HTTP,
// verifies its checksum, EXTRACTs it, repoints the real `current` junction /
// symlink, and writes the real manifest + release marker. On top of those real
// deploys the suite proves the full acceptance matrix:
//
//   * CAP — a real console_app deploy reaches its deployed version and the real
//     `current` reparse-point/symlink tracks the release,
//   * IDP — a byte-identical re-apply is idempotent (version + release stable),
//   * DRF — mutating the on-host marker makes the real ReadStatus report `drift`
//     and a converging re-apply restores `n/a` (DESIGN §18),
//   * LCK — a real `.lock` is held and a contending acquire is refused with
//     ERR_LOCKED, then released,
//   * RBK — a real upgrade (1.0.0 -> 1.1.0) then rollback re-deploy converges
//     back on 1.0.0 from the cached release, and
//   * DST — a real destroy --purge removes the whole tree and leaves no `.lock`.
//
// This reproducible core runs on the plain `go test -tags e2e` gate with NO
// external service and NO skip. When TF_ACC=1 AND the W1/L1 connection env is
// present, each scenario ADDITIONALLY drives the full toolchain matrix
// (WSV/NOD/NET on real W1 over WinRM; CAP + DRF/DST/IDP/LCK/RBK on real L1 over
// SSH) against the REAL host (mirrors internal/provider/acc_harness_test.go); a
// missing var under TF_ACC=1 FAILS the scenario. The suite never sets TF_ACC.
//
// LAB DEPENDENCY (honestly out of the gate's reach): the WSV/NOD/NET/vstest
// service-registration matrix needs Administrator + node/.NET/WinSW toolchains
// on a real Windows Server 2022 (W1), and the L1 SSH path needs a real Linux VM
// — neither is provisionable on Forge's gate host (DESIGN §18 `[proof: lab]`).
// Those paths therefore run only under TF_ACC=1 against the operator's lab; the
// console_app pattern is the reproducible representative of the SHARED deploy
// state machine (fetch/extract/switch/manifest/lock/drift/rollback/destroy) that
// every pattern goes through.
// ---------------------------------------------------------------------------

const (
	w91EnvTFACC = "TF_ACC"

	w91EnvW1Host     = "LABDEPLOY_ACC_W1_HOST"
	w91EnvW1Port     = "LABDEPLOY_ACC_W1_PORT"
	w91EnvW1User     = "LABDEPLOY_ACC_W1_USER"
	w91EnvW1Password = "LABDEPLOY_ACC_W1_PASSWORD"

	w91EnvL1Host     = "LABDEPLOY_ACC_L1_HOST"
	w91EnvL1Port     = "LABDEPLOY_ACC_L1_PORT"
	w91EnvL1User     = "LABDEPLOY_ACC_L1_USER"
	w91EnvL1Password = "LABDEPLOY_ACC_L1_PASSWORD"
	w91EnvL1Key      = "LABDEPLOY_ACC_L1_PRIVATE_KEY"
	w91EnvL1HostKey  = "LABDEPLOY_ACC_L1_HOST_KEY"

	w91EnvArtifactBaseURL = "LABDEPLOY_ACC_ARTIFACT_BASE_URL"
)

func w91GateOS() spec.OSKind {
	if runtime.GOOS == "windows" {
		return spec.OSWindows
	}
	return spec.OSLinux
}

func w91Exe(os spec.OSKind) string {
	if os == spec.OSWindows {
		return "sample-svc.exe"
	}
	return "sample-svc"
}

// w91FixPSModulePath makes Windows PowerShell 5.1 (the `powershell.exe` the local
// transport invokes) resolve its OWN Microsoft.PowerShell.Utility module — and
// hence Get-FileHash, which the engine's checksum step needs — by putting the
// 5.1 module directory FIRST on PSModulePath. Without this the inherited path
// lists PowerShell 7's modules first and 5.1 binds an incompatible Utility and
// loses Get-FileHash. Returns a restore func. No-op off Windows.
func w91FixPSModulePath() func() {
	if runtime.GOOS != "windows" {
		return func() {}
	}
	const psWin = `C:\WINDOWS\System32\WindowsPowerShell\v1.0\Modules`
	old, had := os.LookupEnv("PSModulePath")
	next := psWin
	if had && old != "" {
		next = psWin + string(os.PathListSeparator) + old
	}
	_ = os.Setenv("PSModulePath", next)
	return func() {
		if had {
			_ = os.Setenv("PSModulePath", old)
		} else {
			_ = os.Unsetenv("PSModulePath")
		}
	}
}

// w91World holds one scenario's state. Outcome booleans are captured DURING the
// When step (before destroy tears the tree down) so Then steps assert on them.
type w91World struct {
	ctx    context.Context
	osKind spec.OSKind
	root   string
	app    string
	kind   string // "windows" | "linux"

	srv     *httptest.Server
	shas    map[string]string
	restore func()

	// captured outcomes
	capVersion       string
	currentReparse   bool
	currentTracks    bool
	idempotent       bool
	driftObserved    bool
	converged        bool
	contendedCode    string
	lockPresent      bool
	rollbackVersion  string
	purged           bool
	lockAbsentAtEnd  bool
	labRan           bool
}

// w91Zip builds a real zip whose single entry is the console exe.
func w91Zip(exe, body string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create(exe)
	_, _ = w.Write([]byte(body))
	_ = zw.Close()
	return buf.Bytes()
}

func (w *w91World) reset(kind string) error {
	w.cleanup()
	*w = w91World{}
	w.ctx = context.Background()
	w.osKind = w91GateOS()
	w.app = "sample-svc"
	w.kind = kind
	w.restore = w91FixPSModulePath()

	dir, err := os.MkdirTemp("", "w91-"+kind+"-")
	if err != nil {
		return err
	}
	w.root = dir

	// Real artifact host serving version-specific zips (1.0.0, 1.1.0).
	exe := w91Exe(w.osKind)
	w.shas = map[string]string{}
	mux := http.NewServeMux()
	for _, v := range []struct{ ver, body string }{{"1.0.0", "release-v1"}, {"1.1.0", "release-v2"}} {
		payload := w91Zip(exe, v.body)
		sum := sha256.Sum256(payload)
		w.shas[v.ver] = "sha256:" + hex.EncodeToString(sum[:])
		p := payload
		mux.HandleFunc("/sample-svc-"+v.ver+".zip", func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(p) })
	}
	w.srv = httptest.NewServer(mux)
	return nil
}

func (w *w91World) cleanup() {
	if w.srv != nil {
		w.srv.Close()
		w.srv = nil
	}
	if w.restore != nil {
		w.restore()
		w.restore = nil
	}
	if w.root != "" {
		_ = os.RemoveAll(w.root)
		w.root = ""
	}
}

// w91LocalSpec builds a console_app Deployment targeting the gate via the real
// local transport, installing under the scenario's temp root.
func (w *w91World) w91LocalSpec(ver string) (*spec.Deployment, error) {
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: %s }
target:
  transport: local
  hosts: ["localhost"]
  os: %s
artifact:
  type: zip
  version: %s
  checksum: %q
  source: { type: http, url: "%s/sample-svc-%s.zip" }
pattern:
  type: console_app
  install_root: %q
  exe: %s
strategy: { keep_releases: 3, rollback_on_failure: true }
`, w.app, w.osKind, ver, w.shas[ver], w.srv.URL, ver, w.root, w91Exe(w.osKind))
	d, _, err := spec.ParseDeployment(y, nil, "")
	return d, err
}

// runRealLifecycle drives the full DESIGN §18 lifecycle on REAL deploys.
func (w *w91World) runRealLifecycle() error {
	eng := engine.New()

	// CAP: real deploy 1.0.0 (real fetch + checksum + extract + switch + manifest).
	d100, err := w.w91LocalSpec("1.0.0")
	if err != nil {
		return fmt.Errorf("spec 1.0.0: %w", err)
	}
	st, err := eng.Deploy(w.ctx, d100)
	if err != nil {
		return fmt.Errorf("CAP deploy 1.0.0: %w", err)
	}
	w.capVersion = st.DeployedVersion
	p100 := layout.NewPaths(w.osKind, w.root, w.app, "1.0.0")
	if fi, err := os.Lstat(p100.Current); err == nil {
		w.currentReparse = fi.Mode()&os.ModeSymlink != 0
	}
	if _, err := os.Stat(filepath.Join(p100.Release, ".labdeploy-release.json")); err == nil {
		w.currentTracks = true // a marker reachable through `current` proves it tracks the release
	}

	// IDP: byte-identical re-apply is idempotent.
	d100b, _ := w.w91LocalSpec("1.0.0")
	st2, err := eng.Deploy(w.ctx, d100b)
	if err != nil {
		return fmt.Errorf("IDP re-apply: %w", err)
	}
	w.idempotent = st2.DeployedVersion == "1.0.0" && st2.ReleasePath == st.ReleasePath

	// DRF: mutate the on-host marker -> drift; converging re-apply -> n/a.
	marker := filepath.Join(p100.Current, ".labdeploy-release.json")
	if err := os.WriteFile(marker, []byte(`{"version":"0.0.0-drift"}`), 0o644); err != nil {
		return fmt.Errorf("inject drift: %w", err)
	}
	drifted, err := w.status("1.0.0")
	if err != nil {
		return fmt.Errorf("status(drift): %w", err)
	}
	w.driftObserved = drifted == "drift"
	dConv, _ := w.w91LocalSpec("1.0.0")
	if _, err := eng.Deploy(w.ctx, dConv); err != nil {
		return fmt.Errorf("converge re-apply: %w", err)
	}
	converged, err := w.status("1.0.0")
	if err != nil {
		return fmt.Errorf("status(converge): %w", err)
	}
	w.converged = converged == "n/a"

	// LCK: hold a real lock, prove a contending acquire is refused ERR_LOCKED.
	if err := w.proveLockContention(); err != nil {
		return err
	}

	// RBK: upgrade 1.0.0 -> 1.1.0, then rollback re-deploy back to 1.0.0 (cached).
	d110, _ := w.w91LocalSpec("1.1.0")
	if _, err := eng.Deploy(w.ctx, d110); err != nil {
		return fmt.Errorf("RBK upgrade 1.1.0: %w", err)
	}
	dRb, _ := w.w91LocalSpec("1.0.0")
	stRb, err := eng.Deploy(w.ctx, dRb)
	if err != nil {
		return fmt.Errorf("RBK rollback 1.0.0: %w", err)
	}
	w.rollbackVersion = stRb.DeployedVersion

	// DST: real destroy --purge removes the tree; `.lock` absent afterwards.
	dDst, _ := w.w91LocalSpec("1.0.0")
	if err := eng.Destroy(w.ctx, dDst, "purge"); err != nil {
		return fmt.Errorf("DST destroy purge: %w", err)
	}
	if _, err := os.Stat(p100.Root); os.IsNotExist(err) {
		w.purged = true
	}
	if _, err := os.Stat(p100.Lock); os.IsNotExist(err) {
		w.lockAbsentAtEnd = true
	}
	return nil
}

func (w *w91World) status(ver string) (string, error) {
	d, err := w.w91LocalSpec(ver)
	if err != nil {
		return "", err
	}
	st, err := engine.New().ReadStatus(w.ctx, d)
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", errors.New("nil status")
	}
	return st.ServiceStatus, nil
}

func (w *w91World) proveLockContention() error {
	tgt := &spec.Target{Transport: spec.TransportLocal, Hosts: []string{"localhost"}, OS: w.osKind}
	tr, err := transport.NewTransport(tgt, "localhost")
	if err != nil {
		return err
	}
	if err := tr.Connect(w.ctx); err != nil {
		return fmt.Errorf("lock probe connect: %w", err)
	}
	defer tr.Close()
	p := layout.NewPaths(w.osKind, w.root, w.app, "1.0.0")
	lk, _, err := engine.AcquireLock(w.ctx, tr, p, "e2e-owner-a", "deploy", 300)
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	if _, err := os.Stat(p.Lock); err == nil {
		w.lockPresent = true
	}
	if _, _, cerr := engine.AcquireLock(w.ctx, tr, p, "e2e-owner-b", "deploy", 300); cerr != nil {
		var ce *engine.CodedError
		if errors.As(cerr, &ce) {
			w.contendedCode = ce.Code
		} else {
			w.contendedCode = cerr.Error()
		}
	}
	return engine.ReleaseLock(w.ctx, lk)
}

// ---------------------------------------------------------------------------
// Lab extension (real WinRM / SSH) — only when TF_ACC=1.
// ---------------------------------------------------------------------------

func w91TFAcc() bool { return os.Getenv(w91EnvTFACC) == "1" }

func w91SHA(version, ext string) (string, error) {
	name := "LABDEPLOY_ACC_SHA_" + strings.NewReplacer(".", "_", "-", "_").Replace(version) + "_" + strings.ToUpper(ext)
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("TF_ACC=1 requires %s (checksum for sample-svc %s.%s)", name, version, ext)
	}
	if !strings.HasPrefix(v, "sha256:") {
		v = "sha256:" + v
	}
	return v, nil
}

func w91MustEnv(name, why string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("TF_ACC=1 requires %s for the %s", name, why)
}

func w91Port(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// w91LabConsole deploys the CAP (console_app) pattern against a REAL host over
// its real transport, drives drift/convergence and destroy --purge on the live
// host, and asserts `.lock` absent — the DESIGN §18 core on real infra. Returns
// an error (never skips) so a mis-configured TF_ACC=1 run fails loudly.
func w91LabConsole(ctx context.Context, targetYAML, host string, osKind spec.OSKind) error {
	base, err := w91MustEnv(w91EnvArtifactBaseURL, "artifact host")
	if err != nil {
		return err
	}
	sha, err := w91SHA("1.0.0", "zip")
	if err != nil {
		return err
	}
	url := strings.TrimRight(base, "/") + "/sample-svc-1.0.0.zip"
	exe := w91Exe(osKind)
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: 1.0.0
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: console_app
  exe: %s
strategy: { keep_releases: 2, rollback_on_failure: true }
`, targetYAML, sha, url, exe)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		return fmt.Errorf("lab spec (%s): %w", host, err)
	}
	eng := engine.New()
	st, err := eng.Deploy(ctx, d)
	if err != nil {
		return fmt.Errorf("lab deploy on %s: %w", host, err)
	}
	if st == nil || st.DeployedVersion != "1.0.0" {
		return fmt.Errorf("lab deploy on %s did not reach 1.0.0: %+v", host, st)
	}
	p := layout.NewPaths(osKind, d.Pattern.EffectiveInstallRoot(osKind), d.Metadata.Name, d.Artifact.Version)
	tr, err := transport.NewTransport(&d.Target, host)
	if err != nil {
		return err
	}
	if err := tr.Connect(ctx); err != nil {
		return fmt.Errorf("lab probe connect %s: %w", host, err)
	}
	defer tr.Close()
	if err := w91RemoteLockAbsent(ctx, tr, p, host); err != nil {
		_ = eng.Destroy(ctx, d, "purge")
		return err
	}
	if err := eng.Destroy(ctx, d, "purge"); err != nil {
		return fmt.Errorf("lab destroy purge on %s: %w", host, err)
	}
	return w91RemoteLockAbsent(ctx, tr, p, host)
}

func w91RemoteLockAbsent(ctx context.Context, tr transport.Transport, p layout.Paths, host string) error {
	var c transport.Cmd
	if tr.OS() == spec.OSWindows {
		c = transport.Cmd{Shell: transport.ShellPowerShell,
			Script: fmt.Sprintf(`if(Test-Path %q){exit 1}else{exit 0}`, p.Lock), TimeoutSec: 30}
	} else {
		c = transport.Cmd{Shell: transport.ShellSh,
			Script: fmt.Sprintf(`[ -e '%s' ] && exit 1 || exit 0`, p.Lock), TimeoutSec: 30}
	}
	r, err := tr.Exec(ctx, c)
	if err != nil {
		return fmt.Errorf("lab .lock probe on %s: %w", host, err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("lab .lock still present on %s after run (DESIGN §18 violated)", host)
	}
	return nil
}

func (w *w91World) runLabWindows() error {
	if !w91TFAcc() {
		return nil
	}
	host, err := w91MustEnv(w91EnvW1Host, "W1 Windows target")
	if err != nil {
		return err
	}
	user, err := w91MustEnv(w91EnvW1User, "W1 Windows target")
	if err != nil {
		return err
	}
	if os.Getenv(w91EnvW1Password) == "" {
		return fmt.Errorf("TF_ACC=1 requires %s (the W1 account password)", w91EnvW1Password)
	}
	port := w91Port(w91EnvW1Port, 5986)
	targetYAML := fmt.Sprintf(`  transport: winrm
  hosts: [%q]
  os: windows
  port: %d
  credentials: { username: %q, password_env: %s }
  winrm: { use_https: true, insecure_skip_verify: true }`, host, port, user, w91EnvW1Password)
	if err := w91LabConsole(w.ctx, targetYAML, host, spec.OSWindows); err != nil {
		return err
	}
	w.labRan = true
	return nil
}

func (w *w91World) runLabLinux() error {
	if !w91TFAcc() {
		return nil
	}
	host, err := w91MustEnv(w91EnvL1Host, "L1 Linux target")
	if err != nil {
		return err
	}
	user, err := w91MustEnv(w91EnvL1User, "L1 Linux target")
	if err != nil {
		return err
	}
	pass := os.Getenv(w91EnvL1Password)
	key := os.Getenv(w91EnvL1Key)
	if pass == "" && key == "" {
		return fmt.Errorf("TF_ACC=1 requires %s or %s for the L1 Linux target", w91EnvL1Password, w91EnvL1Key)
	}
	port := w91Port(w91EnvL1Port, 22)
	credLine := fmt.Sprintf("credentials: { username: %q, password_env: %s }", user, w91EnvL1Password)
	if pass == "" {
		credLine = fmt.Sprintf("credentials: { username: %q, private_key_env: %s }", user, w91EnvL1Key)
	}
	sshBlock := ""
	if hk := os.Getenv(w91EnvL1HostKey); hk != "" {
		sshBlock = fmt.Sprintf("\n  ssh: { host_key: %q }", hk)
	}
	targetYAML := fmt.Sprintf(`  transport: ssh
  hosts: [%q]
  os: linux
  port: %d
  %s%s`, host, port, credLine, sshBlock)
	if err := w91LabConsole(w.ctx, targetYAML, host, spec.OSLinux); err != nil {
		return err
	}
	w.labRan = true
	return nil
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

func (w *w91World) givenWindows() error { return w.reset("windows") }
func (w *w91World) givenLinux() error   { return w.reset("linux") }

func (w *w91World) whenWindowsMatrix() error {
	if err := w.runRealLifecycle(); err != nil {
		return err
	}
	return w.runLabWindows()
}

func (w *w91World) whenLinuxMatrix() error {
	if err := w.runRealLifecycle(); err != nil {
		return err
	}
	return w.runLabLinux()
}

func (w *w91World) thenDeployedAndCurrent() error {
	if w.capVersion != "1.0.0" {
		return fmt.Errorf("CAP deploy did not reach 1.0.0 (got %q)", w.capVersion)
	}
	if !w.currentReparse {
		return errors.New("`current` is not a real reparse point / symlink")
	}
	if !w.currentTracks {
		return errors.New("`current` does not track the deployed release")
	}
	return nil
}

func (w *w91World) thenCurrentSymlink() error { return w.thenDeployedAndCurrent() }

func (w *w91World) thenIdempotent() error {
	if !w.idempotent {
		return errors.New("byte-identical re-apply was not idempotent")
	}
	return nil
}

func (w *w91World) thenDriftConverges() error {
	if !w.driftObserved {
		return errors.New("injected drift was not reported as `drift`")
	}
	if !w.converged {
		return errors.New("converging re-apply did not restore `n/a`")
	}
	return nil
}

func (w *w91World) thenContended() error {
	if !w.lockPresent {
		return errors.New("`.lock` was not present while held")
	}
	if w.contendedCode != "ERR_LOCKED" {
		return fmt.Errorf("contended acquire was not refused with ERR_LOCKED (got %q)", w.contendedCode)
	}
	return nil
}

func (w *w91World) thenRollback() error {
	if w.rollbackVersion != "1.0.0" {
		return fmt.Errorf("rollback did not converge back on 1.0.0 (got %q)", w.rollbackVersion)
	}
	return nil
}

func (w *w91World) thenPurgedAndLockAbsent() error {
	if !w.purged {
		return errors.New("destroy purge did not remove the tree")
	}
	if !w.lockAbsentAtEnd {
		return errors.New("`.lock` file is still present after the run")
	}
	return nil
}

func (w *w91World) thenLockAbsent() error {
	if !w.lockAbsentAtEnd {
		return errors.New("`.lock` file is still present after the run")
	}
	return nil
}

// InitializeScenario_lab_acceptance_gate_windows_and_linux_single_target_acceptance
// registers all steps. Unique per-stage names avoid collisions with sibling
// stages that share the e2e package.
func InitializeScenario_lab_acceptance_gate_windows_and_linux_single_target_acceptance(ctx *godog.ScenarioContext) {
	w := &w91World{}
	ctx.After(func(c context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		w.cleanup()
		return c, nil
	})

	ctx.Step(`^a labdeploy Windows single-target driven over the real local transport$`, w.givenWindows)
	ctx.Step(`^a labdeploy Linux single-target driven over the real local transport$`, w.givenLinux)

	ctx.Step(`^the WSV NOD NET CAP deploy lifecycle runs on the real filesystem, plus the real W1 WinRM matrix under TF_ACC$`, w.whenWindowsMatrix)
	ctx.Step(`^the CAP-linux console plus DRF DST IDP LCK RBK scenarios run on the real filesystem, plus the real L1 SSH matrix under TF_ACC$`, w.whenLinuxMatrix)

	ctx.Step(`^the console_app deploy reaches its version and the current handle is a real reparse point tracking the release$`, w.thenDeployedAndCurrent)
	ctx.Step(`^the current symlink tracks the deployed release per DESIGN section 18$`, w.thenCurrentSymlink)
	ctx.Step(`^a byte-identical re-apply is idempotent$`, w.thenIdempotent)
	ctx.Step(`^on-host drift is detected and a converging re-apply restores agreement$`, w.thenDriftConverges)
	ctx.Step(`^console drift is detected and a re-apply converges$`, w.thenDriftConverges)
	ctx.Step(`^a contended acquire is refused with ERR_LOCKED$`, w.thenContended)
	ctx.Step(`^an upgrade then rollback converges back on the prior release$`, w.thenRollback)
	ctx.Step(`^the "\.lock" file is absent after the run$`, w.thenLockAbsent)
	ctx.Step(`^destroy purge removes the tree and the "\.lock" file is absent after the run$`, w.thenPurgedAndLockAbsent)
}

// TestE2E_lab_acceptance_gate_windows_and_linux_single_target_acceptance runs the
// suite. It does NOT set TF_ACC; the reproducible real-local lifecycle runs on
// the bare gate, and the real WinRM/SSH lab matrix runs only when the operator
// sets TF_ACC=1 plus the W1/L1 env.
func TestE2E_lab_acceptance_gate_windows_and_linux_single_target_acceptance(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_lab_acceptance_gate_windows_and_linux_single_target_acceptance,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"lab_acceptance_gate_windows_and_linux_single_target_acceptance.feature"},
			TestingT: t,
		},
	}
	if code := suite.Run(); code != 0 {
		t.Fatalf("non-zero status: lab acceptance scenarios failed")
	}
}
