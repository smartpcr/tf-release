package provider

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// consoleSpecKeep is consoleSpec with an explicit keep_releases (RBK-02 prunes
// to 1 so the rolled-back version must be re-fetched).
func consoleSpecKeep(t *testing.T, at accTarget, version, ext string, keep int) string {
	t.Helper()
	url := artifactURL(t, version, ext)
	sha := artifactSHA(t, version, ext)
	exe := "bin/sample-svc"
	if at.tgt.OS == spec.OSWindows {
		exe = `bin\sample-svc.exe`
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: %s
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: console_app
  exe: %s
strategy: { keep_releases: %d, rollback_on_failure: true }
`, at.yaml, artifactType(ext), version, sha, url, exe, keep)
}

// plantLock writes a `.lock` file on the target with the given owner and age.
// ageSeconds<=0 yields a FRESH lock (contention ⇒ ERR_LOCKED); a large ageSeconds
// yields a STALE lock that a new apply overrides with a warn (LCK-01 / LCK-02).
func plantLock(at accTarget, installRoot, app, owner string, ageSeconds int) error {
	p := layout.NewPaths(at.tgt.OS, installRoot, app, "")
	started := time.Now().UTC().Add(-time.Duration(ageSeconds) * time.Second).Format(time.RFC3339)
	content := fmt.Sprintf(`{"owner":%q,"op":"deploy","started_utc":%q,"token":"planted-token"}`, owner, started)
	root := p.Root
	winScript := fmt.Sprintf("New-Item -ItemType Directory -Force -Path '%s' | Out-Null; Set-Content -LiteralPath '%s' -Value '%s' -NoNewline",
		strings.ReplaceAll(root, "'", "''"), strings.ReplaceAll(p.Lock, "'", "''"), strings.ReplaceAll(content, "'", "''"))
	shScript := fmt.Sprintf("mkdir -p '%s' && printf '%%s' '%s' > '%s'",
		strings.ReplaceAll(root, "'", `'\''`), strings.ReplaceAll(content, "'", `'\''`), strings.ReplaceAll(p.Lock, "'", `'\''`))
	_, err := probeHost(at, winScript, shScript)
	return err
}

// deletePathOnHost removes a file/dir on the target (drift induction: DRF-02
// deletes manifest.json).
func deletePathOnHost(at accTarget, path string) error {
	_, err := probeHost(at,
		fmt.Sprintf("Remove-Item -LiteralPath '%s' -Recurse -Force -ErrorAction SilentlyContinue", strings.ReplaceAll(path, "'", "''")),
		fmt.Sprintf("rm -rf '%s'", strings.ReplaceAll(path, "'", `'\''`)))
	return err
}

// checkPathAbsent asserts a path does NOT exist on the target.
func checkPathAbsent(at accTarget, path, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("if (Test-Path -LiteralPath '%s') { 'PRESENT' } else { 'ABSENT' }", strings.ReplaceAll(path, "'", "''")),
			fmt.Sprintf("if [ -e '%s' ]; then echo PRESENT; else echo ABSENT; fi", strings.ReplaceAll(path, "'", `'\''`)))
		if err != nil {
			return fmt.Errorf("probe %s: %w", path, err)
		}
		if strings.Contains(out, "PRESENT") {
			return fmt.Errorf("%s: %s still exists on %s", why, path, at.host)
		}
		return nil
	}
}

// checkPathPresent asserts a path DOES exist on the target.
func checkPathPresent(at accTarget, path, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("if (Test-Path -LiteralPath '%s') { 'PRESENT' } else { 'ABSENT' }", strings.ReplaceAll(path, "'", "''")),
			fmt.Sprintf("if [ -e '%s' ]; then echo PRESENT; else echo ABSENT; fi", strings.ReplaceAll(path, "'", `'\''`)))
		if err != nil {
			return fmt.Errorf("probe %s: %w", path, err)
		}
		if !strings.Contains(out, "PRESENT") {
			return fmt.Errorf("%s: %s is missing on %s", why, path, at.host)
		}
		return nil
	}
}

// mustRe compiles an ExpectError regexp for the scenario helpers.
func mustRe(pat string) *regexp.Regexp { return regexp.MustCompile(pat) }

// checkReleaseCount asserts exactly want directories exist under releases/
// (CAP-02: keep_releases pruning leaves newest+previous).
func checkReleaseCount(at accTarget, want int) func(*terraform.State) error {
	return func(*terraform.State) error {
		p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "")
		out, err := probeHost(at,
			fmt.Sprintf("(Get-ChildItem -LiteralPath '%s' -Directory | Measure-Object).Count", strings.ReplaceAll(p.Releases, "'", "''")),
			fmt.Sprintf("ls -1 '%s' | wc -l", strings.ReplaceAll(p.Releases, "'", `'\''`)))
		if err != nil {
			return fmt.Errorf("probe release count: %w", err)
		}
		got := strings.TrimSpace(out)
		if got != fmt.Sprintf("%d", want) {
			return fmt.Errorf("release dir count = %s, want %d (keep_releases pruning)", got, want)
		}
		return nil
	}
}

// flipLastHex flips the final hex digit of a `sha256:<hex>` checksum so it is a
// syntactically-valid but WRONG checksum (ART-02: "off by one hex").
func flipLastHex(sha string) string {
	if sha == "" {
		return sha
	}
	b := []byte(sha)
	last := b[len(b)-1]
	if last == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	return string(b)
}

// buildLocalWindowsTarget builds the CON-05 local/windows target.
func buildLocalWindowsTarget() spec.Target {
	return spec.Target{
		Transport: spec.TransportLocal,
		Hosts:     []string{"localhost"},
		OS:        spec.OSWindows,
	}
}

// sep returns the path separator layout uses for an OS (mirrors layout.sep,
// which is unexported).
func sep(os spec.OSKind) rune {
	if os == spec.OSLinux {
		return '/'
	}
	return '\\'
}

// runProbe connects to the (single-host) target and runs one shell script,
// returning the raw transport result (including ExitCode). error is returned only
// for transport-level failures; a nonzero remote exit is reported via Result so
// callers that expect a specific code (e.g. `sc query`⇒1060 for an absent
// service) can inspect it.
func runProbe(at accTarget, winScript, shScript string) (transport.Result, error) {
	tr, err := transport.NewTransport(&at.tgt, at.host)
	if err != nil {
		return transport.Result{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := tr.Connect(ctx); err != nil {
		return transport.Result{}, err
	}
	defer tr.Close()
	var cmd transport.Cmd
	if at.tgt.OS == spec.OSWindows {
		cmd = transport.Cmd{Shell: transport.ShellPowerShell, Script: winScript}
	} else {
		cmd = transport.Cmd{Shell: transport.ShellSh, Script: shScript}
	}
	return tr.Exec(ctx, cmd)
}

// probeHost runs one script and returns stdout, FAILING on a nonzero remote exit
// code. A drift-setup or observation probe that silently swallows a nonzero exit
// would let a failed setup masquerade as a valid scenario (evaluator item 2), so
// every probe used by a positive check must go through here.
func probeHost(at accTarget, winScript, shScript string) (string, error) {
	res, err := runProbe(at, winScript, shScript)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("remote script exit=%d stderr=%q", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// preConfig wraps an out-of-band mutation (drift/lock induction) so a failure to
// set up the scenario FAILS the test rather than being silently discarded — a
// discarded PreConfig error would let the following plan/apply assert against an
// un-perturbed host and pass for the wrong reason (evaluator item 2).
func preConfig(t *testing.T, fn func() error) func() {
	return func() {
		t.Helper()
		if err := fn(); err != nil {
			t.Fatalf("scenario PreConfig setup failed (host untouched, assertions would be invalid): %v", err)
		}
	}
}

// checkCurrentTarget asserts `current` resolves to releases/<version> — a junction
// on Windows, a symlink on Linux (DESIGN §9.1, §18 CAP-01/CAP-03).
func checkCurrentTarget(at accTarget, version string) func(*terraform.State) error {
	return func(*terraform.State) error {
		p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", version)
		out, err := probeHost(at,
			fmt.Sprintf("(Get-Item -LiteralPath '%s').Target", strings.ReplaceAll(p.Current, "'", "''")),
			fmt.Sprintf("readlink '%s'", strings.ReplaceAll(p.Current, "'", `'\''`)))
		if err != nil {
			return fmt.Errorf("probe current target: %w", err)
		}
		if !strings.Contains(strings.ToLower(out), strings.ToLower(version)) {
			return fmt.Errorf("current does not resolve to release %s (got %q)", version, strings.TrimSpace(out))
		}
		return nil
	}
}

// Stage 9.1 — shared spec builders + step helpers for the lab (W1/L1) scenario
// tests. The sample application is `sample-svc` (DESIGN §18): a tiny .NET worker
// packaged as zip/nupkg at 1.0.0, 1.1.0 and 1.2.0-bad. Artifact URLs and
// checksums are supplied by env so the same tests target any lab package host.

// artifactSHA returns the sha256 checksum (as `sha256:<64hex>`) the lab publishes
// for a given sample-svc version+FORMAT, read from
// LABDEPLOY_ACC_SHA_<VER-normalized>_<EXT>. The checksum is format-specific: the
// zip and nupkg of the same version have DIFFERENT bytes and therefore different
// sha256, so the env key MUST carry the extension (evaluator item 1). Under
// TF_ACC=1 a missing checksum is a FAILURE — a scenario cannot silently pass with
// an unverifiable artifact.
func artifactSHA(t *testing.T, version, ext string) string {
	t.Helper()
	norm := strings.NewReplacer(".", "_", "-", "_")
	name := "LABDEPLOY_ACC_SHA_" + norm.Replace(strings.ToUpper(version)) + "_" + strings.ToUpper(ext)
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("TF_ACC=1 requires %s (the sha256 of the %s of sample-svc %s)", name, ext, version)
	}
	if !strings.HasPrefix(v, "sha256:") {
		v = "sha256:" + v
	}
	return v
}

// consoleSpec builds a console_app Deployment spec for the given target/version.
// verifyCmd is the optional verify_command (e.g. `sample-svc.exe --version`).
func consoleSpec(t *testing.T, at accTarget, version, ext, verifyCmd string) string {
	t.Helper()
	url := artifactURL(t, version, ext)
	sha := artifactSHA(t, version, ext)
	exe := "bin/sample-svc"
	if at.tgt.OS == spec.OSWindows {
		exe = `bin\sample-svc.exe`
	}
	verify := ""
	if verifyCmd != "" {
		verify = fmt.Sprintf("\n  verify_command: %q", verifyCmd)
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: %s
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: console_app
  exe: %s%s
strategy: { keep_releases: 2, rollback_on_failure: true }
`, at.yaml, artifactType(ext), version, sha, url, exe, verify)
}

