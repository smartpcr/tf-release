package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// checkWindowsEventLog asserts the System event log contains a Service Control
// Manager error (id 7000/7009/7011/7031) for the SampleSvc service raised AT OR
// AFTER `since` (the moment the failing apply began). Filtering by both the
// service name and the operation start time prevents an unrelated historical SCM
// failure from satisfying WSV-03 (DESIGN §18 WSV-03 "an event-log 7000/7009
// line") — evaluator item 10.
func checkWindowsEventLog(at accTarget, svc string, since time.Time, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		start := since.UTC().Format("2006-01-02T15:04:05Z")
		ps := fmt.Sprintf("$since=([datetimeoffset]'%s').LocalDateTime; "+
			"$e = Get-WinEvent -FilterHashtable @{LogName='System';Id=7000,7009,7011,7031;StartTime=$since} -ErrorAction SilentlyContinue | "+
			"Where-Object { $_.Message -match '%s' }; if ($e) { 'EVT_FOUND' } else { 'EVT_NONE' }", start, psq(svc))
		out, err := probeHost(at, ps, "echo EVT_NONE")
		if err != nil {
			return fmt.Errorf("%s: event-log probe: %w", why, err)
		}
		if !strings.Contains(out, "EVT_FOUND") {
			return fmt.Errorf("%s: no SCM 7000/7009/7011/7031 event naming %s found at/after %s", why, svc, start)
		}
		return nil
	}
}

// winswXMLPath is the WinSW config the provider writes: <current>\<svc>.winsw.xml
// (internal/pattern/windows_service.go writes `rc.P.Current\<svc>.winsw.xml`).
func winswXMLPath(at accTarget, svc string) string {
	cur := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", "").Current
	return cur + string(sep(at.tgt.OS)) + svc + ".winsw.xml"
}

// checkWinswXMLVersion asserts the regenerated WinSW service XML under `current`
// references the expected release version (WSV-06: xml regenerated on upgrade) —
// evaluator items 11, 13.
func checkWinswXMLVersion(at accTarget, version, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		xml := winswXMLPath(at, "SampleSvc")
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

// checkWinswStopwait asserts the WinSW xml carries <stopwait>Nsec</stopwait>
// matching the configured stop_timeout_seconds (WSV-06: "stopwait honored") —
// evaluator item 12.
func checkWinswStopwait(at accTarget, sec int, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		xml := winswXMLPath(at, "SampleSvc")
		out, err := hostReadFile(at, xml)
		if err != nil {
			return fmt.Errorf("%s: read winsw xml %s: %w", why, xml, err)
		}
		want := fmt.Sprintf("<stopwait>%dsec</stopwait>", sec)
		if !strings.Contains(out, want) {
			return fmt.Errorf("%s: winsw xml %s missing %s (stopwait not honored)", why, xml, want)
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

// checkServiceEnv asserts the service's per-service Environment (the MultiString
// under HKLM:\SYSTEM\CurrentControlSet\Services\<svc>\Environment that
// writeServiceEnv populates — NOT the machine-wide Session Manager environment)
// exposes name=value. This is the env-delivery contract DESIGN §18 WSV-01/NET-01
// require (LD_VERSION, ASPNETCORE_URLS, caller `environment:` vars) — item 7.
func checkServiceEnv(at accTarget, svc, name, value string) func(*terraform.State) error {
	const base = `HKLM:\SYSTEM\CurrentControlSet\Services\`
	return func(*terraform.State) error {
		key := base + svc
		out, err := probeHost(at,
			fmt.Sprintf("(Get-ItemProperty -Path '%s' -Name Environment -ErrorAction SilentlyContinue).Environment -join \"`n\"", psq(key)),
			"echo n/a")
		if err != nil {
			return fmt.Errorf("service %s env probe: %w", svc, err)
		}
		want := name + "=" + value
		if !containsEnvEntry(out, name, value) {
			return fmt.Errorf("service %s Environment missing %q (got %q)", svc, want, strings.TrimSpace(out))
		}
		return nil
	}
}

