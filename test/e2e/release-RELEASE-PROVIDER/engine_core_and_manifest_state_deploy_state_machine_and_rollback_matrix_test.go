//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 3.5 (Deploy State
// Machine and Rollback Matrix).
//
// All four scenarios exercise the REAL internal/engine Deploy state machine
// in-process (setup: inline) against a fake, script-dispatching Transport that
// emulates just enough of the PowerShell surface the engine emits. No external
// service is required:
//
//   - Rollback matrix row -> a fake transport scripts a failure at each mutating
//     DESIGN §10.2 step; after the REAL engine.Deploy runs, the on-target state
//     (files/service/junction/manifest recorded by the fake) must match the
//     matrix row: a fresh pre-switch failure leaves NOTHING and wipes staging, a
//     fresh switch-phase failure leaves the machine CLEAN, an update pre-switch
//     failure keeps the previous release intact (staging wiped), and an update
//     switch-phase failure ROLLS BACK to the previous version (proof: in-process).
//   - Idempotent short-circuit -> a seeded manifest whose version+checksum equal
//     the spec plus a healthy service makes Deploy return a NO-OP with no FETCH
//     or SWITCH step emitted through the tflog sink.
//   - Single-host step order logged -> a successful fresh single-host Deploy is
//     captured through the tflog test sink; the emitted step values appear in the
//     fixed VALIDATE..UNLOCK order and every record carries app/host/step/version
//     and a numeric duration_ms.
//   - Conditional and per-host steps logged -> a genuine 2-node cluster update
//     where one node has the release cached (skips FETCH/CHECKSUM) and a health
//     failure drives a rollback that REPEATS the SWITCH step; every per-node and
//     repeated record still carries the full field set.
//
// Every Given/When/Then invokes the real engine code and asserts on its result.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// dsmReleaseMarker mirrors the engine's per-release completion marker filename
// (internal/engine releaseMarker). The e2e package cannot see the unexported
// const, so the cached-release scenario reproduces it here.
const dsmReleaseMarker = ".labdeploy-release.json"

const dsmManifestPath = `C:\deploy\sample-svc\manifest.json`

// ensureDeployPassword sets the credential env var the spec fixtures reference
// (target.credentials.password_env: LABDEPLOY_PASSWORD) so spec.ParseDeployment
// resolves it during scenario setup.
func ensureDeployPassword() error {
	return os.Setenv("LABDEPLOY_PASSWORD", "pw")
}

// ----------------------------------------------------------------------------
// dsmHost: a script-dispatching virtual Windows target (DESIGN §17 "fake
// transport"). It emulates just enough of the PS surface the engine emits so
// the REAL Deploy state machine runs end-to-end in-process. Faithfully ported
// from the engine package's fakeHost so the e2e suite drives production code.
// ----------------------------------------------------------------------------

type dsmHost struct {
	host       string
	files      map[string][]byte
	dirs       map[string]bool
	current    string          // junction target
	svc        string          // "", "Stopped", "Running"
	fail       map[string]bool // step toggles
	failN      map[string]int  // one-shot step failures: fail the first N calls, then succeed
	healthGate func() bool     // optional dynamic health failure (true => fail)
	log        []string
}

func dsmNewHost(name string) *dsmHost {
	return &dsmHost{
		host:  name,
		files: map[string][]byte{},
		dirs:  map[string]bool{},
		fail:  map[string]bool{},
		failN: map[string]int{},
	}
}

func (f *dsmHost) mark(s string) { f.log = append(f.log, s) }

func (f *dsmHost) Connect(ctx context.Context) error {
	if f.fail["connect"] {
		return fmt.Errorf("simulated connect failure")
	}
	return nil
}
func (f *dsmHost) Close() error    { return nil }
func (f *dsmHost) OS() spec.OSKind { return spec.OSWindows }
func (f *dsmHost) Host() string    { return f.host }

var (
	dsmReRead       = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
	dsmReWrite      = regexp.MustCompile(`WriteAllBytes\('([^']+)',\[Convert\]::FromBase64String\('([^']*)'\)\)`)
	dsmReLock       = regexp.MustCompile(`\[IO\.File\]::Move\(\$tmp,'([^']+)'\)`)
	dsmReRmRecurse  = regexp.MustCompile(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue '([^']+)'`)
	dsmReRmOne      = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)
	dsmReMklink     = regexp.MustCompile(`mklink /J "([^"]+)" "([^"]+)"`)
	dsmReList       = regexp.MustCompile(`Get-ChildItem -Directory '([^']+)'`)
	dsmReCasDel     = regexp.MustCompile(`\[IO\.File\]::Delete\('([^']+)'\)`)
	dsmReCurEq      = regexp.MustCompile(`\$cur -eq '([^']*)'`)
	dsmReCasReplace = regexp.MustCompile(`\[IO\.File\]::Replace\(\$tmp,'([^']+)',`)
	dsmReCurNe      = regexp.MustCompile(`\$cur -ne '([^']*)'`)
	dsmReB64        = regexp.MustCompile(`FromBase64String\('([^']*)'\)`)
)