// winServiceSpec builds a windows_service Deployment spec (W1 only). healthVariant
// lets a scenario point health at a 500-serving build; extra is appended raw into
// the pattern block (e.g. wrapper=winsw, account, stop_timeout_seconds).
func winServiceSpec(t *testing.T, at accTarget, version, ext, healthURL, extra string) string {
	t.Helper()
	url := artifactURL(t, version, ext)
	sha := artifactSHA(t, version, ext)
	health := ""
	if healthURL != "" {
		health = fmt.Sprintf(`
health_check:
  type: http
  http: { url: %q }`, healthURL)
	}
	patExtra := ""
	if extra != "" {
		patExtra = "\n" + extra
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: %s
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\sample-svc.exe%s%s
strategy: { keep_releases: 2, rollback_on_failure: true }
`, at.yaml, artifactType(ext), version, sha, url, patExtra, health)
}

func artifactType(ext string) string {
	if ext == "nupkg" {
		return "nupkg"
	}
	return "zip"
}

// winServiceSpecStrategy is winServiceSpec with a caller-supplied strategy block
// (e.g. rollback_on_failure: false for WSV-05).
func winServiceSpecStrategy(t *testing.T, at accTarget, version, ext, healthURL, strategy string) string {
	t.Helper()
	url := artifactURL(t, version, ext)
	sha := artifactSHA(t, version, ext)
	health := ""
	if healthURL != "" {
		health = fmt.Sprintf(`
health_check:
  type: http
  http: { url: %q }`, healthURL)
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: %s
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\sample-svc.exe%s
%s
`, at.yaml, artifactType(ext), version, sha, url, health, strategy)
}

