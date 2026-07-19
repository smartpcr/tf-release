//go:build e2e

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
	"regexp"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// ----------------------------------------------------------------------------
// In-process "fake Transport with a scripted Result queue" (DESIGN §17). This is
// a self-contained port of the engine package's fakeHost/clusterNode test
// dispatcher so the out-of-package godog suite can drive the REAL Stage 6.2
// cluster rolling-update / rollback engine (internal/engine/cluster.go) end to
// end with NO network and NO docker. Every symbol is `clu`-prefixed so sibling
// stages sharing this `e2e` package do not collide.
// ----------------------------------------------------------------------------

// cluFakeHost is a script-dispatching virtual Windows target: it emulates just
// enough of the PowerShell surface the engine emits (staging, junction switch,
// service control, health probe, manifest read/write, locking).
type cluFakeHost struct {
	host       string
	files      map[string][]byte
	dirs       map[string]bool
	current    string // junction target
	svc        string // "", "Stopped", "Running"
	fail       map[string]bool
	failN      map[string]int
	healthGate func() bool
	log        []string
}

func cluNewFakeHost(name string) *cluFakeHost {
	return &cluFakeHost{host: name, files: map[string][]byte{}, dirs: map[string]bool{},
		fail: map[string]bool{}, failN: map[string]int{}}
}

func (f *cluFakeHost) mark(s string)                       { f.log = append(f.log, s) }
func (f *cluFakeHost) Connect(ctx context.Context) error   { if f.fail["connect"] { return fmt.Errorf("simulated connect failure") }; return nil }
func (f *cluFakeHost) Close() error                        { return nil }
func (f *cluFakeHost) OS() spec.OSKind                     { return spec.OSWindows }
func (f *cluFakeHost) Host() string                        { return f.host }

var (
	cluReRead       = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
	cluReWrite      = regexp.MustCompile(`WriteAllBytes\('([^']+)',\[Convert\]::FromBase64String\('([^']*)'\)\)`)
	cluReLock       = regexp.MustCompile(`\[IO\.File\]::Move\(\$tmp,'([^']+)'\)`)
	cluReRmRecurse  = regexp.MustCompile(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue '([^']+)'`)
	cluReRmOne      = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)
	cluReMklink     = regexp.MustCompile(`mklink /J "([^"]+)" "([^"]+)"`)
	cluReList       = regexp.MustCompile(`Get-ChildItem -Directory '([^']+)'`)
	cluReCasDel     = regexp.MustCompile(`\[IO\.File\]::Delete\('([^']+)'\)`)
	cluReCurEq      = regexp.MustCompile(`\$cur -eq '([^']*)'`)
	cluReCasReplace = regexp.MustCompile(`\[IO\.File\]::Replace\(\$tmp,'([^']+)',`)
	cluReCurNe      = regexp.MustCompile(`\$cur -ne '([^']*)'`)
	cluReB64        = regexp.MustCompile(`FromBase64String\('([^']*)'\)`)
)

func cluOK(out string) transport.Result { return transport.Result{ExitCode: 0, Stdout: out} }

