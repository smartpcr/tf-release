//go:build e2e

package e2e

// rdFakeHost is a script-dispatching virtual Windows target (DESIGN §17 "fake
// transport"), adapted from the engine's own package-private fakeHost so this
// out-of-package acceptance suite can drive the REAL engine (ReadStatus/Deploy) over
// a scripted Result queue rather than injecting statuses. It emulates just enough of
// the PowerShell surface the engine emits for the Stage 5.3 Read/apply paths: manifest
// read, service status probe, and the full happy-path deploy steps. Names are
// rd*-prefixed so the shared `e2e` package has no symbol collisions with sibling stages.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

type rdFakeHost struct {
	host    string
	files   map[string][]byte // path -> content
	dirs    map[string]bool
	dirTS   map[string]int64
	current string          // junction target
	svc     string          // "", "Stopped", "Running"
	fail    map[string]bool // step toggles, e.g. "connect"
	log     []string
}

func newRDFakeHost(name string) *rdFakeHost {
	return &rdFakeHost{host: name, files: map[string][]byte{}, dirs: map[string]bool{},
		dirTS: map[string]int64{}, fail: map[string]bool{}}
}

func (f *rdFakeHost) mark(s string) { f.log = append(f.log, s) }

func (f *rdFakeHost) Connect(ctx context.Context) error {
	if f.fail["connect"] {
		// Emit the SAME coded dial failure a real SSH/WinRM transport returns when the
		// TCP dial is refused, so ReadStatus classifies it through the real
		// wrapTransportErr taxonomy into an ERR_CONNECT diagnostic (DESIGN §10.4).
		return transport.ErrConnect(fmt.Errorf("dial tcp %s:5986: connect: connection refused", f.host))
	}
	return nil
}
func (f *rdFakeHost) Close() error    { return nil }
func (f *rdFakeHost) OS() spec.OSKind { return spec.OSWindows }
func (f *rdFakeHost) Host() string    { return f.host }

var rdReRead = regexp.MustCompile(`ReadAllBytes\('([^']+)'\)`)
var rdReWrite = regexp.MustCompile(`WriteAllBytes\('([^']+)',\[Convert\]::FromBase64String\('([^']*)'\)\)`)
var rdReLock = regexp.MustCompile(`\[IO\.File\]::Move\(\$tmp,'([^']+)'\)`)
var rdReRmRecurse = regexp.MustCompile(`Remove-Item -Recurse -Force -ErrorAction SilentlyContinue '([^']+)'`)
var rdReRmOne = regexp.MustCompile(`Remove-Item -Force -ErrorAction SilentlyContinue '([^']+)'`)
var rdReMklink = regexp.MustCompile(`mklink /J "([^"]+)" "([^"]+)"`)
var rdReList = regexp.MustCompile(`Get-ChildItem -Directory '([^']+)'`)
var rdReCasDel = regexp.MustCompile(`\[IO\.File\]::Delete\('([^']+)'\)`)
var rdReCurEq = regexp.MustCompile(`\$cur -eq '([^']*)'`)
var rdReCasReplace = regexp.MustCompile(`\[IO\.File\]::Replace\(\$tmp,'([^']+)',`)
var rdReCurNe = regexp.MustCompile(`\$cur -ne '([^']*)'`)
var rdReB64 = regexp.MustCompile(`FromBase64String\('([^']*)'\)`)

func rdOK(out string) transport.Result { return transport.Result{ExitCode: 0, Stdout: out} }