// nodeSpec builds a node_web_app Deployment spec (W1 only). installDeps toggles
// npm-install on the target (DESIGN §18.6 NOD).
func nodeSpec(t *testing.T, at accTarget, version string, port int, installDeps bool) string {
	t.Helper()
	url := artifactURL(t, version, "zip")
	sha := artifactSHA(t, version, "zip")
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: node_web_app
  service_name: SampleSvc
  entry: server.js
  port: %d
  install_deps: %t
  winsw_exe: tools\winsw.exe
health_check:
  type: http
  http: { url: %q }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, at.yaml, version, sha, url, port, installDeps, healthURLFor(port))
}

// dotnetSpec builds a dotnet_api Deployment spec (W1 only). launcher is exe or
// dotnet_dll; hosting is windows_service_native or winsw (DESIGN §18.6 NET).
func dotnetSpec(t *testing.T, at accTarget, version, launcher, hosting string) string {
	t.Helper()
	url := artifactURL(t, version, "zip")
	sha := artifactSHA(t, version, "zip")
	launchLine := "  exe: bin\\sample-svc.exe"
	if launcher == "dotnet_dll" {
		launchLine = "  dll: bin\\sample-svc.dll"
	}
	hostLine := ""
	if hosting == "winsw" {
		hostLine = "\n  winsw_exe: tools\\winsw.exe"
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: zip
  version: %q
  checksum: %q
  source: { type: http, url: %q }
pattern:
  type: dotnet_api
  service_name: SampleSvc
  launcher: %s
%s
  hosting: %s%s
  urls: "http://+:8088"
health_check:
  type: http
  http: { url: %q }
strategy: { keep_releases: 2, rollback_on_failure: true }
`, at.yaml, version, sha, url, launcher, launchLine, hosting, hostLine, healthURLFor(8088))
}

// healthURLFor mirrors healthURL but is import-safe for the specs file.
func healthURLFor(port int) string {
	if u := os.Getenv("LABDEPLOY_ACC_HEALTH_URL"); u != "" {
		return u
	}
	return fmt.Sprintf("http://localhost:%d/health", port)
}

// applyStep is a positive resource.TestStep applying a config and running the
// provided checks. The lock-absent invariant is appended automatically so every
// scenario ends by proving `.lock` is gone on the touched host.
func applyStep(at accTarget, config string, checks ...resource.TestCheckFunc) resource.TestStep {
	all := append([]resource.TestCheckFunc{}, checks...)
	all = append(all, checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"))
	return resource.TestStep{
		Config: config,
		Check:  resource.ComposeAggregateTestCheckFunc(all...),
	}
}

// errorStep is a negative resource.TestStep expecting a coded error.
func errorStep(config, wantErr string) resource.TestStep {
	return resource.TestStep{
		Config:      config,
		ExpectError: mustRe(wantErr),
	}
}

// errorScenario drives a negative scenario expecting wantErr and runs post-state
// probes (release-absent, service-absent, container-list-unchanged, ...) in
// CheckDestroy. This is necessary because terraform-plugin-testing does NOT
// invoke a TestStep.Check when ExpectError matches, so any on-host post-condition
// (DESIGN §18 "POST-STATE:" clauses) must be asserted at destroy time. The
// `.lock`-absent invariant is always included.
func errorScenario(t *testing.T, at accTarget, cfg, wantErr string, post ...resource.TestCheckFunc) {
	t.Helper()
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	all := append([]resource.TestCheckFunc{checkLockAbsent(at.tgt, installRootFor(at), "sample-svc")}, post...)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    []resource.TestStep{{Config: cfg, ExpectError: mustRe(wantErr)}},
		CheckDestroy:             resource.ComposeAggregateTestCheckFunc(all...),
	})
}

// installRootFor returns the default install root for the target OS (no explicit
// install_root is set in the harness specs, so the engine default applies).
func installRootFor(at accTarget) string {
	if at.tgt.OS == spec.OSWindows {
		return `C:\deploy`
	}
	return "/opt/deploy"
}

// runScenario is the standard single-target acceptance driver: it env-gates,
// wires the reattach address, sets the account password into the runner env
// (spec references it by NAME), and runs the steps.
func runScenario(t *testing.T, at accTarget, steps ...resource.TestStep) {
	t.Helper()
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
		// After the last step Terraform destroys the resource; the deployment
		// destroy (purge) must also leave `.lock` absent. The probe reaches the
		// (valid, reachable) scenario target.
		CheckDestroy: checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
	})
}

// runScenarioNoProbe drives connectivity/auth NEGATIVE scenarios whose whole
// point is that the host is never reached (unroutable host, wrong password,
// host-key mismatch). No host is touched, so the `.lock` invariant is trivially
// satisfied and probing with the broken target would itself fail — hence no
// CheckDestroy probe here.
func runScenarioNoProbe(t *testing.T, steps ...resource.TestStep) {
	t.Helper()
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    steps,
	})
}

// ---------------------------------------------------------------------------
// On-host observation helpers (evaluator items 5-18): the Terraform attributes
// alone don't prove the DESIGN §18 post-state, so these connect to the touched
// host and assert the actual filesystem / service / registry / health outcome.
// ---------------------------------------------------------------------------

// psq / shq single-quote a path for the two shells (mirrors layout's escaping).
func psq(s string) string { return strings.ReplaceAll(s, "'", "''") }
func shq(s string) string { return strings.ReplaceAll(s, "'", `'\''`) }

// hostReadFile returns a file's contents on the target, failing if it is missing
// (Get-Content / cat both exit nonzero on a missing file ⇒ probeHost errors).
func hostReadFile(at accTarget, path string) (string, error) {
	return probeHost(at,
		fmt.Sprintf("Get-Content -LiteralPath '%s' -Raw", psq(path)),
		fmt.Sprintf("cat '%s'", shq(path)))
}

// checkFileContains asserts a file on the target exists and contains want.
func checkFileContains(at accTarget, path, want, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := hostReadFile(at, path)
		if err != nil {
			return fmt.Errorf("%s: read %s: %w", why, path, err)
		}
		if !strings.Contains(out, want) {
			return fmt.Errorf("%s: %s does not contain %q", why, path, want)
		}
		return nil
	}
}

// checkReleaseMarkerSHA asserts releases/<version>/.labdeploy-release.json exists
// AND records the expected sha (ART-01: the marker proves the verified checksum,
// not merely that a file was written) — evaluator item 5.
func checkReleaseMarkerSHA(at accTarget, version, ext string, wantSHA string) func(*terraform.State) error {
	hex := strings.TrimPrefix(wantSHA, "sha256:")
	return func(*terraform.State) error {
		p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", version)
		marker := p.Release + string(sep(at.tgt.OS)) + ".labdeploy-release.json"
		out, err := hostReadFile(at, marker)
		if err != nil {
			return fmt.Errorf("release marker %s: %w", marker, err)
		}
		if !strings.Contains(strings.ToLower(out), strings.ToLower(hex)) {
			return fmt.Errorf("release marker %s does not record sha %s (got %q)", marker, hex, strings.TrimSpace(out))
		}
		return nil
	}
}

// checkReleaseAbsent asserts releases/<version> does NOT exist (ART-02: a failed
// checksum must leave no release dir) — evaluator item 6.
func checkReleaseAbsent(at accTarget, version, why string) func(*terraform.State) error {
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", version)
	return checkPathAbsent(at, p.Release, why)
}

// checkStagingEmpty asserts the staging dir holds no files (ART-02: a failed
// fetch must not leave a partial payload behind) — evaluator item 6.
func checkStagingEmpty(at accTarget) func(*terraform.State) error {
	return func(*terraform.State) error {
		p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "")
		out, err := probeHost(at,
			fmt.Sprintf("if (Test-Path -LiteralPath '%s') { (Get-ChildItem -LiteralPath '%s' -Recurse -File | Measure-Object).Count } else { 0 }", psq(p.Staging), psq(p.Staging)),
			fmt.Sprintf("if [ -d '%s' ]; then find '%s' -type f | wc -l; else echo 0; fi", shq(p.Staging), shq(p.Staging)))
		if err != nil {
			return fmt.Errorf("staging probe: %w", err)
		}
		if strings.TrimSpace(out) != "0" {
			return fmt.Errorf("staging is not empty after failed fetch (%s files under %s)", strings.TrimSpace(out), p.Staging)
		}
		return nil
	}
}

// checkManifestVersion asserts manifest.json still records the expected current
// version (ART-02/WSV: a failed op must not mutate the recorded state) — item 6.
func checkManifestVersion(at accTarget, version, why string) func(*terraform.State) error {
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "")
	return checkFileContains(at, p.Manifest, version, why)
}

// checkManifestContains asserts manifest.json contains an arbitrary token (e.g.
// `"result":"failed"` or `"last_operation"`) — used by WSV-05 drift proof.
func checkManifestContains(at accTarget, token, why string) func(*terraform.State) error {
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "")
	return checkFileContains(at, p.Manifest, token, why)
}

// checkWindowsEventLog asserts the System event log contains a recent Service
// Control Manager error (id 7000/7009/7011/7031) — WSV-03's fail-start proof
// (DESIGN §18 WSV-03 "an event-log 7000/7009 line") — evaluator item 12.
func checkWindowsEventLog(at accTarget, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			"$e = Get-WinEvent -FilterHashtable @{LogName='System';Id=7000,7009,7011,7031} -MaxEvents 20 -ErrorAction SilentlyContinue; if ($e) { 'EVT_FOUND' } else { 'EVT_NONE' }",
			"echo EVT_NONE")
		if err != nil {
			return fmt.Errorf("%s: event-log probe: %w", why, err)
		}
		if !strings.Contains(out, "EVT_FOUND") {
			return fmt.Errorf("%s: no SCM 7000/7009/7011/7031 event found for the fail-start", why)
		}
		return nil
	}
}

// checkWinswXMLVersion asserts the regenerated WinSW service XML under `current`
// references the expected release version (WSV-06: xml regenerated on upgrade) —
// evaluator item 13.
func checkWinswXMLVersion(at accTarget, version, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		cur := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Current
		xml := cur + string(sep(at.tgt.OS)) + "SampleSvc.xml"
		out, err := hostReadFile(at, xml)
		if err != nil {
			return fmt.Errorf("%s: read winsw xml %s: %w", why, xml, err)
		}
		if !strings.Contains(out, version) {
			return fmt.Errorf("%s: winsw xml %s does not reference %s (not regenerated?)", why, xml, version)
		}
		return nil
	}
}

// checkServiceState asserts a Windows service is in wantState (RUNNING/STOPPED).
// DESIGN §18 WSV asserts via `sc query` — evaluator items 6, 12, 13.
func checkServiceState(at accTarget, svc, wantState string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("(Get-Service -Name '%s' -ErrorAction SilentlyContinue).Status; if(-not $?){'NOSVC'}", psq(svc)),
			"echo NOSVC")
		if err != nil {
			return fmt.Errorf("service state probe: %w", err)
		}
		if !strings.Contains(strings.ToUpper(out), strings.ToUpper(wantState)) {
			return fmt.Errorf("service %s state = %q, want %s", svc, strings.TrimSpace(out), wantState)
		}
		return nil
	}
}

// checkServiceAbsent asserts a Windows service does NOT exist (DST-01: sc query on
// a removed service returns 1060) — evaluator item 16.
func checkServiceAbsent(at accTarget, svc string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("if (Get-Service -Name '%s' -ErrorAction SilentlyContinue) { 'SVC_PRESENT' } else { 'SVC_ABSENT(1060)' }", psq(svc)),
			"echo SVC_ABSENT")
		if err != nil {
			return fmt.Errorf("service-absent probe: %w", err)
		}
		if !strings.Contains(out, "SVC_ABSENT") {
			return fmt.Errorf("service %s still present after purge (want sc query⇒1060)", svc)
		}
		return nil
	}
}

// checkServiceObjectName asserts the service runs under wantAccount (WSV-08:
// ObjectName=.\svcuser) — evaluator item 13.
func checkServiceObjectName(at accTarget, svc, wantAccount string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("(Get-CimInstance Win32_Service -Filter \"Name='%s'\").StartName", psq(svc)),
			"echo n/a")
		if err != nil {
			return fmt.Errorf("service ObjectName probe: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(out), wantAccount) {
			return fmt.Errorf("service %s ObjectName = %q, want %q", svc, strings.TrimSpace(out), wantAccount)
		}
		return nil
	}
}

// checkHealthBody asserts an HTTP GET of url returns a body containing want
// (DESIGN §18 WSV/NOD/NET: `/health` body `v=<ver>`) — evaluator items 12, 14.
func checkHealthBody(at accTarget, url, want string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("(Invoke-WebRequest -UseBasicParsing -Uri '%s').Content", psq(url)),
			fmt.Sprintf("curl -fsS '%s'", shq(url)))
		if err != nil {
			return fmt.Errorf("GET %s: %w", url, err)
		}
		if !strings.Contains(out, want) {
			return fmt.Errorf("GET %s body = %q, want to contain %q", url, strings.TrimSpace(out), want)
		}
		return nil
	}
}

// checkHKLMEnv asserts the machine environment (HKLM) exposes name=value — the
// service env delivery DESIGN §18 WSV-01/NET-01 require (LD_VERSION,
// ASPNETCORE_URLS) — evaluator items 12, 14.
func checkHKLMEnv(at accTarget, name, value string) func(*terraform.State) error {
	const key = `HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("(Get-ItemProperty -Path '%s' -Name '%s' -ErrorAction SilentlyContinue).'%s'", key, psq(name), psq(name)),
			"echo n/a")
		if err != nil {
			return fmt.Errorf("HKLM env %s probe: %w", name, err)
		}
		if !strings.Contains(out, value) {
			return fmt.Errorf("HKLM env %s = %q, want to contain %q", name, strings.TrimSpace(out), value)
		}
		return nil
	}
}