func (f *cluFakeHost) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "$PSVersionTable"): // preflight
		if f.fail["preflight"] {
			return transport.Result{ExitCode: 1, Stderr: "preflight gate failed"}, nil
		}
		f.mark("PREFLIGHT")
		return cluOK(""), nil

	case cluReCasReplace.MatchString(s): // casReplace atomic compare-and-replace
		path := cluReCasReplace.FindStringSubmatch(s)[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil
		}
		em := cluReCurNe.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil
		}
		if nm := cluReB64.FindStringSubmatch(s); nm != nil {
			raw, _ := base64.StdEncoding.DecodeString(nm[1])
			f.files[path] = raw
		}
		f.mark("LOCK")
		return cluOK(""), nil

	case cluReCasDel.MatchString(s): // casDelete compare-and-delete
		path := cluReCasDel.FindStringSubmatch(s)[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil
		}
		em := cluReCurEq.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil
		}
		delete(f.files, path)
		return cluOK(""), nil

	case cluReLock.MatchString(s): // AcquireLock atomic create
		if f.fail["lock"] {
			return transport.Result{}, fmt.Errorf("simulated lock acquire transport failure")
		}
		p := cluReLock.FindStringSubmatch(s)[1]
		if _, held := f.files[p]; held {
			return transport.Result{ExitCode: 48}, nil
		}
		if m := cluReB64.FindStringSubmatch(s); m != nil {
			raw, _ := base64.StdEncoding.DecodeString(m[1])
			f.files[p] = raw
		} else {
			f.files[p] = []byte("{}")
		}
		f.mark("LOCK")
		return cluOK(""), nil

	case cluReWrite.MatchString(s): // writeSmallFile
		m := cluReWrite.FindStringSubmatch(s)
		if f.fail["render"] && strings.Contains(m[1], "render.conf") {
			return transport.Result{ExitCode: 1, Stderr: "render write failed"}, nil
		}
		raw, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return transport.Result{ExitCode: 1, Stderr: "b64"}, nil
		}
		f.files[m[1]] = raw
		return cluOK(""), nil

	case cluReRead.MatchString(s): // readSmallFile
		p := cluReRead.FindStringSubmatch(s)[1]
		if f.fail["read"] {
			return transport.Result{}, fmt.Errorf("simulated manifest read transport failure")
		}
		raw, exists := f.files[p]
		if !exists {
			return transport.Result{ExitCode: 3}, nil
		}
		return cluOK(base64.StdEncoding.EncodeToString(raw)), nil

	case strings.Contains(s, "Expand-Archive"): // extract
		if f.fail["extract"] {
			return transport.Result{ExitCode: 1, Stderr: "corrupt zip"}, nil
		}
		f.mark("EXTRACT")
		return cluOK(""), nil

	case strings.Contains(s, "New-Item -ItemType Directory"): // ensureLayout/ensureDir
		if f.fail["stage"] {
			return transport.Result{ExitCode: 1, Stderr: "mkdir failed"}, nil
		}
		return cluOK(""), nil

	case cluReMklink.MatchString(s): // switchJunction
		if f.failN["switch"] > 0 {
			f.failN["switch"]--
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		if f.fail["switch"] {
			return transport.Result{ExitCode: 42, Stderr: "mklink failed"}, nil
		}
		m := cluReMklink.FindStringSubmatch(s)
		f.current = m[2]
		f.mark("SWITCH->" + m[2])
		return cluOK(""), nil

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
		return cluOK(""), nil

	case strings.Contains(s, "Stop-Service"): // graceful Stop
		f.mark("STOP")
		if f.svc == "Running" {
			f.svc = "Stopped"
		}
		return cluOK(""), nil

	case strings.Contains(s, "taskkill /PID"): // FORCE_KILL escalation
		f.mark("FORCE_KILL")
		f.svc = "Stopped"
		return cluOK(""), nil

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
		return cluOK(""), nil

	case strings.Contains(s, "exit 41"): // target-pull fetch/checksum
		if f.fail["checksum"] {
			return transport.Result{ExitCode: 41, Stderr: "sha256 mismatch"}, nil
		}
		if f.fail["fetch"] {
			return transport.Result{ExitCode: 40, Stderr: "download failed"}, nil
		}
		f.mark("FETCH")
		return cluOK(""), nil

	case strings.Contains(s, "Invoke-WebRequest"): // health http
		if f.fail["health"] || (f.healthGate != nil && f.healthGate()) {
			return transport.Result{ExitCode: 1, Stdout: "connection refused"}, nil
		}
		f.mark("HEALTH")
		return cluOK(""), nil

	case strings.Contains(s, "not_installed"): // Status probe (non-cluster)
		if f.fail["status"] {
			return transport.Result{}, fmt.Errorf("simulated status probe transport failure")
		}
		if f.svc == "" {
			return cluOK("not_installed"), nil
		}
		return cluOK(strings.ToLower(f.svc)), nil

	case cluReList.MatchString(s): // prune listing
		var b strings.Builder
		i := int64(1000)
		for d := range f.dirs {
			fmt.Fprintf(&b, "%s|%d\n", d, i)
			i++
		}
		return cluOK(b.String()), nil

	case cluReRmRecurse.MatchString(s):
		p := cluReRmRecurse.FindStringSubmatch(s)[1]
		for k := range f.files {
			if strings.HasPrefix(k, p) {
				delete(f.files, k)
			}
		}
		f.mark("RM " + p)
		return cluOK(""), nil

	case cluReRmOne.MatchString(s): // ReleaseLock / staging cleanup
		delete(f.files, cluReRmOne.FindStringSubmatch(s)[1])
		return cluOK(""), nil

	case strings.Contains(s, "sc.exe query") && strings.Contains(s, "sc.exe delete"): // Uninstall
		if f.fail["uninstall"] {
			return transport.Result{ExitCode: 46, Stderr: "sc delete failed"}, nil
		}
		f.mark("UNINSTALL")
		f.svc = ""
		return cluOK(""), nil

	case strings.Contains(s, "rmdir"): // removeJunction
		f.current = ""
		return cluOK(""), nil

	case strings.Contains(s, "SCM") || strings.Contains(s, "Get-WinEvent"):
		return cluOK("[]"), nil
	}
	f.mark("UNMATCHED<<" + cluFirstLine(s) + ">>")
	return transport.Result{ExitCode: 0, Stdout: ""}, nil
}