// containsEnvEntry reports whether any `NAME=...value...` line appears in a
// newline-joined MultiString environment block.
func containsEnvEntry(block, name, value string) bool {
	for _, ln := range strings.Split(block, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, name+"=") && strings.Contains(ln, value) {
			return true
		}
	}
	return false
}

// checkServiceStartMode asserts the service start type (WSV-01 AUTO_START ⇒
// Win32_Service.StartMode == "Auto") — evaluator item 8.
func checkServiceStartMode(at accTarget, svc, wantMode string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("(Get-CimInstance Win32_Service -Filter \"Name='%s'\").StartMode", psq(svc)),
			"echo n/a")
		if err != nil {
			return fmt.Errorf("service %s start-mode probe: %w", svc, err)
		}
		if !strings.EqualFold(strings.TrimSpace(out), wantMode) {
			return fmt.Errorf("service %s StartMode = %q, want %s", svc, strings.TrimSpace(out), wantMode)
		}
		return nil
	}
}

// checkServiceRecovery asserts the service has a RESTART recovery action
// configured (WSV-01 "recovery actions set"; the provider writes
// `sc.exe failure ... actions= restart/5000/...`) — evaluator item 8.
func checkServiceRecovery(at accTarget, svc, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at,
			fmt.Sprintf("& sc.exe qfailure '%s' | Out-String", psq(svc)),
			"echo n/a")
		if err != nil {
			return fmt.Errorf("%s: sc qfailure probe: %w", why, err)
		}
		if !strings.Contains(strings.ToUpper(out), "RESTART") {
			return fmt.Errorf("%s: service %s has no RESTART recovery action (sc qfailure=%q)", why, svc, strings.TrimSpace(out))
		}
		return nil
	}
}

// checkNoSecretInSCQC asserts the `sc qc` capture for a service does NOT contain
// the account password (WSV-08 "secret absent from sc qc capture") — evaluator
// item 14. The password is read from the same env var the spec references.
func checkNoSecretInSCQC(at accTarget, svc, secretEnv, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		secret := os.Getenv(secretEnv)
		if secret == "" {
			return fmt.Errorf("%s: %s not set (needed to prove secret absence)", why, secretEnv)
		}
		out, err := probeHost(at,
			fmt.Sprintf("& sc.exe qc '%s' | Out-String", psq(svc)),
			"echo n/a")
		if err != nil {
			return fmt.Errorf("%s: sc qc probe: %w", why, err)
		}
		if strings.Contains(out, secret) {
			return fmt.Errorf("%s: service %s sc qc output leaks the account password", why, svc)
		}
		return nil
	}
}

// snapshotTree returns a stable, sorted recursive fingerprint of every entry
// under root. For files it includes a SHA-256 CONTENT HASH (plus mtime + size);
// for directories it records the path. The content hash is what makes this a
// faithful "nothing changed" proof for an abandon destroy (DST-02): a same-size,
// same-mtime content rewrite still flips the hash, so it cannot slip through —
// evaluator item 12(b).
func snapshotTree(at accTarget, root string) (string, error) {
	win := "if(Test-Path -LiteralPath '" + psq(root) + "'){ Get-ChildItem -LiteralPath '" + psq(root) + "' -Recurse -Force | Sort-Object FullName | ForEach-Object { " +
		"if($_.PSIsContainer){ \"$($_.FullName)|dir\" } else { $h=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash; \"$($_.FullName)|$($_.LastWriteTimeUtc.Ticks)|$($_.Length)|$h\" } } | Out-String } else { 'MISSING' }"
	// Files: sha256 + path; directories: path|dir. Sorted for stability.
	lin := "{ find '" + shq(root) + "' -type f -print0 2>/dev/null | xargs -0 sha256sum 2>/dev/null; find '" + shq(root) + "' -type d -printf '%p|dir\\n' 2>/dev/null; } | sort"
	return probeHost(at, win, lin)
}