func (f *dsmHost) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "$PSVersionTable"): // preflight
		if f.fail["preflight"] {
			return transport.Result{ExitCode: 1, Stderr: "preflight gate failed"}, nil
		}
		f.mark("PREFLIGHT")
		return dsmOK(""), nil

	case dsmReCasReplace.MatchString(s): // casReplace atomic compare-and-replace
		path := dsmReCasReplace.FindStringSubmatch(s)[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil
		}
		em := dsmReCurNe.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil
		}
		if nm := dsmReB64.FindStringSubmatch(s); nm != nil {
			raw, _ := base64.StdEncoding.DecodeString(nm[1])
			f.files[path] = raw
		}
		f.mark("LOCK")
		return dsmOK(""), nil

	case dsmReCasDel.MatchString(s): // casDelete compare-and-delete
		path := dsmReCasDel.FindStringSubmatch(s)[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil
		}
		em := dsmReCurEq.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil
		}
		delete(f.files, path)
		return dsmOK(""), nil

	case dsmReLock.MatchString(s): // AcquireLock atomic create
		if f.fail["lock"] {
			return transport.Result{}, fmt.Errorf("simulated lock acquire transport failure")
		}
		p := dsmReLock.FindStringSubmatch(s)[1]
		if _, held := f.files[p]; held {
			return transport.Result{ExitCode: 48}, nil
		}
		if m := dsmReB64.FindStringSubmatch(s); m != nil {
			raw, _ := base64.StdEncoding.DecodeString(m[1])
			f.files[p] = raw
		} else {
			f.files[p] = []byte("{}")
		}
		f.mark("LOCK")
		return dsmOK(""), nil

	case dsmReWrite.MatchString(s): // writeSmallFile
		m := dsmReWrite.FindStringSubmatch(s)
		if f.fail["render"] && strings.Contains(m[1], "render.conf") {
			return transport.Result{ExitCode: 1, Stderr: "render write failed"}, nil
		}
		raw, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return transport.Result{ExitCode: 1, Stderr: "b64"}, nil
		}
		f.files[m[1]] = raw
		return dsmOK(""), nil

	case dsmReRead.MatchString(s): // readSmallFile
		p := dsmReRead.FindStringSubmatch(s)[1]
		if f.fail["read"] {
			return transport.Result{}, fmt.Errorf("simulated manifest read transport failure")
		}
		raw, exists := f.files[p]
		if !exists {
			return transport.Result{ExitCode: 3}, nil
		}
		return dsmOK(base64.StdEncoding.EncodeToString(raw)), nil

	case strings.Contains(s, "Expand-Archive"): // extract
		if f.fail["extract"] {
			return transport.Result{ExitCode: 1, Stderr: "corrupt zip"}, nil
		}
		f.mark("EXTRACT")
		return dsmOK(""), nil

	case strings.Contains(s, "New-Item -ItemType Directory"): // ensureLayout/ensureDir
		if f.fail["stage"] {
			return transport.Result{ExitCode: 1, Stderr: "mkdir failed"}, nil
		}
		return dsmOK(""), nil

	case dsmReMklink.MatchString(s): // switchJunction
		if f.failN["switch"] > 0 {
			f.failN["switch"]--
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		if f.fail["switch"] {
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		m := dsmReMklink.FindStringSubmatch(s)
		f.current = m[2]
		f.mark("SWITCH->" + m[2])
		return dsmOK(""), nil

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
		return dsmOK(""), nil

	case strings.Contains(s, "Stop-Service"): // Stop
		f.mark("STOP")
		if f.svc == "Running" {
			f.svc = "Stopped"
		}
		return dsmOK(""), nil

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
		return dsmOK(""), nil

	case strings.Contains(s, "exit 41"): // target-pull fetch script (checksum marker)
		if f.fail["checksum"] {
			return transport.Result{ExitCode: 41, Stderr: "sha256 mismatch"}, nil
		}
		if f.fail["fetch"] {
			return transport.Result{ExitCode: 40, Stderr: "download failed"}, nil
		}
		f.mark("FETCH")
		return dsmOK(""), nil

	case strings.Contains(s, "Invoke-WebRequest"): // health http
		if f.fail["health"] || (f.healthGate != nil && f.healthGate()) {
			return transport.Result{ExitCode: 1, Stdout: "connection refused"}, nil
		}
		f.mark("HEALTH")
		return dsmOK(""), nil

	case strings.Contains(s, "not_installed"): // Status probe
		if f.fail["status"] {
			return transport.Result{}, fmt.Errorf("simulated status probe transport failure")
		}
		if f.svc == "" {
			return dsmOK("not_installed"), nil
		}
		return dsmOK(strings.ToLower(f.svc)), nil

	case dsmReList.MatchString(s): // prune listing
		var b strings.Builder
		i := int64(1000)
		for d := range f.dirs {
			fmt.Fprintf(&b, "%s|%d\n", d, i)
			i++
		}
		return dsmOK(b.String()), nil

	case dsmReRmRecurse.MatchString(s):
		p := dsmReRmRecurse.FindStringSubmatch(s)[1]
		if f.fail["rm"] {
			return transport.Result{ExitCode: 1, Stderr: "remove failed"}, nil
		}
		for k := range f.files {
			if strings.HasPrefix(k, p) {
				delete(f.files, k)
			}
		}
		for d := range f.dirs {
			if strings.HasPrefix(d, p) {
				delete(f.dirs, d)
			}
		}
		f.mark("RM " + p)
		return dsmOK(""), nil

	case dsmReRmOne.MatchString(s): // ReleaseLock / staging cleanup
		delete(f.files, dsmReRmOne.FindStringSubmatch(s)[1])
		return dsmOK(""), nil

	case strings.Contains(s, "sc.exe query") && strings.Contains(s, "sc.exe delete"): // Uninstall
		if f.fail["uninstall"] {
			return transport.Result{ExitCode: 46, Stderr: "sc delete failed"}, nil
		}
		f.mark("UNINSTALL")
		f.svc = ""
		return dsmOK(""), nil

	case strings.Contains(s, "rmdir"): // removeJunction (standalone)
		f.current = ""
		return dsmOK(""), nil

	case strings.Contains(s, "SCM") || strings.Contains(s, "Get-WinEvent"):
		return dsmOK("[]"), nil
	}
	f.mark("UNMATCHED<<" + dsmFirstLine(s) + ">>")
	return transport.Result{ExitCode: 0}, nil
}