func (f *cluFakeHost) Upload(ctx context.Context, r io.Reader, size int64, remotePath string) error {
	b, _ := io.ReadAll(r)
	f.files[remotePath] = b
	return nil
}
func (f *cluFakeHost) Download(ctx context.Context, remotePath, localPath string) error {
	return fmt.Errorf("not needed in tests")
}

var _ transport.Transport = (*cluFakeHost)(nil)

func cluFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

// cluFakeCluster is the WSFC state shared across all nodes (role existence,
// bound service, current owner, group state). The coordinator cmdlets mutate it.
type cluFakeCluster struct {
	nodes []string
	role  bool
	svc   string
	owner string
	state string
}

var cluReNodeArg = regexp.MustCompile(`-Node '([^']+)'`)

// cluNode wraps a per-node cluFakeHost and additionally answers the coordinator
// FailoverClusters cmdlets against the shared cluster state.
type cluNode struct {
	*cluFakeHost
	cl *cluFakeCluster
}

func (n *cluNode) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "Add-ClusterGenericServiceRole"): // CreateRole
		n.cl.role = true
		n.cl.state = "Online"
		return cluOK(""), nil
	case strings.Contains(s, "Remove-ClusterGroup"): // RemoveRole
		n.cl.role = false
		return cluOK(""), nil
	case strings.Contains(s, "Move-ClusterGroup"): // MoveGroup
		if m := cluReNodeArg.FindStringSubmatch(s); m != nil {
			n.cl.owner = strings.ToLower(m[1])
		}
		n.cl.state = "Online"
		return cluOK(""), nil
	case strings.Contains(s, "Start-ClusterGroup"): // StartGroup
		n.cl.state = "Online"
		return cluOK(""), nil
	case strings.Contains(s, "Set-ClusterOwnerNode"): // SetPreferredOwners
		return cluOK(""), nil
	case strings.Contains(s, "Stop-ClusterGroup"): // StopGroup
		n.cl.state = "Offline"
		return cluOK(""), nil
	case strings.Contains(s, "Get-ClusterResource"): // RoleBinding
		if !n.cl.role {
			return cluOK("ABSENT"), nil
		}
		return cluOK("PRESENT|" + n.cl.svc + "|" + n.cl.owner + "|" + n.cl.state), nil
	case strings.Contains(s, "Get-ClusterNode"): // NodesUp
		return cluOK(strings.Join(n.cl.nodes, "\n")), nil
	case strings.Contains(s, "FailoverClusters module missing"): // pattern Preflight
		return cluOK(""), nil
	case strings.Contains(s, "Get-ClusterGroup") && strings.Contains(s, "not_installed"): // Status
		if !n.cl.role {
			return cluOK("not_installed"), nil
		}
		return cluOK(strings.ToLower(n.cl.state)), nil
	}
	return n.cluFakeHost.Exec(ctx, c)
}

// ----------------------------------------------------------------------------
// tflog step-record capture (DESIGN §8.5). Every fixed step is emitted through
// tflog carrying app/host/step/version; we decode them to assert order/counts.
// ----------------------------------------------------------------------------

type cluStepEntry struct {
	step    string
	host    string
	version string
}