// tfLogCapture routes the provider's tflog output (the DESIGN §8.5 structured
// "deploy step" records, cache decisions, etc.) into a per-test TRACE log file so
// Check functions can assert which steps the engine actually emitted — the only
// faithful way to prove "no FETCH/SWITCH" (DRF), FORCE_KILL escalation (WSV-07),
// cache_hit (RBK), and secret redaction (WSV-08). Terraform forwards provider
// logs to TF_LOG_PATH when TF_LOG(_PROVIDER)=TRACE. Returns the log path.
func tfLogCapture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tf-trace.log")
	t.Setenv("TF_LOG", "TRACE")
	t.Setenv("TF_LOG_PROVIDER", "TRACE")
	t.Setenv("TF_LOG_PATH", p)
	return p
}

// truncateTFLog empties the capture file so the NEXT apply's records are the only
// ones present — used in a step PreConfig to scope a step-log assertion to a
// single apply.
func truncateTFLog(t *testing.T, path string) func() {
	return func() { _ = os.WriteFile(path, nil, 0o600) }
}

// readTFLogLines returns the non-empty lines of the capture file.
func readTFLogLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out, nil
}

// stepLineRe matches a provider "deploy step" TRACE record carrying step=<NAME>
// (tflog renders fields as key=value on the message line).
func stepLineRe(step string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)deploy step.*\bstep=` + regexp.QuoteMeta(step) + `\b`)
}

// haveAnyStepLog proves the TRACE capture actually worked: at least one "deploy
// step" record must be present. Without this guard an EMPTY/uncaptured log would
// make every "step absent" assertion pass vacuously (the WSV-02 failure mode the
// evaluator flagged) — so absence checks call this first.
func haveAnyStepLog(lines []string) bool {
	for _, ln := range lines {
		if strings.Contains(strings.ToLower(ln), "deploy step") {
			return true
		}
	}
	return false
}

// checkTFLogMatches asserts the provider TRACE log matches re (used for WARN
// diagnostics that Terraform surfaces into its logs but that terraform-plugin-
// testing exposes no Check hook for, e.g. the LCK-02 stale-lock override notice).
func checkTFLogMatches(path string, re *regexp.Regexp, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		if len(b) == 0 {
			return fmt.Errorf("%s: TRACE log empty; expected a match for %s", why, re.String())
		}
		if !re.Match(b) {
			return fmt.Errorf("%s: TRACE log has no match for %s", why, re.String())
		}
		return nil
	}
}
func checkStepLogged(path, step, why string) func(*terraform.State) error {
	re := stepLineRe(step)
	return func(*terraform.State) error {
		lines, err := readTFLogLines(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		for _, ln := range lines {
			if re.MatchString(ln) {
				return nil
			}
		}
		return fmt.Errorf("%s: no structured step=%s record in the provider TRACE log", why, step)
	}
}

// checkStepNotLogged asserts the engine did NOT emit step=<step>, but only after
// proving the capture worked (≥1 deploy-step record exists) so an empty log can't
// pass vacuously.
func checkStepNotLogged(path, step, why string) func(*terraform.State) error {
	re := stepLineRe(step)
	return func(*terraform.State) error {
		lines, err := readTFLogLines(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		if !haveAnyStepLog(lines) {
			return fmt.Errorf("%s: no deploy-step records captured; cannot prove step=%s absence (log empty/uncaptured)", why, step)
		}
		for _, ln := range lines {
			if re.MatchString(ln) {
				return fmt.Errorf("%s: engine emitted a forbidden step=%s record", why, step)
			}
		}
		return nil
	}
}

// checkCacheHitLogged proves cache_hit=true the way the provider actually
// expresses it (engine.go: no FETCH step + the "release cached; skipping
// fetch/extract" record) — evaluator item 8. DESIGN's `cache_hit=true` field is
// realized as this step-skip signal.
func checkCacheHitLogged(path, why string) func(*terraform.State) error {
	fetch := stepLineRe("FETCH")
	return func(*terraform.State) error {
		lines, err := readTFLogLines(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		if !haveAnyStepLog(lines) {
			return fmt.Errorf("%s: no deploy-step records captured; cannot prove cache hit", why)
		}
		cached := false
		for _, ln := range lines {
			if fetch.MatchString(ln) {
				return fmt.Errorf("%s: a FETCH step was emitted — the release was NOT served from cache", why)
			}
			if strings.Contains(strings.ToLower(ln), "release cached") {
				cached = true
			}
		}
		if !cached {
			return fmt.Errorf("%s: expected the 'release cached; skipping fetch/extract' record (cache_hit=true)", why)
		}
		return nil
	}
}

// checkRefetchLogged proves cache_hit=false: a FETCH step WAS emitted (the pruned
// release had to be re-downloaded) and the cache-hit record is absent — item 9.
func checkRefetchLogged(path, why string) func(*terraform.State) error {
	fetch := stepLineRe("FETCH")
	return func(*terraform.State) error {
		lines, err := readTFLogLines(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		for _, ln := range lines {
			if fetch.MatchString(ln) {
				return nil
			}
		}
		return fmt.Errorf("%s: expected a FETCH step (re-fetch / cache_hit=false) but none was logged", why)
	}
}

// checkSecretAbsentInLog asserts the account password VALUE never appears in the
// provider's TRACE log — the realizable proof of DESIGN §18 WSV-08 "secret absent
// from the sc qc capture in TF logs at TRACE": secrets are only ever carried in
// the base64 env blob, never in any logged command text — evaluator item 7.
func checkSecretAbsentInLog(path, secretEnv, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		secret := os.Getenv(secretEnv)
		if secret == "" {
			return fmt.Errorf("%s: %s not set (needed to prove secret absence in logs)", why, secretEnv)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: read TRACE log: %w", why, err)
		}
		if len(b) == 0 {
			return fmt.Errorf("%s: TRACE log empty; cannot prove redaction", why)
		}
		if strings.Contains(string(b), secret) {
			return fmt.Errorf("%s: the account password leaked into the provider TRACE log", why)
		}
		return nil
	}
}

// checkTreeUnchanged asserts the recursive fingerprint of root equals `before`.
func checkTreeUnchanged(at accTarget, root, before, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		now, err := snapshotTree(at, root)
		if err != nil {
			return fmt.Errorf("%s: tree snapshot: %w", why, err)
		}
		if strings.TrimSpace(now) != strings.TrimSpace(before) {
			return fmt.Errorf("%s: the tree under %s was mutated during destroy (abandon must touch nothing)", why, root)
		}
		return nil
	}
}

// startDowntimeProbe launches a DETACHED 1 Hz health poller on W1 that appends
// OK/FAIL to ldDowntimeLog for durSec seconds, so a concurrent upgrade's health
// outage can be measured from W1 itself (WSV-02 "downtime window, 1s-poll curl
// from W1") — evaluator item 9. Fire-and-forget: it runs during the apply that
// follows this PreConfig.
const ldDowntimeLog = `C:\Windows\Temp\ld_downtime.log`

func startDowntimeProbe(at accTarget, url string, durSec int) error {
	const scriptPath = `C:\Windows\Temp\ld_downtime.ps1`
	body := fmt.Sprintf("$end=(Get-Date).AddSeconds(%d); Remove-Item -LiteralPath '%s' -ErrorAction SilentlyContinue; "+
		"while((Get-Date) -lt $end){ try{ $c=(Invoke-WebRequest -UseBasicParsing -TimeoutSec 1 -Uri '%s').StatusCode }catch{ $c=0 }; "+
		"if($c -eq 200){ Add-Content -LiteralPath '%s' -Value 'OK' }else{ Add-Content -LiteralPath '%s' -Value 'FAIL' }; Start-Sleep -Seconds 1 }",
		durSec, ldDowntimeLog, psq(url), ldDowntimeLog, ldDowntimeLog)
	cmd := fmt.Sprintf("Set-Content -LiteralPath '%s' -Value '%s' -Encoding ASCII; "+
		"Start-Process -FilePath 'powershell.exe' -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','%s' -WindowStyle Hidden; 'STARTED'",
		scriptPath, psq(body), scriptPath)
	_, err := probeHost(at, cmd, "true")
	return err
}

// checkDowntimeBounded reads the poller log and asserts (1) enough samples were
// captured to be meaningful, (2) the service was HEALTHY before the outage and
// RECOVERED after it — so a truncated/empty capture with maxRun=0 can never pass
// vacuously — and (3) the longest run of consecutive FAIL samples (≈ seconds of
// outage) does not exceed maxSec (DESIGN §18 WSV-02 ≤ stop_timeout+15s) —
// evaluator item 3.
func checkDowntimeBounded(at accTarget, maxSec int, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := hostReadFile(at, ldDowntimeLog)
		if err != nil {
			return fmt.Errorf("%s: read downtime log: %w", why, err)
		}
		samples := strings.Fields(out)
		if len(samples) < 5 {
			return fmt.Errorf("%s: only %d downtime samples captured; the 1 Hz probe did not run across the upgrade", why, len(samples))
		}
		isFail := func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "FAIL") }
		maxRun, cur := 0, 0
		firstFail, lastFail, okBefore := -1, -1, false
		for i, s := range samples {
			if isFail(s) {
				if firstFail < 0 {
					firstFail = i
				}
				lastFail = i
				cur++
				if cur > maxRun {
					maxRun = cur
				}
			} else {
				cur = 0
				if firstFail < 0 {
					okBefore = true
				}
			}
		}
		if firstFail >= 0 {
			// There WAS an outage: require a healthy sample before it and recovery after.
			if !okBefore {
				return fmt.Errorf("%s: no successful health sample BEFORE the outage — capture is partial, cannot bound downtime", why)
			}
			okAfter := false
			for i := lastFail + 1; i < len(samples); i++ {
				if !isFail(samples[i]) {
					okAfter = true
				}
			}
			if !okAfter {
				return fmt.Errorf("%s: service never returned healthy after the outage (last %d samples still FAIL)", why, len(samples)-lastFail-1)
			}
		}
		if maxRun > maxSec {
			return fmt.Errorf("%s: health downtime %ds exceeds bound %ds", why, maxRun, maxSec)
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

// wallSinceBetween returns a check asserting the wall time since *t0 is within
// [min,max]. Used by WSV-06 to prove a winsw <stopwait> was HONORED: against a
// slow-stop build the upgrade's STOP phase must take AT LEAST most of stopwait
// (it actually waited, not stopped instantly) and at most stopwait+margin (winsw
// force-killed at the deadline rather than hanging).
func wallSinceBetween(t0 *time.Time, min, max time.Duration, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		d := time.Since(*t0)
		if d > max {
			return fmt.Errorf("%s: took %s, want <= %s", why, d, max)
		}
		if d < min {
			return fmt.Errorf("%s: took %s, want >= %s (stopwait was not honored — stop returned too early)", why, d, min)
		}
		return nil
	}
}
// wallSince returns a check asserting the wall time elapsed since *t0 is ≤ max.
// The caller sets *t0 in the step's PreConfig (which runs immediately before the
// apply) so the Check (which runs immediately after) bounds that single apply's
// duration — used by WSV-07 to bound the STOP/force-kill phase.
func wallSince(t0 *time.Time, max time.Duration, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		if d := time.Since(*t0); d > max {
			return fmt.Errorf("%s: took %s, want <= %s", why, d, max)
		}
		return nil
	}
}
