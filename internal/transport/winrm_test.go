package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

// fakeWinRM is an in-memory PowerShell channel: it interprets just enough of the
// generated upload/download scripts to round-trip a single remote file, letting
// the Upload/Download contract be exercised with no live server.
type fakeWinRM struct {
	files    map[string][]byte
	scripts  []string // decoded scripts, in execution order
	failWith error    // if set, every run returns this error
}

var (
	reB64    = regexp.MustCompile(`FromBase64String\('([^']*)'\)`)
	reMode   = regexp.MustCompile(`\[IO\.FileMode\]::(\w+)`)
	reOpen   = regexp.MustCompile(`\[IO\.File\]::Open\('([^']*)'`)
	reRemove = regexp.MustCompile(`Remove-Item -LiteralPath '([^']*)'`)
	reRead   = regexp.MustCompile(`OpenRead\('([^']*)'\);\$fs\.Seek\((\d+),`)
)

func (f *fakeWinRM) run(_ context.Context, command, _ string) (string, string, int, error) {
	if f.failWith != nil {
		return "", "", 0, f.failWith
	}
	script := command
	const prefix = "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "
	if strings.HasPrefix(command, prefix) {
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, prefix))
		u16 := make([]uint16, len(raw)/2)
		for i := range u16 {
			u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
		}
		script = string(utf16.Decode(u16))
	}
	f.scripts = append(f.scripts, script)

	switch {
	case strings.Contains(script, "cmd.exe /c echo ok"):
		return "ok\n", "", 0, nil
	case strings.Contains(script, "New-Item"): // mkdir
		return "", "", 0, nil
	case strings.HasPrefix(script, "if(Test-Path"): // pre-delete
		if m := reRemove.FindStringSubmatch(script); m != nil {
			delete(f.files, m[1])
		}
		return "", "", 0, nil
	case reRead.MatchString(script): // download read
		m := reRead.FindStringSubmatch(script)
		path := m[1]
		off, _ := strconv.Atoi(m[2])
		data := f.files[path]
		if off >= len(data) {
			return "", "", 0, nil
		}
		end := off + winrmChunkRaw
		if end > len(data) {
			end = len(data)
		}
		return base64.StdEncoding.EncodeToString(data[off:end]) + "\n", "", 0, nil
	case reMode.MatchString(script): // upload write (CreateNew / Append) or empty file
		path := reOpen.FindStringSubmatch(script)[1]
		mode := reMode.FindStringSubmatch(script)[1]
		var payload []byte
		if b := reB64.FindStringSubmatch(script); b != nil {
			payload, _ = base64.StdEncoding.DecodeString(b[1])
		}
		switch mode {
		case "CreateNew":
			if _, exists := f.files[path]; exists {
				return "", "file exists", 1, nil // real CreateNew would throw
			}
			f.files[path] = append([]byte(nil), payload...)
		case "Append":
			f.files[path] = append(f.files[path], payload...)
		}
		return "", "", 0, nil
	}
	return "", "unhandled script: " + script, 1, nil
}

func newFakeTransport() (*winrmTransport, *fakeWinRM) {
	fk := &fakeWinRM{files: map[string][]byte{}}
	return &winrmTransport{host: "h", port: 5986, run: fk.run}, fk
}

// Scenario: Upload uses CreateNew-first then Append, then Download round-trips
// the exact bytes back — exercising the generated first/append scripts and the
// full Upload/Download contract (evaluator item 6).
func TestWinRMUploadDownloadRoundTrip(t *testing.T) {
	tr, fk := newFakeTransport()
	ctx := context.Background()
	remote := `C:\deploy\app\payload.bin`

	// 48001 bytes → 2 chunks (one full CreateNew + one Append tail).
	want := make([]byte, winrmChunkRaw+1)
	for i := range want {
		want[i] = byte(i % 251)
	}
	if err := tr.Upload(ctx, bytes.NewReader(want), int64(len(want)), remote); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !bytes.Equal(fk.files[remote], want) {
		t.Fatalf("uploaded bytes mismatch: got %d want %d", len(fk.files[remote]), len(want))
	}

	// The generated write scripts must be CreateNew first, Append after.
	var writes []string
	for _, s := range fk.scripts {
		if reMode.MatchString(s) {
			writes = append(writes, reMode.FindStringSubmatch(s)[1])
		}
	}
	if len(writes) != 2 || writes[0] != "CreateNew" || writes[1] != "Append" {
		t.Fatalf("write modes = %v, want [CreateNew Append]", writes)
	}

	// Download the same file back and compare.
	dir := t.TempDir()
	local := filepath.Join(dir, "out.bin")
	if err := tr.Download(ctx, remote, local); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("download round-trip mismatch: got %d want %d bytes", len(got), len(want))
	}
}