func cluCaptureSteps(buf *bytes.Buffer) ([]cluStepEntry, error) {
	entries, err := tflogtest.MultilineJSONDecode(buf)
	if err != nil {
		return nil, fmt.Errorf("decode tflog sink: %w\nraw=%s", err, buf.String())
	}
	var out []cluStepEntry
	for _, e := range entries {
		if e["@message"] != "deploy step" {
			continue
		}
		se := cluStepEntry{}
		se.step, _ = e["step"].(string)
		se.host, _ = e["host"].(string)
		se.version, _ = e["version"].(string)
		out = append(out, se)
	}
	return out, nil
}

func cluCountStep(steps []cluStepEntry, name string) int {
	n := 0
	for _, s := range steps {
		if s.step == name {
			n++
		}
	}
	return n
}

func cluStepNames(steps []cluStepEntry) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.step
	}
	return out
}

// ----------------------------------------------------------------------------
// scenario world
// ----------------------------------------------------------------------------

type cluWorld struct {
	hosts       []string
	owner       string
	prevRelease string
	nodeCount   int // declared "N-node" size from the scenario header; enforced against len(hosts)
	cl          *cluFakeCluster
	nodes       map[string]*cluNode
	order       []*cluNode // hosts order

	srv *httptest.Server
	url string
	sum string

	out   *engine.Status
	err   error
	steps []cluStepEntry
}

func (w *cluWorld) reset() {
	if w.srv != nil {
		w.srv.Close()
	}
	*w = cluWorld{nodes: map[string]*cluNode{}}
}

func (w *cluWorld) manifestPath() string { return `C:\deploy\sample-svc\manifest.json` }