func (f *dsmHost) Upload(ctx context.Context, r io.Reader, size int64, remotePath string) error {
	b, _ := io.ReadAll(r)
	f.files[remotePath] = b
	return nil
}

func (f *dsmHost) Download(ctx context.Context, remotePath, localPath string) error {
	return fmt.Errorf("not needed in deploy e2e")
}

var _ transport.Transport = (*dsmHost)(nil)

func dsmOK(out string) transport.Result { return transport.Result{ExitCode: 0, Stdout: out} }

func dsmFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

// ----------------------------------------------------------------------------
// dsmCluster / dsmClusterNode: a genuine 2-node WSFC state machine. The
// coordinator FailoverClusters cmdlets mutate shared cluster state; all other
// scripts delegate to the embedded per-node dsmHost. Ported from the engine
// package's fakeCluster/clusterNode so scenario 4 drives the REAL cluster path.
// ----------------------------------------------------------------------------

type dsmCluster struct {
	nodes []string
	role  bool
	svc   string
	owner string
	state string
}

var dsmReNodeArg = regexp.MustCompile(`-Node '([^']+)'`)

type dsmClusterNode struct {
	*dsmHost
	cl *dsmCluster
}

func (n *dsmClusterNode) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "Add-ClusterGenericServiceRole"):
		n.cl.role = true
		n.cl.state = "Online"
		return dsmOK(""), nil
	case strings.Contains(s, "Remove-ClusterGroup"):
		n.cl.role = false
		return dsmOK(""), nil
	case strings.Contains(s, "Move-ClusterGroup"):
		if m := dsmReNodeArg.FindStringSubmatch(s); m != nil {
			n.cl.owner = strings.ToLower(m[1])
		}
		n.cl.state = "Online"
		return dsmOK(""), nil
	case strings.Contains(s, "Start-ClusterGroup"):
		n.cl.state = "Online"
		return dsmOK(""), nil
	case strings.Contains(s, "Set-ClusterOwnerNode"):
		return dsmOK(""), nil
	case strings.Contains(s, "Stop-ClusterGroup"):
		n.cl.state = "Offline"
		return dsmOK(""), nil
	case strings.Contains(s, "Get-ClusterResource"):
		if !n.cl.role {
			return dsmOK("ABSENT"), nil
		}
		return dsmOK("PRESENT|" + n.cl.svc + "|" + n.cl.owner + "|" + n.cl.state), nil
	case strings.Contains(s, "Get-ClusterNode"):
		return dsmOK(strings.Join(n.cl.nodes, "\n")), nil
	case strings.Contains(s, "FailoverClusters module missing"):
		return dsmOK(""), nil
	case strings.Contains(s, "Get-ClusterGroup") && strings.Contains(s, "not_installed"):
		if !n.cl.role {
			return dsmOK("not_installed"), nil
		}
		return dsmOK(strings.ToLower(n.cl.state)), nil
	}
	return n.dsmHost.Exec(ctx, c)
}

