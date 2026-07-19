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
	sha := artifactSHA(t, version)
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

// checkReleaseMarker asserts releases/<version>/.labdeploy-release.json exists
// on the target (ART-01: the runner-push marker records the verified sha).
func checkReleaseMarker(at accTarget, version string) func(*terraform.State) error {
	return func(*terraform.State) error {
		p := layout.NewPaths(at.tgt.OS, installRootFor(at), "sample-svc", version)
		marker := p.Release + string(sep(at.tgt.OS)) + ".labdeploy-release.json"
		out, err := probeHost(at,
			fmt.Sprintf("if (Test-Path -LiteralPath '%s') { 'FOUND' } else { 'MISSING' }", strings.ReplaceAll(marker, "'", "''")),
			fmt.Sprintf("if [ -e '%s' ]; then echo FOUND; else echo MISSING; fi", strings.ReplaceAll(marker, "'", `'\''`)))
		if err != nil {
			return fmt.Errorf("probe release marker: %w", err)
		}
		if !strings.Contains(out, "FOUND") {
			return fmt.Errorf("release marker %s not found on %s", marker, at.host)
		}
		return nil
	}
}

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

// probeHost connects to the (single-host) target and runs one shell script,
// returning stdout. Used by scenario checks that must observe on-target state
// (junction/symlink targets, marker files) beyond the Terraform attributes.
func probeHost(at accTarget, winScript, shScript string) (string, error) {
	tr, err := transport.NewTransport(&at.tgt, at.host)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := tr.Connect(ctx); err != nil {
		return "", err
	}
	defer tr.Close()
	var cmd transport.Cmd
	if at.tgt.OS == spec.OSWindows {
		cmd = transport.Cmd{Shell: transport.ShellPowerShell, Script: winScript}
	} else {
		cmd = transport.Cmd{Shell: transport.ShellSh, Script: shScript}
	}
	res, err := tr.Exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
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
// for a given sample-svc version, read from LABDEPLOY_ACC_SHA_<VER-normalized>.
// Under TF_ACC=1 a missing checksum is a FAILURE — a scenario cannot silently
// pass with an unverifiable artifact.
func artifactSHA(t *testing.T, version string) string {
	t.Helper()
	name := "LABDEPLOY_ACC_SHA_" + strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToUpper(version))
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("TF_ACC=1 requires %s (the sha256 of sample-svc %s)", name, version)
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
	sha := artifactSHA(t, version)
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
	sha := artifactSHA(t, version)
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
	sha := artifactSHA(t, version)
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
	sha := artifactSHA(t, version)
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
	sha := artifactSHA(t, version)
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