func (f *rdFakeHost) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "$PSVersionTable"): // preflight
		f.mark("PREFLIGHT")
		return rdOK(""), nil

	case rdReCasReplace.MatchString(s): // casReplace atomic compare-and-replace
		path := rdReCasReplace.FindStringSubmatch(s)[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil
		}
		em := rdReCurNe.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil
		}
		if nm := rdReB64.FindStringSubmatch(s); nm != nil {
			raw, _ := base64.StdEncoding.DecodeString(nm[1])
			f.files[path] = raw
		}
		f.mark("LOCK")
		return rdOK(""), nil

	case rdReCasDel.MatchString(s): // casDelete compare-and-delete
		path := rdReCasDel.FindStringSubmatch(s)[1]
		cur, exists := f.files[path]
		if !exists {
			return transport.Result{ExitCode: 48}, nil
		}
		em := rdReCurEq.FindStringSubmatch(s)
		if em == nil || base64.StdEncoding.EncodeToString(cur) != em[1] {
			return transport.Result{ExitCode: 10}, nil
		}
		delete(f.files, path)
		return rdOK(""), nil

	case rdReLock.MatchString(s): // AcquireLock atomic create
		p := rdReLock.FindStringSubmatch(s)[1]
		if _, held := f.files[p]; held {
			return transport.Result{ExitCode: 48}, nil
		}
		if m := rdReB64.FindStringSubmatch(s); m != nil {
			raw, _ := base64.StdEncoding.DecodeString(m[1])
			f.files[p] = raw
		} else {
			f.files[p] = []byte("{}")
		}
		f.mark("LOCK")
		return rdOK(""), nil

	case rdReWrite.MatchString(s): // writeSmallFile (manifest, rendered files)
		m := rdReWrite.FindStringSubmatch(s)
		raw, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			return transport.Result{ExitCode: 1, Stderr: "b64"}, nil
		}
		f.files[m[1]] = raw
		return rdOK(""), nil

	case rdReRead.MatchString(s): // readSmallFile (manifest)
		p := rdReRead.FindStringSubmatch(s)[1]
		if f.fail["read"] {
			return transport.Result{}, fmt.Errorf("simulated manifest read transport failure")
		}
		raw, exists := f.files[p]
		if !exists {
			return transport.Result{ExitCode: 3}, nil // absent
		}
		return rdOK(base64.StdEncoding.EncodeToString(raw)), nil

	case strings.Contains(s, "Expand-Archive"): // extract
		f.mark("EXTRACT")
		return rdOK(""), nil

	case strings.Contains(s, "New-Item -ItemType Directory"): // ensureLayout
		return rdOK(""), nil

	case rdReMklink.MatchString(s): // switchJunction
		m := rdReMklink.FindStringSubmatch(s)
		f.current = m[2]
		f.mark("SWITCH->" + m[2])
		return rdOK(""), nil

	case strings.Contains(s, "sc.exe create") || strings.Contains(s, "sc.exe config"): // Configure
		f.mark("CONFIGURE")
		if f.svc == "" {
			f.svc = "Stopped"
		}
		return rdOK(""), nil

	case strings.Contains(s, "Stop-Service"): // Stop
		f.mark("STOP")
		if f.svc == "Running" {
			f.svc = "Stopped"
		}
		return rdOK(""), nil

	case strings.Contains(s, "taskkill /PID"): // FORCE_KILL escalation
		f.mark("FORCE_KILL")
		f.svc = "Stopped"
		return rdOK(""), nil

	case strings.Contains(s, "Start-Service"): // Start
		f.mark("START")
		f.svc = "Running"
		return rdOK(""), nil

	case strings.Contains(s, "exit 41"): // target-pull fetch (checksum marker)
		f.mark("FETCH")
		return rdOK(""), nil

	case strings.Contains(s, "Invoke-WebRequest"): // health http probe
		f.mark("HEALTH")
		return rdOK(""), nil

	case strings.Contains(s, "not_installed"): // windows_service Status probe
		if f.fail["status"] {
			return transport.Result{}, fmt.Errorf("simulated status probe transport failure")
		}
		if f.svc == "" {
			return rdOK("not_installed"), nil
		}
		return rdOK(strings.ToLower(f.svc)), nil

	case rdReList.MatchString(s): // prune listing
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
		return rdOK(b.String()), nil

	case rdReRmRecurse.MatchString(s):
		p := rdReRmRecurse.FindStringSubmatch(s)[1]
		for k := range f.files {
			if strings.HasPrefix(k, p) {
				delete(f.files, k)
			}
		}
		f.mark("RM " + p)
		return rdOK(""), nil

	case rdReRmOne.MatchString(s): // ReleaseLock / staging cleanup
		delete(f.files, rdReRmOne.FindStringSubmatch(s)[1])
		return rdOK(""), nil

	case strings.Contains(s, "sc.exe query") && strings.Contains(s, "sc.exe delete"): // Uninstall
		f.mark("UNINSTALL")
		f.svc = ""
		return rdOK(""), nil

	case strings.Contains(s, "rmdir"): // removeJunction
		f.current = ""
		return rdOK(""), nil

	case strings.Contains(s, "SCM") || strings.Contains(s, "Get-WinEvent"):
		return rdOK("[]"), nil
	}
	f.mark("UNMATCHED<<" + rdFirstLine(s) + ">>")
	return transport.Result{ExitCode: 0, Stdout: ""}, nil // benign default for aux scripts
}

func rdFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

func (f *rdFakeHost) Upload(ctx context.Context, r io.Reader, size int64, remotePath string) error {
	b, _ := io.ReadAll(r)
	f.files[remotePath] = b
	f.mark("UPLOAD " + remotePath)
	return nil
}

func (f *rdFakeHost) Download(ctx context.Context, remotePath, localPath string) error {
	return fmt.Errorf("download not needed in these scenarios")
}

var _ transport.Transport = (*rdFakeHost)(nil)