// ----------------------------------------------------------------------------
// spec / artifact helpers
// ----------------------------------------------------------------------------

func dsmChecksum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// dsmArtifactServer stands up an in-process http server serving payload and
// returns its url plus the payload's sha256 (the fake transport emulates the
// on-target fetch, so the server is never actually downloaded from — it only
// supplies a real URL + matching checksum for the spec).
func dsmArtifactServer(payload []byte) (url, checksum string, closeFn func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	return srv.URL + "/pkg.zip", dsmChecksum(payload), srv.Close
}

func dsmWinSvcSpec(url, checksum, version string, withRender bool) (*spec.Deployment, error) {
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
  version: %s
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
`, version, checksum, url)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		return nil, err
	}
	if withRender {
		d.Files = []spec.RenderedFile{{Path: "render.conf", Content: "x"}}
	}
	return d, nil
}

func dsmClusterSpec(url, checksum string) (*spec.Deployment, error) {
	y := fmt.Sprintf(`
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01", "lab-02"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: 2.0.0
  checksum: "%s"
  source: { type: http, url: "%s" }
pattern:
  type: cluster_generic_service
  service_name: SampleSvc
  role_name: SampleRole
  preferred_owner: lab-01
  exe: bin\SampleSvc.exe
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy:
  keep_releases: 2
  rollback_on_failure: true
  cluster: { health_settle_seconds: 1 }
`, checksum, url)
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		return nil, err
	}
	return d, nil
}

func dsmSeedManifest(f *dsmHost, m *engine.Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f.files[dsmManifestPath] = b
	return nil
}

func dsmSeedReleaseMarker(f *dsmHost, releaseDir, version, checksum string) {
	f.dirs[releaseDir] = true
	marker := releaseDir + `\` + dsmReleaseMarker
	body := struct {
		Version     string `json:"version"`
		SHA256      string `json:"sha256"`
		ExtractedAt string `json:"extracted_at"`
	}{version, checksum, time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(body)
	f.files[marker] = b
}

// ----------------------------------------------------------------------------
// tflog step-record capture
// ----------------------------------------------------------------------------

type dsmStep struct {
	step, app, host, version string
}

// dsmCaptureSteps decodes the tflog JSON sink and returns the "deploy step"
// records in emission order, asserting the mandatory field set (including a
// numeric duration_ms) on each.
func dsmCaptureSteps(buf *bytes.Buffer) ([]dsmStep, error) {
	entries, err := tflogtest.MultilineJSONDecode(buf)
	if err != nil {
		return nil, fmt.Errorf("decode tflog sink: %w\nraw=%s", err, buf.String())
	}
	var out []dsmStep
	for _, e := range entries {
		if e["@message"] != "deploy step" {
			continue
		}
		for _, k := range []string{"app", "host", "step", "version", "duration_ms"} {
			if v, ok := e[k]; !ok || v == nil {
				return nil, fmt.Errorf("step record missing %q field: %#v", k, e)
			}
		}
		durOK := false
		if _, ok := e["duration_ms"].(float64); ok {
			durOK = true
		} else if n, ok := e["duration_ms"].(json.Number); ok {
			_, ferr := n.Float64()
			durOK = ferr == nil
		}
		if !durOK {
			return nil, fmt.Errorf("step duration_ms is not numeric: %#v (%T)", e["duration_ms"], e["duration_ms"])
		}
		se := dsmStep{}
		se.step, _ = e["step"].(string)
		se.app, _ = e["app"].(string)
		se.host, _ = e["host"].(string)
		se.version, _ = e["version"].(string)
		out = append(out, se)
	}
	return out, nil
}

func dsmStepNames(steps []dsmStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.step
	}
	return out
}

func dsmCountStep(steps []dsmStep, name string) int {
	n := 0
	for _, s := range steps {
		if s.step == name {
			n++
		}
	}
	return n
}

// ----------------------------------------------------------------------------
// world + steps
// ----------------------------------------------------------------------------

type dsmWorld struct {
	closers []func()

	// matrix scenario
	mode    string
	step    string
	host    *dsmHost
	eng     *engine.Engine
	depSpec *spec.Deployment
	err     error

	// logging scenarios
	buf   bytes.Buffer
	steps []dsmStep
	noop  bool
}

func (w *dsmWorld) addCloser(c func()) { w.closers = append(w.closers, c) }

func (w *dsmWorld) reset() {
	for _, c := range w.closers {
		c()
	}
	*w = dsmWorld{}
}

func dsmManifestOf(f *dsmHost) string { return string(f.files[dsmManifestPath]) }

func dsmIsPreSwitch(step string) bool {
	switch step {
	case "stage", "fetch", "checksum", "extract", "render":
		return true
	}
	return false
}

// --- Rollback matrix row ----------------------------------------------------

func (w *dsmWorld) matrixFailureAtStep(step, mode string) error {
	w.step = step
	w.mode = mode
	payload := []byte("v-" + mode + "-" + step)
	url, sum, closeFn := dsmArtifactServer(payload)
	w.addCloser(closeFn)

	f := dsmNewHost("lab-01")
	w.host = f
	w.eng = engine.New()
	w.eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return f, nil }

	withRender := step == "render"

	if mode == "update" {
		// Deploy a real v1 (1.0.0) first so the update leg has a previous release,
		// a running service and a success manifest to protect / roll back to.
		v1url, v1sum, v1close := dsmArtifactServer([]byte("v1-" + step))
		w.addCloser(v1close)
		v1, err := dsmWinSvcSpec(v1url, v1sum, "1.0.0", withRender)
		if err != nil {
			return err
		}
		if _, err := w.eng.Deploy(context.Background(), v1); err != nil {
			return fmt.Errorf("v1 setup deploy failed: %w", err)
		}
		f.log = nil
	}

	version := "1.0.0"
	if mode == "update" {
		version = "2.0.0"
	}
	d, err := dsmWinSvcSpec(url, sum, version, withRender)
	if err != nil {
		return err
	}
	w.depSpec = d

	// Inject the scripted failure at the requested step.
	if mode == "update" && !dsmIsPreSwitch(step) {
		// Switch-phase failures on an update: fail only the FORWARD attempt so the
		// rollback's re-switch/re-start/health against the previous release passes.
		switch step {
		case "switch", "configure", "start":
			f.failN[step] = 1
		case "health":
			f.healthGate = func() bool { return strings.HasSuffix(f.current, "2.0.0") }
		}
	} else {
		f.fail[step] = true
	}
	return nil
}

func (w *dsmWorld) theDeployRuns() error {
	if w.depSpec == nil || w.eng == nil {
		return fmt.Errorf("deploy preconditions not set (Given step did not run)")
	}
	_, w.err = w.eng.Deploy(context.Background(), w.depSpec)
	return nil
}

func (w *dsmWorld) matrixStateMatches(outcome string) error {
	if w.err == nil {
		return fmt.Errorf("expected the scripted %q failure to fail Deploy, got nil (log=%v)", w.step, w.host.log)
	}
	f := w.host
	switch outcome {
	case "fresh-cleaned", "fresh-wiped":
		if _, has := f.files[dsmManifestPath]; has {
			return fmt.Errorf("fresh failure must leave NO manifest; log=%v", f.log)
		}
		if f.svc != "" {
			return fmt.Errorf("fresh failure must leave NO service, got %q", f.svc)
		}
		if f.current != "" {
			return fmt.Errorf("fresh failure must leave NO junction, got %q", f.current)
		}
		if outcome == "fresh-wiped" {
			if err := dsmAssertStagingEmpty(f); err != nil {
				return err
			}
			if err := dsmAssertNoRelease(f); err != nil {
				return err
			}
		}
		return nil

	case "update-wiped":
		if err := dsmAssertPrevIntact(f); err != nil {
			return err
		}
		if err := dsmAssertStagingEmpty(f); err != nil {
			return err
		}
		if _, has := f.files[`C:\deploy\sample-svc\releases\2.0.0\`+dsmReleaseMarker]; has {
			return fmt.Errorf("incomplete 2.0.0 release marker must be removed on staging failure")
		}
		return nil

	case "update-restored":
		if !strings.Contains(w.err.Error(), "rolled back to 1.0.0") {
			return fmt.Errorf("switch-phase update failure must roll back to 1.0.0, got: %v", w.err)
		}
		if !strings.HasSuffix(f.current, `releases\1.0.0`) {
			return fmt.Errorf("rollback must restore junction to 1.0.0, got %q", f.current)
		}
		m := dsmManifestOf(f)
		if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "rolled_back"`) {
			return fmt.Errorf("rollback manifest must record 1.0.0 rolled_back: %s", m)
		}
		return nil
	}
	return fmt.Errorf("unknown rollback-matrix outcome %q", outcome)
}