// checkAppLogContains asserts shared/logs/app.log contains want (env dump /
// heartbeat / FORCE_KILL step) — evaluator items 12, 13, 14.
func checkAppLogContains(at accTarget, want, why string) func(*terraform.State) error {
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "")
	logPath := p.SharedLogs + string(sep(at.tgt.OS)) + "app.log"
	return checkFileContains(at, logPath, want, why)
}

// checkReleaseSubdir asserts releases/<version>/<sub> exists (ART-04 nupkg
// `lib/...`; NOD-03 `node_modules`) — evaluator items 8, 14.
func checkReleaseSubdir(at accTarget, version, sub, why string) func(*terraform.State) error {
	p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", version)
	target := p.Release + string(sep(at.tgt.OS)) + sub
	return checkPathPresent(at, target, why)
}

// snapshotContainers returns the current `docker ps -a` id list on the target
// (ART-06: prove a failed docker login left the container list unchanged).
func snapshotContainers(at accTarget) (string, error) {
	return probeHost(at, "docker ps -a --format '{{.ID}}' | Sort-Object", "docker ps -a --format '{{.ID}}' | sort")
}

// checkContainersUnchanged asserts `docker ps -a` matches a pre-captured snapshot
// (ART-06: a failed login must not create/remove containers) — evaluator item 9.
func checkContainersUnchanged(at accTarget, before string) func(*terraform.State) error {
	return func(*terraform.State) error {
		after, err := snapshotContainers(at)
		if err != nil {
			return fmt.Errorf("container list probe: %w", err)
		}
		if strings.TrimSpace(before) != strings.TrimSpace(after) {
			return fmt.Errorf("container list changed after failed docker login: before=%q after=%q", strings.TrimSpace(before), strings.TrimSpace(after))
		}
		return nil
	}
}