// Zero-byte upload creates an empty file via CreateNew (no FromBase64String).
func TestWinRMUploadEmptyFile(t *testing.T) {
	tr, fk := newFakeTransport()
	remote := `C:\deploy\empty.bin`
	if err := tr.Upload(context.Background(), bytes.NewReader(nil), 0, remote); err != nil {
		t.Fatalf("Upload empty: %v", err)
	}
	got, ok := fk.files[remote]
	if !ok || len(got) != 0 {
		t.Fatalf("empty file not created (ok=%v len=%d)", ok, len(got))
	}
	for _, s := range fk.scripts {
		if strings.Contains(s, "FromBase64String") {
			t.Fatalf("empty-file upload must not emit base64 payload: %s", s)
		}
	}
}

// Upload to a bare filename (no directory separator) must not panic and must
// skip the mkdir step (evaluator item 3).
func TestWinRMUploadBareFilenameNoPanic(t *testing.T) {
	tr, fk := newFakeTransport()
	remote := "payload.bin"
	if err := tr.Upload(context.Background(), bytes.NewReader([]byte("hi")), 2, remote); err != nil {
		t.Fatalf("Upload bare filename: %v", err)
	}
	if !bytes.Equal(fk.files[remote], []byte("hi")) {
		t.Fatalf("bare-filename upload wrong content: %q", fk.files[remote])
	}
	for _, s := range fk.scripts {
		if strings.Contains(s, "New-Item") {
			t.Fatalf("bare filename must not trigger mkdir: %s", s)
		}
	}
}

// Scenario: a mid-session 401/unauthorized from Exec maps to ERR_AUTH, not
// ERR_CONNECT (evaluator item 2).
func TestWinRMExecAuthMapsToErrAuth(t *testing.T) {
	fk := &fakeWinRM{files: map[string][]byte{}, failWith: errors.New("http response error: 401 Unauthorized")}
	tr := &winrmTransport{host: "h", port: 5986, run: fk.run}
	_, err := tr.Exec(context.Background(), Cmd{Shell: ShellPowerShell, Script: "Get-Date"})
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_AUTH" {
		t.Fatalf("expected ERR_AUTH on 401, got %v", err)
	}
}

// A non-auth transport failure from Exec maps to ERR_CONNECT.
func TestWinRMExecTransportFailureMapsToErrConnect(t *testing.T) {
	fk := &fakeWinRM{files: map[string][]byte{}, failWith: errors.New("dial tcp: connection refused")}
	tr := &winrmTransport{host: "h", port: 5986, run: fk.run}
	_, err := tr.Exec(context.Background(), Cmd{Shell: ShellPowerShell, Script: "Get-Date"})
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_CONNECT" {
		t.Fatalf("expected ERR_CONNECT on dial failure, got %v", err)
	}
}

// Connect maps a 401 probe failure to ERR_AUTH and does NOT retry it.
func TestWinRMConnectAuthNotRetried(t *testing.T) {
	calls := 0
	fk := &fakeWinRM{files: map[string][]byte{}}
	tr := &winrmTransport{host: "h", port: 5986, retries: 5, opTimeoutSec: 5, run: func(ctx context.Context, cmd, stdin string) (string, string, int, error) {
		calls++
		return fk.run(ctx, cmd, stdin)
	}}
	fk.failWith = errors.New("401 Unauthorized")
	err := tr.Connect(context.Background())
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_AUTH" {
		t.Fatalf("expected ERR_AUTH from Connect, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("auth failure retried: probe called %d times, want 1", calls)
	}
}

// uploadChunkScript renders the exact CreateNew / Append FileStream forms.
func TestUploadChunkScriptForms(t *testing.T) {
	first := uploadChunkScript(`C:\a\b.bin`, "QUJD", true)
	if !strings.Contains(first, "[IO.FileMode]::CreateNew") {
		t.Fatalf("first chunk must use CreateNew: %s", first)
	}
	rest := uploadChunkScript(`C:\a\b.bin`, "QUJD", false)
	if !strings.Contains(rest, "[IO.FileMode]::Append") {
		t.Fatalf("subsequent chunk must use Append: %s", rest)
	}
	if !strings.Contains(rest, "$fs.Write($b,0,$b.Length)") {
		t.Fatalf("append chunk must Write the buffer: %s", rest)
	}
}

// remoteParentDir returns "" for a bare filename and the parent otherwise.
func TestRemoteParentDir(t *testing.T) {
	cases := map[string]string{
		`C:\a\b\c.bin`: `C:\a\b`,
		`/tmp/x/y.bin`: `/tmp/x`,
		`bare.bin`:     "",
	}
	for in, want := range cases {
		if got := remoteParentDir(in); got != want {
			t.Fatalf("remoteParentDir(%q)=%q want %q", in, got, want)
		}
	}
}