func dsmAssertStagingEmpty(f *dsmHost) error {
	for k := range f.files {
		if strings.Contains(k, `\staging\`) {
			return fmt.Errorf("staging must be empty after a pre-switch failure; found file %q", k)
		}
	}
	for d := range f.dirs {
		if strings.Contains(d, `\staging\`) {
			return fmt.Errorf("staging dir must be wiped after a pre-switch failure; found %q", d)
		}
	}
	return nil
}

func dsmAssertNoRelease(f *dsmHost) error {
	for k := range f.files {
		if strings.Contains(k, `\releases\`) {
			return fmt.Errorf("fresh failure must leave NO release tree; found %q", k)
		}
	}
	return nil
}

func dsmAssertPrevIntact(f *dsmHost) error {
	if !strings.HasSuffix(f.current, `releases\1.0.0`) {
		return fmt.Errorf("pre-switch failure must keep junction at 1.0.0, got %q", f.current)
	}
	m := dsmManifestOf(f)
	if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "success"`) {
		return fmt.Errorf("pre-switch failure must leave the 1.0.0 success manifest untouched: %s", m)
	}
	return nil
}

// --- Idempotent short-circuit -----------------------------------------------

func (w *dsmWorld) targetAlreadyAtSpecVersion() error {
	payload := []byte("idempotent zip")
	url, sum, closeFn := dsmArtifactServer(payload)
	w.addCloser(closeFn)
	f := dsmNewHost("lab-01")
	f.svc = "Running"
	if err := dsmSeedManifest(f, &engine.Manifest{
		Schema: 1, App: "sample-svc", Pattern: "windows_service",
		CurrentVersion: "1.0.0", ArtifactChecksum: sum,
		CurrentRelease:  `C:\deploy\sample-svc\releases\1.0.0`,
		ProviderVersion: engine.ProviderVersion,
		LastOperation:   engine.LastOp{Type: "deploy", Result: "success"},
	}); err != nil {
		return err
	}
	w.host = f
	w.eng = engine.New()
	w.eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return f, nil }
	d, err := dsmWinSvcSpec(url, sum, "1.0.0", false)
	if err != nil {
		return err
	}
	w.depSpec = d
	return nil
}