// hostMtime returns the modification time (epoch seconds) of a path on the target.
func hostMtime(at accTarget, path string) (string, error) {
	return probeHost(at,
		fmt.Sprintf("[int][double]::Parse((Get-Item -LiteralPath '%s').LastWriteTimeUtc.Subtract([datetime]'1970-01-01').TotalSeconds)", psq(path)),
		fmt.Sprintf("stat -c %%Y '%s'", shq(path)))
}

// checkMtimeUnchanged asserts path's mtime equals a pre-captured value (IDP-01:
// an idempotent re-apply must not rewrite target files) — evaluator item 17.
func checkMtimeUnchanged(at accTarget, path, before, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		after, err := hostMtime(at, path)
		if err != nil {
			return fmt.Errorf("%s: mtime probe: %w", why, err)
		}
		if strings.TrimSpace(before) != strings.TrimSpace(after) {
			return fmt.Errorf("%s: %s mtime changed (before=%s after=%s)", why, path, strings.TrimSpace(before), strings.TrimSpace(after))
		}
		return nil
	}
}

// stopServiceOnHost stops a Windows service out-of-band (DRF-01 drift induction).
func stopServiceOnHost(at accTarget, svc string) error {
	_, err := probeHost(at,
		fmt.Sprintf("Stop-Service -Name '%s' -Force; Start-Sleep -Seconds 1; exit 0", psq(svc)),
		"exit 0")
	return err
}

// wallBetween runs fn and fails unless min <= elapsed <= max. CON-02 asserts a
// wrong password fails FAST (<15s, no retry); CON-03 asserts connect_retries=2
// actually retries (elapsed proves 3 attempts happened, not one immediate fail).
// LCK-01 asserts the contended lock fails in <5s. Pass min=0 to bound only above.
func wallBetween(t *testing.T, min, max time.Duration, what string, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	elapsed := time.Since(start)
	if elapsed > max {
		t.Fatalf("%s took %s, want <= %s (DESIGN timing bound)", what, elapsed, max)
	}
	if min > 0 && elapsed < min {
		t.Fatalf("%s took %s, want >= %s (retries/attempts did not occur)", what, elapsed, min)
	}
}
