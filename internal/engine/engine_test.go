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
	current    string          // junction target
	svc        string          // "", "Stopped", "Running"
	fail       map[string]bool // step toggles: "switch","health","start","extract"
	healthGate func() bool     // optional dynamic health failure (true => fail)
	log        []string        // executed step markers, in order
}

func newFakeHost(name string) *fakeHost {
	return &fakeHost{host: name, files: map[string][]byte{}, dirs: map[string]bool{}, dirTS: map[string]int64{}, fail: map[string]bool{}}
}

func (f *fakeHost) mark(s string) { f.log = append(f.log, s) }

func (f *fakeHost) Connect(ctx context.Context) error { return nil }
func (f *fakeHost) Close() error                      { return nil }
func (f *fakeHost) OS() spec.OSKind                   { return spec.OSWindows }
func (f *fakeHost) Host() string                      { return f.host }

var reRead = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
var reWrite = regexp.MustCompile(`WriteAllBytes\('([^']+)',\[Convert\]::FromBase64String\('([^']*)'\)\)`)
var reLock = regexp.MustCompile(`\[IO\.File\]::Open\('([^']+)','CreateNew'\)`)
var reRmRecurse = regexp.MustCompile(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue '([^']+)'`)
var reRmOne = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)
var reMklink = regexp.MustCompile(`mklink /J "([^"]+)" "([^"]+)"`)
var reList = regexp.MustCompile(`Get-ChildItem -Directory '([^']+)'`)

func (f *fakeHost) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "$PSVersionTable"): // preflight
		f.mark("PREFLIGHT")
		return ok(""), nil

	case reLock.MatchString(s): // AcquireLock CreateNew
		p := reLock.FindStringSubmatch(s)[1]
		if _, held := f.files[p]; held {
			return transport.Result{ExitCode: 48}, nil
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
		raw, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return transport.Result{ExitCode: 1, Stderr: "b64"}, nil
		}
		f.files[m[1]] = raw
		return ok(""), nil

	case reRead.MatchString(s): // readSmallFile
		p := reRead.FindStringSubmatch(s)[1]
		raw, exists := f.files[p]
		if !exists {
			return transport.Result{ExitCode: 3}, nil
		}
		return ok(base64.StdEncoding.EncodeToString(raw)), nil

	case strings.Contains(s, "New-Item -ItemType Directory"): // ensureLayout/ensureDir
		return ok(""), nil

	case strings.Contains(s, "Expand-Archive"): // extract
		if f.fail["extract"] {
			return transport.Result{ExitCode: 1, Stderr: "corrupt zip"}, nil
		}
		f.mark("EXTRACT")
		return ok(""), nil

	case reMklink.MatchString(s): // switchJunction
		if f.fail["switch"] {
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		m := reMklink.FindStringSubmatch(s)
		f.current = m[2]
		f.mark("SWITCH->" + m[2])
		return ok(""), nil

	case strings.Contains(s, "sc.exe create") || strings.Contains(s, "sc.exe config"): // Configure
		f.mark("CONFIGURE")
		if f.svc == "" {
			f.svc = "Stopped"
		}
		return ok(""), nil

	case strings.Contains(s, "Stop-Service"): // Stop
		f.mark("STOP")
		if f.svc == "Running" {
			f.svc = "Stopped"
		}
		return ok(""), nil

	case strings.Contains(s, "Start-Service"): // Start
		if f.fail["start"] {
			return transport.Result{ExitCode: 44, Stderr: "start timeout"}, nil
		}
		f.mark("START")
		f.svc = "Running"
		return ok(""), nil

	case strings.Contains(s, "exit 41"): // target-pull fetch script (checksum marker)
		f.mark("FETCH")
		return ok(""), nil

	case strings.Contains(s, "Invoke-WebRequest"): // health http
		if f.fail["health"] || (f.healthGate != nil && f.healthGate()) {
			return transport.Result{ExitCode: 1, Stdout: "connection refused"}, nil
		}
		f.mark("HEALTH")
		return ok(""), nil

	case strings.Contains(s, "not_installed"): // Status probe
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
		p := reRmRecurse.FindStringSubmatch(s)[1]
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
  initial_delay_seconds: 0
  interval_seconds: 1
  timeout_seconds: 2
strategy: { keep_releases: 2, rollback_on_failure: true }
`, checksum, url)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
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
	// Newest→oldest. previous (1.0.0) is intentionally older than the kept
	// non-protected 1.1.0 to prove previous is never pruned by age.
	f.dirs = map[string]bool{"1.2.0": true, "1.1.0": true, "1.0.0": true, "0.9.0": true}
	f.dirTS = map[string]int64{"1.2.0": 400, "1.1.0": 300, "1.0.0": 200, "0.9.0": 100}

	m := &Manifest{CurrentVersion: "1.2.0", PreviousVersion: "1.0.0"}
	if err := eng.pruneReleases(context.Background(), f, d, p, m); err != nil {
		t.Fatalf("prune: %v", err)
	}

	joined := strings.Join(f.log, ">")
	oldest := `RM C:\deploy\sample-svc\releases\0.9.0`
	if !strings.Contains(joined, oldest) {
		t.Fatalf("oldest release must be pruned; log=%v", f.log)
	}
	for _, keep := range []string{"1.2.0", "1.1.0", "1.0.0"} {
		if strings.Contains(joined, `RM C:\deploy\sample-svc\releases\`+keep) {
			t.Fatalf("release %s must be retained (current/previous/kept), log=%v", keep, f.log)
		}
	}
}