func (w *dsmWorld) theDeployRunsCapturing() error {
	if w.depSpec == nil {
		return fmt.Errorf("deploy preconditions not set (Given step did not run)")
	}
	ctx := tflogtest.RootLogger(context.Background(), &w.buf)
	st, err := w.eng.Deploy(ctx, w.depSpec)
	w.err = err
	if st != nil && st.DeployedVersion != "" && err == nil {
		w.noop = st.DeployedVersion == w.depSpec.Artifact.Version
	}
	steps, cerr := dsmCaptureSteps(&w.buf)
	if cerr != nil {
		return cerr
	}
	w.steps = steps
	return nil
}

func (w *dsmWorld) noopNoFetchNoSwitch() error {
	if w.err != nil {
		return fmt.Errorf("idempotent deploy returned error: %w", w.err)
	}
	if !w.noop {
		return fmt.Errorf("expected NO-OP at the spec version, got deployed=%q", func() string {
			return "not-noop"
		}())
	}
	if dsmCountStep(w.steps, "FETCH") != 0 {
		return fmt.Errorf("idempotent no-op logged FETCH: %v", dsmStepNames(w.steps))
	}
	if dsmCountStep(w.steps, "SWITCH") != 0 {
		return fmt.Errorf("idempotent no-op logged SWITCH: %v", dsmStepNames(w.steps))
	}
	// The pre-switch gates still run and are still logged.
	for _, must := range []string{"VALIDATE", "CONNECT", "PREFLIGHT", "LOCK"} {
		if dsmCountStep(w.steps, must) == 0 {
			return fmt.Errorf("gate step %q missing on no-op: %v", must, dsmStepNames(w.steps))
		}
	}
	return nil
}

// --- Single-host step order logged ------------------------------------------

func (w *dsmWorld) successfulSingleHostDeploy() error {
	payload := []byte("fresh order zip")
	url, sum, closeFn := dsmArtifactServer(payload)
	w.addCloser(closeFn)
	f := dsmNewHost("lab-01")
	w.host = f
	w.eng = engine.New()
	w.eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return f, nil }
	d, err := dsmWinSvcSpec(url, sum, "1.0.0", false)
	if err != nil {
		return err
	}
	w.depSpec = d
	return nil
}

func (w *dsmWorld) theDeployCompletes() error {
	ctx := tflogtest.RootLogger(context.Background(), &w.buf)
	if _, err := w.eng.Deploy(ctx, w.depSpec); err != nil {
		return fmt.Errorf("deploy failed: %w (log=%v)", err, w.host.log)
	}
	steps, cerr := dsmCaptureSteps(&w.buf)
	if cerr != nil {
		return cerr
	}
	w.steps = steps
	return nil
}

func (w *dsmWorld) stepsInFixedOrder() error {
	names := dsmStepNames(w.steps)
	want := []string{
		"VALIDATE", "CONNECT", "PREFLIGHT", "LOCK",
		"FETCH", "CHECKSUM", "STAGE", "EXTRACT", "RENDER",
		"STOP", "SWITCH", "CONFIGURE", "START", "HEALTH",
		"FINALIZE", "PRUNE", "UNLOCK",
	}
	if len(names) != len(want) {
		return fmt.Errorf("expected exactly %d step records, got %d\n want=%v\n got =%v", len(want), len(names), want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			return fmt.Errorf("step order mismatch at index %d: want %q got %q\n want=%v\n got =%v", i, want[i], names[i], want, names)
		}
	}
	for _, wnt := range want {
		if c := dsmCountStep(w.steps, wnt); c != 1 {
			return fmt.Errorf("fixed step %q must be logged exactly once, got %d; names=%v", wnt, c, names)
		}
	}
	return nil
}

