package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// fakeRunnerTransport drives the WHOLE RunTest flow in-process (DESIGN §17 seam):
// it answers the staging/marker probes so the cached fast-path is taken, records
// the runner watchdog script, returns a SCRIPTED runner Result, and serves the
// on-target results archive (a real .tar.gz) back through Download so collection
// extracts a real results tree. No network, no external service.
type fakeRunnerTransport struct {
	osKind      spec.OSKind
	host        string
	markerJSON  string // returned for the release-marker read ⇒ cached=true
	runnerRes   transport.Result
	runnerErr   error
	archiveSrc  string   // local file whose bytes are served as the results archive
	runnerCmds  []string // recorded runner watchdog scripts
	execScripts []string // every script executed (for assertions)
}

func (f *fakeRunnerTransport) Connect(ctx context.Context) error { return nil }
func (f *fakeRunnerTransport) Close() error                      { return nil }
func (f *fakeRunnerTransport) OS() spec.OSKind                   { return f.osKind }
func (f *fakeRunnerTransport) Host() string                      { return f.host }

func (f *fakeRunnerTransport) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	f.execScripts = append(f.execScripts, c.Script)
	switch {
	case strings.Contains(c.Script, releaseMarker) && strings.Contains(c.Script, "base64"):
		// release-marker read ⇒ report the wanted version is already extracted.
		return transport.Result{ExitCode: 0, Stdout: base64.StdEncoding.EncodeToString([]byte(f.markerJSON))}, nil
	case strings.Contains(c.Script, "kill -9 -$__pgid") || strings.Contains(c.Script, "taskkill /PID"):
		// The runner watchdog script — the process-tree kill wrapper.
		f.runnerCmds = append(f.runnerCmds, c.Script)
		return f.runnerRes, f.runnerErr
	case strings.Contains(c.Script, "labdeploy-logs-"):
		// The collection zip/tar probe: report files present (not NOFILES) so
		// CollectFiles proceeds to Download.
		return transport.Result{ExitCode: 0, Stdout: "packed"}, nil
	default:
		// mkdir layout / wipe staging / etc.
		return transport.Result{ExitCode: 0}, nil
	}
}

func (f *fakeRunnerTransport) Upload(ctx context.Context, rd io.Reader, size int64, remote string) error {
	return nil
}