func (w *cluWorld) seedManifest(f *cluFakeHost, version, releasePath string) error {
	m := &engine.Manifest{
		Schema: 1, App: "sample-svc", Pattern: "cluster_generic_service",
		CurrentVersion: version, ArtifactChecksum: "sha256:old",
		CurrentRelease:  releasePath,
		ProviderVersion: engine.ProviderVersion,
		LastOperation:   engine.LastOp{Type: "deploy", Result: "success"},
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f.files[w.manifestPath()] = b
	return nil
}

// givenClusterOwnerRunning records the declared node count, the original owner
// and the previous release version. It does not build the nodes yet — that
// happens once the full host list is known. The declared count is stored so
// givenClusterHosts can enforce that the scenario header ("N-node cluster") and
// the hosts list stay in sync.
func (w *cluWorld) givenClusterOwnerRunning(n int, owner, version string) error {
	w.nodeCount = n
	w.owner = strings.ToLower(owner)
	w.prevRelease = version
	return nil
}

// givenClusterHosts builds the shared WSFC state + one seeded fake node per host.
func (w *cluWorld) givenClusterHosts(hostsCSV string) error {
	w.hosts = splitCSV(hostsCSV)
	if len(w.hosts) != w.nodeCount {
		return fmt.Errorf("declared %d-node cluster header disagrees with hosts list %q (%d hosts); keep the scenario cardinality and the hosts list in sync",
			w.nodeCount, hostsCSV, len(w.hosts))
	}
	relPath := `C:\deploy\sample-svc\releases\` + w.prevRelease
	w.cl = &cluFakeCluster{
		nodes: w.hosts, role: true, svc: "SampleSvc",
		owner: w.owner, state: "Online",
	}
	for _, h := range w.hosts {
		f := cluNewFakeHost(h)
		f.svc = "Running"
		f.current = relPath
		if err := w.seedManifest(f, w.prevRelease, relPath); err != nil {
			return err
		}
		node := &cluNode{cluFakeHost: f, cl: w.cl}
		w.nodes[strings.ToLower(h)] = node
		w.order = append(w.order, node)
	}
	return nil
}

// givenHealthFailsOnFirstNew makes the named node's health probe fail while its
// junction points at the new (non-previous) release, so U5 fails on firstNew and
// passes again once rollback restores the previous junction.
func (w *cluWorld) givenHealthFailsOnFirstNew(host string) error {
	node, ok := w.nodes[strings.ToLower(host)]
	if !ok {
		return fmt.Errorf("unknown host %q", host)
	}
	prev := w.prevRelease
	node.cluFakeHost.healthGate = func() bool {
		return !strings.Contains(node.cluFakeHost.current, prev)
	}
	return nil
}

func (w *cluWorld) whenUpdate(version string) error {
	payload := []byte("cluster " + version + " zip")
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = rw.Write(payload)
	}))
	w.url = w.srv.URL + "/pkg.zip"
	sum := sha256.Sum256(payload)
	w.sum = "sha256:" + hex.EncodeToString(sum[:])

	d, err := cluUpdateSpec(w.hosts, w.url, w.sum, version)
	if err != nil {
		return err
	}

	eng := engine.New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) {
		n, ok := w.nodes[strings.ToLower(host)]
		if !ok {
			return nil, fmt.Errorf("no fake node for host %q", host)
		}
		return n, nil
	}

	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	w.out, w.err = eng.Deploy(ctx, d)
	steps, cerr := cluCaptureSteps(&buf)
	if cerr != nil {
		return cerr
	}
	w.steps = steps
	return nil
}

func (w *cluWorld) thenUpdateSucceeds() error {
	if w.err != nil {
		return fmt.Errorf("healthy rolling update must succeed, got: %v", w.err)
	}
	if w.out == nil {
		return fmt.Errorf("healthy rolling update must return a status")
	}
	return nil
}

func (w *cluWorld) thenPassivesUpdatedInOrder(orderCSV string) error {
	want := splitCSV(orderCSV)
	var pre []string
	for _, s := range w.steps {
		if s.step == "MOVE_GROUP" {
			break
		}
		if s.step == "SWITCH" {
			pre = append(pre, s.host)
		}
	}
	if !cluEqual(pre, want) {
		return fmt.Errorf("pre-move passive SWITCH order must be EXACTLY %v (hosts order, no owner, no dupes), got %v",
			want, pre)
	}
	return nil
}

func (w *cluWorld) thenExactlyOneMove() error {
	if got := cluCountStep(w.steps, "MOVE_GROUP"); got != 1 {
		return fmt.Errorf("rolling update must emit EXACTLY ONE MOVE_GROUP, got %d: %v", got, cluStepNames(w.steps))
	}
	return nil
}

func (w *cluWorld) thenOwnerOnNewRelease(version string) error {
	if w.cl.owner == w.owner {
		return fmt.Errorf("owner must move off the original owner %q onto a new-release node, still %q", w.owner, w.cl.owner)
	}
	node, ok := w.nodes[w.cl.owner]
	if !ok {
		return fmt.Errorf("cluster owner %q has no fake node", w.cl.owner)
	}
	if !strings.Contains(node.cluFakeHost.current, version) {
		return fmt.Errorf("new owner %q must run the new release %s; junction=%q", w.cl.owner, version, node.cluFakeHost.current)
	}
	if w.cl.state != "Online" {
		return fmt.Errorf("role must be Online after a healthy update, state=%q", w.cl.state)
	}
	return nil
}

func (w *cluWorld) thenEverySuccessManifest(version string) error {
	for _, n := range w.order {
		if !strings.Contains(n.cluFakeHost.current, version) {
			return fmt.Errorf("host %s must run new release %s; junction=%q", n.host, version, n.cluFakeHost.current)
		}
		m := string(n.cluFakeHost.files[w.manifestPath()])
		if !strings.Contains(m, `"result": "success"`) || !strings.Contains(m, `"current_version": "`+version+`"`) {
			return fmt.Errorf("host %s must record a success manifest at %s: %q", n.host, version, m)
		}
	}
	return nil
}

func (w *cluWorld) thenUpdateRollsBack(version string) error {
	if w.err == nil {
		return fmt.Errorf("a firstNew health failure at U5 must roll back and surface the original error")
	}
	if !strings.Contains(w.err.Error(), "rolled back to "+version) {
		return fmt.Errorf("expected rollback-to-previous error, got: %v", w.err)
	}
	if cluCountStep(w.steps, "MOVE_GROUP") != 2 {
		return fmt.Errorf("firstNew rollback must emit exactly 2 MOVE_GROUP (U4 forward + R1 back), got %d: %v",
			cluCountStep(w.steps, "MOVE_GROUP"), cluStepNames(w.steps))
	}
	if cluCountStep(w.steps, "ROLLBACK") == 0 {
		return fmt.Errorf("expected a ROLLBACK step record: %v", cluStepNames(w.steps))
	}
	return nil
}

func (w *cluWorld) thenRoleBackToOwner(owner string) error {
	if w.cl.owner != strings.ToLower(owner) {
		return fmt.Errorf("role must return to the original owner %s after rollback, got owner=%q", owner, w.cl.owner)
	}
	return nil
}

func (w *cluWorld) thenBothJunctionPrev(version string) error {
	for _, n := range w.order {
		if !strings.Contains(n.cluFakeHost.current, version) {
			return fmt.Errorf("host %s junction must be restored to previous version %s; junction=%q",
				n.host, version, n.cluFakeHost.current)
		}
	}
	return nil
}

func (w *cluWorld) thenBothRolledBack(version string) error {
	for _, n := range w.order {
		m := string(n.cluFakeHost.files[w.manifestPath()])
		if !strings.Contains(m, `"result": "rolled_back"`) {
			return fmt.Errorf("host %s manifest must record result=rolled_back: %q", n.host, m)
		}
		if !strings.Contains(m, `"current_version": "`+version+`"`) {
			return fmt.Errorf("host %s rolled_back manifest must record current_version=%s: %q", n.host, version, m)
		}
		if strings.Contains(m, `"result": "success"`) {
			return fmt.Errorf("host %s must NOT record a success manifest after rollback: %q", n.host, m)
		}
	}
	return nil
}

// cluUpdateSpec builds a cluster_generic_service update Deployment over an
// explicit host list (no preferred owner ⇒ no U7 second failover) to the given
// version, with a short settle to keep the scenario fast.
func cluUpdateSpec(hosts []string, url, checksum, version string) (*spec.Deployment, error) {
	hostList := `["` + strings.Join(hosts, `", "`) + `"]`
	y := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ` + hostList + `
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: zip
  version: ` + version + `
  checksum: "` + checksum + `"
  source: { type: http, url: "` + url + `" }
pattern:
  type: cluster_generic_service
  service_name: SampleSvc
  role_name: SampleRole
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
`
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return d, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cluEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func InitializeScenario_failover_cluster_support_cluster_rolling_update_and_rollback_engine(ctx *godog.ScenarioContext) {
	w := &cluWorld{nodes: map[string]*cluNode{}}

	ctx.Before(func(c context.Context, sc *godog.Scenario) (context.Context, error) {
		w.reset()
		return c, nil
	})
	ctx.After(func(c context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if w.srv != nil {
			w.srv.Close()
			w.srv = nil
		}
		return c, nil
	})

	ctx.Step(`^a (\d+)-node cluster fake transport with owner "([^"]*)" running release "([^"]*)"$`, w.givenClusterOwnerRunning)
	ctx.Step(`^the cluster hosts are "([^"]*)" with no preferred owner$`, w.givenClusterHosts)
	ctx.Step(`^the health check fails on firstNew "([^"]*)" while it runs the new release$`, w.givenHealthFailsOnFirstNew)
	ctx.Step(`^a rolling UPDATE to "([^"]*)" runs$`, w.whenUpdate)

	ctx.Step(`^the update succeeds$`, w.thenUpdateSucceeds)
	ctx.Step(`^the passives are updated in hosts order "([^"]*)" before the failover$`, w.thenPassivesUpdatedInOrder)
	ctx.Step(`^exactly one MOVE_GROUP occurs$`, w.thenExactlyOneMove)
	ctx.Step(`^the role owner ends on a node running the new release "([^"]*)"$`, w.thenOwnerOnNewRelease)
	ctx.Step(`^every node records a success manifest at "([^"]*)"$`, w.thenEverySuccessManifest)

	ctx.Step(`^the update fails and rolls back to "([^"]*)"$`, w.thenUpdateRollsBack)
	ctx.Step(`^the role moves back to the original owner "([^"]*)"$`, w.thenRoleBackToOwner)
	ctx.Step(`^both nodes junction to the previous version "([^"]*)"$`, w.thenBothJunctionPrev)
	ctx.Step(`^both manifests read "rolled_back" at "([^"]*)"$`, w.thenBothRolledBack)
}

func TestE2E_failover_cluster_support_cluster_rolling_update_and_rollback_engine(t *testing.T) {
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_failover_cluster_support_cluster_rolling_update_and_rollback_engine,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"failover_cluster_support_cluster_rolling_update_and_rollback_engine.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: e2e scenarios failed")
	}
}