func (w *dsmWorld) everyStepCarriesFields() error {
	for _, s := range w.steps {
		if s.app != "sample-svc" || s.host != "lab-01" || s.version != "1.0.0" {
			return fmt.Errorf("step %q has wrong fields app=%q host=%q version=%q", s.step, s.app, s.host, s.version)
		}
	}
	return nil
}

// --- Conditional and per-host steps logged (cluster) ------------------------

func (w *dsmWorld) multiHostUpdateWithCacheAndRollback() error {
	payload := []byte("cluster v2 zip")
	url, sum, closeFn := dsmArtifactServer(payload)
	w.addCloser(closeFn)

	cl := &dsmCluster{
		nodes: []string{"lab-01", "lab-02"},
		role:  true, svc: "SampleSvc", owner: "lab-01", state: "Online",
	}
	newNode := func(host string) (*dsmClusterNode, error) {
		f := dsmNewHost(host)
		f.svc = "Running"
		if err := dsmSeedManifest(f, &engine.Manifest{
			Schema: 1, App: "sample-svc", Pattern: "cluster_generic_service",
			CurrentVersion: "1.0.0", ArtifactChecksum: "sha256:old",
			CurrentRelease:  `C:\deploy\sample-svc\releases\1.0.0`,
			ProviderVersion: engine.ProviderVersion,
			LastOperation:   engine.LastOp{Type: "deploy", Result: "success"},
		}); err != nil {
			return nil, err
		}
		return &dsmClusterNode{dsmHost: f, cl: cl}, nil
	}
	n1, err := newNode("lab-01")
	if err != nil {
		return err
	}
	n2, err := newNode("lab-02")
	if err != nil {
		return err
	}
	// Passive node lab-02 already has 2.0.0 fully extracted ⇒ its stage skips
	// FETCH/CHECKSUM. Owner lab-01 is NOT cached ⇒ it fetches, so exactly one
	// host skips FETCH/CHECKSUM.
	dsmSeedReleaseMarker(n2.dsmHost, `C:\deploy\sample-svc\releases\2.0.0`, "2.0.0", sum)
	// The preferred owner's health at 2.0.0 fails, driving a rollback; it passes
	// again once the junction is restored to 1.0.0.
	n1.healthGate = func() bool { return strings.Contains(n1.current, "2.0.0") }

	nodes := map[string]*dsmClusterNode{"lab-01": n1, "lab-02": n2}
	w.eng = engine.New()
	w.eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
		return nodes[strings.ToLower(host)], nil
	}
	d, err := dsmClusterSpec(url, sum)
	if err != nil {
		return err
	}
	w.depSpec = d
	return nil
}

func (w *dsmWorld) theClusterDeployRuns() error {
	ctx := tflogtest.RootLogger(context.Background(), &w.buf)
	_, err := w.eng.Deploy(ctx, w.depSpec)
	if err == nil {
		return fmt.Errorf("expected rolled-back cluster deploy to return the original error")
	}
	if !strings.Contains(err.Error(), "rolled back to 1.0.0") {
		return fmt.Errorf("expected cluster rollback-to-previous error, got: %v", err)
	}
	steps, cerr := dsmCaptureSteps(&w.buf)
	if cerr != nil {
		return cerr
	}
	w.steps = steps
	return nil
}

func (w *dsmWorld) byHost() map[string][]dsmStep {
	out := map[string][]dsmStep{}
	for _, s := range w.steps {
		out[s.host] = append(out[s.host], s)
	}
	return out
}

func (w *dsmWorld) cachedHostSkipsFetchChecksum() error {
	bh := w.byHost()
	if len(bh["lab-01"]) == 0 || len(bh["lab-02"]) == 0 {
		return fmt.Errorf("expected step records for BOTH nodes; hosts=%v", func() []string {
			var hs []string
			for h := range bh {
				hs = append(hs, h)
			}
			return hs
		}())
	}
	if dsmCountStep(bh["lab-02"], "FETCH") != 0 || dsmCountStep(bh["lab-02"], "CHECKSUM") != 0 {
		return fmt.Errorf("cached passive lab-02 must NOT log FETCH/CHECKSUM: %v", dsmStepNames(bh["lab-02"]))
	}
	if dsmCountStep(bh["lab-01"], "FETCH") == 0 || dsmCountStep(bh["lab-01"], "CHECKSUM") == 0 {
		return fmt.Errorf("non-cached owner lab-01 must log FETCH and CHECKSUM: %v", dsmStepNames(bh["lab-01"]))
	}
	return nil
}