func (f *fakeRunnerTransport) Download(ctx context.Context, remote, local string) error {
	if strings.Contains(remote, "labdeploy-logs-") && f.archiveSrc != "" {
		src, err := os.Open(f.archiveSrc)
		if err != nil {
			return err
		}
		defer src.Close()
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		dst, err := os.Create(local)
		if err != nil {
			return err
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		return err
	}
	return nil
}

var _ transport.Transport = (*fakeRunnerTransport)(nil)

// makeTarGz writes a single-entry .tar.gz at dst carrying `content` at the given
// in-archive path, mimicking the on-target results archive the runner produced.
func makeTarGz(t *testing.T, dst, arcName string, content []byte) {
	t.Helper()
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: arcName, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// e2eTestRun builds a minimal, valid cached-path TestRun on a linux target whose
// results land in destDir. timeoutSec drives the watchdog wrapper.
func e2eTestRun(destDir string, timeoutSec int) *spec.TestRun {
	return &spec.TestRun{
		APIVersion: "labdeploy/v1", Kind: "TestRun",
		Metadata: spec.Metadata{Name: "smoke"},
		Target: spec.Target{OS: spec.OSLinux, Hosts: []string{"lab-01"},
			Transport: spec.TransportSSH},
		Artifact: spec.Artifact{Version: "1.2.3", Checksum: "sha256:abc"},
		Runner:   spec.Runner{Type: "exec", Command: "./run-tests.sh", TimeoutSeconds: timeoutSec},
		Results:  spec.Results{Format: "junit", Paths: []string{"TestResults/*.xml"}},
		Collect:  spec.Collect{DestinationDir: destDir},
	}
}

func cachedMarker() string {
	b, _ := json.Marshal(map[string]string{"version": "1.2.3", "sha256": "sha256:abc", "extracted_at": "2024-01-01T00:00:00Z"})
	return string(b)
}

// TestRunTestCollectionBeforeFailure proves the "Collection before failure"
// scenario (DESIGN §5.3; E2E-05): against a fake transport serving the on-target
// results archive, RunTest fully materializes the local results_dir — the
// extracted results tree is BYTE-IDENTICAL to the committed golden snapshot AND
// summary.json is written — and the failing outcome is RETURNED (not lost) so the
// provider can gate ERR_TEST_FAILED after the tree is persisted.
func TestRunTestCollectionBeforeFailure(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "e2e_results", "TestResults", "results.xml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	dest := t.TempDir()
	archive := filepath.Join(t.TempDir(), "results.tar.gz")
	makeTarGz(t, archive, "TestResults/results.xml", golden)

	ft := &fakeRunnerTransport{
		osKind: spec.OSLinux, host: "lab-01", markerJSON: cachedMarker(),
		runnerRes:  transport.Result{ExitCode: 1}, // tests failed
		archiveSrc: archive,
	}
	e := New()
	e.NewTransport = func(_ *spec.Target, _ string) (transport.Transport, error) { return ft, nil }

	out, rerr := e.RunTest(context.Background(), e2eTestRun(dest, 600))
	if rerr != nil {
		t.Fatalf("RunTest returned error (results should be collected and outcome returned): %v", rerr)
	}
	if out == nil {
		t.Fatal("RunTest returned nil outcome")
	}
	if out.Passed {
		t.Fatal("outcome.Passed = true, want false (1 failure at min_pass_rate=1.0)")
	}
	if out.Total != 4 || out.FailedTests != 1 || out.SkippedTests != 1 || out.PassedTests != 2 {
		t.Fatalf("counters = total=%d passed=%d failed=%d skipped=%d, want 4/2/1/1",
			out.Total, out.PassedTests, out.FailedTests, out.SkippedTests)
	}

	// The extracted results tree must be byte-identical to the golden snapshot.
	extracted := findFile(t, filepath.Join(dest, "results"), "results.xml")
	got, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatalf("read extracted results: %v", err)
	}
	if string(got) != string(golden) {
		t.Fatalf("extracted results tree is NOT byte-identical to golden\n--- extracted ---\n%s\n--- golden ---\n%s", got, golden)
	}

	// summary.json must be fully written into the results_dir before return.
	summaryRaw, err := os.ReadFile(filepath.Join(dest, "summary.json"))
	if err != nil {
		t.Fatalf("summary.json not written: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(summaryRaw, &doc); err != nil {
		t.Fatalf("summary.json invalid: %v", err)
	}
	if doc["passed"] != false {
		t.Errorf("summary.json passed = %v, want false", doc["passed"])
	}
	if doc["total"].(float64) != 4 {
		t.Errorf("summary.json total = %v, want 4", doc["total"])
	}
}

// TestRunTestTimeoutKillsTree proves the "Timeout kills tree" scenario (DESIGN
// §5.3; E2E-04): when the runner exec reports a timeout (scripted sentinel), the
// engine (a) returns ERR_TIMEOUT and (b) the script it issued to the transport
// force-kills the whole process TREE (kill -9 -<pgid> on linux). Collection still
// runs first so partial results are captured.
func TestRunTestTimeoutKillsTree(t *testing.T) {
	dest := t.TempDir()
	ft := &fakeRunnerTransport{
		osKind: spec.OSLinux, host: "lab-01", markerJSON: cachedMarker(),
		// Scripted timeout: sentinel exit code + marker, no transport error.
		runnerRes: transport.Result{ExitCode: timeoutSentinel, Stdout: timeoutMarker},
	}
	e := New()
	e.NewTransport = func(_ *spec.Target, _ string) (transport.Transport, error) { return ft, nil }

	out, rerr := e.RunTest(context.Background(), e2eTestRun(dest, 30))
	if out != nil {
		t.Fatalf("timeout must yield a nil outcome, got %+v", out)
	}
	if rerr == nil || !strings.Contains(rerr.Error(), "ERR_TIMEOUT") {
		t.Fatalf("want ERR_TIMEOUT, got %v", rerr)
	}
	if len(ft.runnerCmds) != 1 {
		t.Fatalf("expected exactly one runner script issued, got %d", len(ft.runnerCmds))
	}
	if !strings.Contains(ft.runnerCmds[0], "kill -9 -$__pgid") {
		t.Fatalf("runner script does not issue a process-tree kill:\n%s", ft.runnerCmds[0])
	}
	if !strings.Contains(ft.runnerCmds[0], "setsid") {
		t.Fatalf("runner script does not create a new process group (setsid):\n%s", ft.runnerCmds[0])
	}
	// Best-effort collection ran before the timeout was surfaced: stdout/stderr
	// tails are always captured (DESIGN §7.4).
	if _, err := os.Stat(filepath.Join(dest, "runner-stdout.txt")); err != nil {
		t.Fatalf("collection did not run before timeout surfaced: %v", err)
	}
}

// TestRunnerScriptWindowsKillsTree locks the Windows watchdog issuing a
// full-tree taskkill (/T) force-kill (/F) on timeout.
func TestRunnerScriptWindowsKillsTree(t *testing.T) {
	s := runnerScriptWindows(`C:\deploy\smoke-tests\releases\1.2.3`, "vstest.console.exe tests.dll", 120)
	for _, want := range []string{"taskkill /PID", "/T /F", timeoutMarker} {
		if !strings.Contains(s, want) {
			t.Fatalf("windows watchdog missing %q:\n%s", want, s)
		}
	}
	// timeout<=0 ⇒ unwrapped (no watchdog).
	if strings.Contains(runnerScriptWindows("wd", "cmd", 0), "taskkill") {
		t.Fatal("timeout<=0 must not wrap the command in a watchdog")
	}
}

// TestIsTimeoutErrRecognizesTransports proves ERR_TIMEOUT recognition is reliable
// across the ssh ("timed out"), winrm/local (context deadline) and killed-signal
// transport error shapes — the prior substring-only check missed the latter two.
func TestIsTimeoutErrRecognizesTransports(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ssh timed out", errString("exec timed out after 30s"), true},
		{"winrm/local deadline", context.DeadlineExceeded, true},
		{"killed signal", errString("local exec: signal: killed"), true},
		{"ordinary failure", errString("connection refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isTimeoutErr(c.err); got != c.want {
			t.Errorf("%s: isTimeoutErr = %v, want %v", c.name, got, c.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestDeleteTestDirRemovesWorkspace proves best-effort Delete (DESIGN §5.3):
// DeleteTestDir issues a RECURSIVE removal of the remote test workspace
// (<install_root>/<name>-tests) and returns nil on success.
func TestDeleteTestDirRemovesWorkspace(t *testing.T) {
	ft := &fakeRunnerTransport{osKind: spec.OSLinux, host: "lab-01"}
	e := New()
	e.NewTransport = func(_ *spec.Target, _ string) (transport.Transport, error) { return ft, nil }

	if err := e.DeleteTestDir(context.Background(), e2eTestRun("dest", 0)); err != nil {
		t.Fatalf("DeleteTestDir returned error: %v", err)
	}
	var removed bool
	for _, s := range ft.execScripts {
		if strings.Contains(s, "rm -rf") && strings.Contains(s, "/opt/deploy/smoke-tests") {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("DeleteTestDir did not issue a recursive remove of the test workspace; scripts:\n%s",
			strings.Join(ft.execScripts, "\n---\n"))
	}
}

// findFile returns the first file named `name` under root (fatal if none).
func findFile(t *testing.T, root, name string) string {
	t.Helper()
	var found string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && info.Name() == name {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("file %q not found under %s", name, root)
	}
	return found
}