func (w *dsmWorld) eachHostRepeatsSwitchWithFields() error {
	bh := w.byHost()
	if got := dsmCountStep(bh["lab-02"], "SWITCH"); got < 2 {
		return fmt.Errorf("expected passive lab-02 SWITCH repeated (forward + rollback), got %d: %v", got, dsmStepNames(bh["lab-02"]))
	}
	if got := dsmCountStep(bh["lab-01"], "SWITCH"); got < 2 {
		return fmt.Errorf("expected owner lab-01 SWITCH repeated (update + rollback), got %d: %v", got, dsmStepNames(bh["lab-01"]))
	}
	if dsmCountStep(w.steps, "ROLLBACK") == 0 {
		return fmt.Errorf("expected a ROLLBACK step record: %v", dsmStepNames(w.steps))
	}
	// captureSteps already enforced field presence + numeric duration_ms; assert
	// the contextual fields are populated and correct per node.
	for _, s := range w.steps {
		if s.app != "sample-svc" {
			return fmt.Errorf("step %q has wrong app=%q", s.step, s.app)
		}
		if s.host != "lab-01" && s.host != "lab-02" {
			return fmt.Errorf("step %q emitted for unexpected host %q", s.step, s.host)
		}
		if s.version == "" {
			return fmt.Errorf("step %q on %q missing version", s.step, s.host)
		}
	}
	return nil
}

// InitializeScenario_engine_core_and_manifest_state_deploy_state_machine_and_rollback_matrix
// registers the step definitions for the Stage 3.5 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_engine_core_and_manifest_state_deploy_state_machine_and_rollback_matrix(ctx *godog.ScenarioContext) {
	w := &dsmWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		w.reset()
		return c, nil
	})
	ctx.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.reset()
		return c, nil
	})

	// Rollback matrix row.
	ctx.Step(`^a fake transport scripting a failure at the "([^"]*)" step of a "([^"]*)" deploy$`,
		func(step, mode string) error { return w.matrixFailureAtStep(step, mode) })
	ctx.Step(`^the deploy runs$`, w.dispatchDeployRuns)
	ctx.Step(`^the resulting on-target state matches the "([^"]*)" rollback-matrix row$`,
		func(outcome string) error { return w.matrixStateMatches(outcome) })

	// Idempotent short-circuit.
	ctx.Step(`^a target already at the spec version with a matching checksum and a healthy service$`,
		w.targetAlreadyAtSpecVersion)
	ctx.Step(`^it returns a NO-OP with no FETCH or SWITCH step logged$`, w.noopNoFetchNoSwitch)

	// Single-host step order logged.
	ctx.Step(`^a successful single-host deploy captured through a tflog test sink$`,
		w.successfulSingleHostDeploy)
	ctx.Step(`^the deploy completes$`, w.theDeployCompletes)
	ctx.Step(`^the logged step values appear in the fixed VALIDATE\.\.UNLOCK order$`, w.stepsInFixedOrder)
	ctx.Step(`^every logged step carries app, host, step, version and a numeric duration_ms$`, w.everyStepCarriesFields)

	// Conditional and per-host steps logged.
	ctx.Step(`^a multi-host update where one host has the release cached and a rollback repeats SWITCH captured through a tflog test sink$`,
		w.multiHostUpdateWithCacheAndRollback)
	ctx.Step(`^the cached host logs neither FETCH nor CHECKSUM while the other host logs both$`,
		w.cachedHostSkipsFetchChecksum)
	ctx.Step(`^each host logs SWITCH at least twice and every record carries the full app/host/step/version/duration_ms field set$`,
		w.eachHostRepeatsSwitchWithFields)
}

// dispatchDeployRuns routes the shared "the deploy runs" phrase to the matrix or
// cluster scenario depending on which Given seeded the world.
func (w *dsmWorld) dispatchDeployRuns() error {
	if w.mode != "" {
		return w.theDeployRuns()
	}
	if w.depSpec != nil && w.depSpec.Pattern.Type == spec.PatternClusterGeneric {
		return w.theClusterDeployRuns()
	}
	// Idempotent short-circuit reuses "the deploy runs".
	return w.theDeployRunsCapturing()
}

// TestE2E_engine_core_and_manifest_state_deploy_state_machine_and_rollback_matrix
// is the go test entrypoint for the Stage 3.5 godog suite.
func TestE2E_engine_core_and_manifest_state_deploy_state_machine_and_rollback_matrix(t *testing.T) {
	if err := ensureDeployPassword(); err != nil {
		t.Fatal(err)
	}
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_engine_core_and_manifest_state_deploy_state_machine_and_rollback_matrix,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"engine_core_and_manifest_state_deploy_state_machine_and_rollback_matrix.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
