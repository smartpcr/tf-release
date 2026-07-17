package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/masterzen/winrm"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

const winrmChunkRaw = 48000 // raw bytes/chunk (DESIGN §8.1) → ~64KB base64

type winrmTransport struct {
	host, user, pass string
	port             int
	https, insecure  bool
	opTimeoutSec     int
	retries          int
	client           *winrm.Client
}

func newWinRM(host string, port int, user, pass string, https, insecure bool, opTimeout, retries int) *winrmTransport {
	if opTimeout <= 0 {
		opTimeout = 60
	}
	return &winrmTransport{host: host, port: port, user: user, pass: pass,
		https: https, insecure: insecure, opTimeoutSec: opTimeout, retries: retries}
}

func (w *winrmTransport) OS() spec.OSKind { return spec.OSWindows }
func (w *winrmTransport) Host() string    { return w.host }

func (w *winrmTransport) Connect(ctx context.Context) error {
	ep := winrm.NewEndpoint(w.host, w.port, w.https, w.insecure, nil, nil, nil,
		time.Duration(w.opTimeoutSec)*time.Second)
	return retryConnect(ctx, w.retries, 5*time.Second, func() error {
		cli, err := winrm.NewClient(ep, w.user, w.pass)
		if err != nil {
			return noRetry(ErrConnect(err))
		}
		// probe: cheap command validates both reachability and credentials
		_, _, code, err := cli.RunWithContextWithString(ctx, "cmd.exe /c echo ok", "")
		if err == nil && code == 0 {
			w.client = cli
			return nil
		}
		if err != nil && isAuthErr(err) {
			return noRetry(ErrAuth(err)) // never retried (DESIGN §8.1)
		}
		if err != nil {
			return err // retryable transport failure
		}
		return fmt.Errorf("probe exit code %d", code)
	})
}

func isAuthErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "access is denied")
}

func (w *winrmTransport) Close() error { w.client = nil; return nil }

func (w *winrmTransport) Exec(ctx context.Context, c Cmd) (Result, error) {
	if w.client == nil {
		return Result{}, ErrConnect(fmt.Errorf("not connected"))
	}
	line, err := BuildCommandLine(spec.OSWindows, c)
	if err != nil {
		return Result{}, err
	}
	if c.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.TimeoutSec)*time.Second)
		defer cancel()
	}
	stdout, stderr, code, err := w.client.RunWithContextWithString(ctx, line, "")
	if err != nil {
		return Result{}, ErrConnect(fmt.Errorf("winrm exec: %w", err))
	}
	return Result{ExitCode: code, Stdout: stdout, Stderr: stderr}, nil
}

// uploadChunk describes one base64 append operation. Offset/Len are the byte
// window of the source payload this chunk carries; First marks the chunk that
// CREATES the remote file (WriteAllBytes) vs. later chunks that APPEND via a
// FileStream (DESIGN §8.1).
type uploadChunk struct {
	Offset int
	Len    int
	First  bool
}

// planUploadChunks derives the fixed 48,000-raw-byte chunk windows for a payload
// of the given size. A zero-byte payload yields NO data chunks (the empty file
// is created separately); every other size yields ceil(size/48000) chunks whose
// offsets step by 48,000 and whose final chunk carries the remainder. This is
// the single source of truth for the chunk math exercised by the golden tests.
func planUploadChunks(size int) []uploadChunk {
	if size <= 0 {
		return nil
	}
	chunks := make([]uploadChunk, 0, (size+winrmChunkRaw-1)/winrmChunkRaw)
	for off := 0; off < size; off += winrmChunkRaw {
		n := winrmChunkRaw
		if off+n > size {
			n = size - off
		}
		chunks = append(chunks, uploadChunk{Offset: off, Len: n, First: off == 0})
	}
	return chunks
}

// uploadChunkScript renders the PowerShell for one chunk. The first chunk
// creates the file with WriteAllBytes; subsequent chunks open a FileStream in
// Append mode and Write (DESIGN §8.1).
func uploadChunkScript(remote, b64 string, first bool) string {
	if first {
		return fmt.Sprintf(
			`[IO.File]::WriteAllBytes(%s,[Convert]::FromBase64String(%s))`,
			psq(remote), psq(b64))
	}
	return fmt.Sprintf(
		`$b=[Convert]::FromBase64String(%s);$fs=[IO.File]::Open(%s,'Append');$fs.Write($b,0,$b.Length);$fs.Close()`,
		psq(b64), psq(remote))
}

// emptyFileScript creates a zero-byte remote file.
func emptyFileScript(remote string) string {
	return fmt.Sprintf(`[IO.File]::WriteAllBytes(%s,[byte[]]@())`, psq(remote))
}

// Upload streams local → remote path via base64 chunk appends (DESIGN §8.1).
func (w *winrmTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	dir := remote[:strings.LastIndexAny(remote, `\/`)]
	mk := fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null`, psq(dir))
	if r, err := w.Exec(ctx, Cmd{Shell: ShellPowerShell, Script: mk}); err != nil || r.ExitCode != 0 {
		return fmt.Errorf("mkdir %s: exit=%d err=%v stderr=%s", dir, r.ExitCode, err, r.Stderr)
	}

	chunks := planUploadChunks(int(size))
	if len(chunks) == 0 { // zero-byte file
		if r, err := w.Exec(ctx, Cmd{Shell: ShellPowerShell, Script: emptyFileScript(remote)}); err != nil || r.ExitCode != 0 {
			return fmt.Errorf("create empty %s: err=%v stderr=%s", remote, err, r.Stderr)
		}
		return nil
	}

	buf := make([]byte, winrmChunkRaw)
	for _, ch := range chunks {
		if _, err := io.ReadFull(local, buf[:ch.Len]); err != nil {
			return fmt.Errorf("read upload payload at offset %d: %w", ch.Offset, err)
		}
		b64 := base64.StdEncoding.EncodeToString(buf[:ch.Len])
		script := uploadChunkScript(remote, b64, ch.First)
		r, err := w.Exec(ctx, Cmd{Shell: ShellPowerShell, Script: script, TimeoutSec: 120})
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("upload chunk to %s: %s", remote, r.Stderr)
		}
	}
	return nil
}

// Download reads remote file as base64 chunks over stdout.
func (w *winrmTransport) Download(ctx context.Context, remote, local string) error {
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	defer f.Close()
	offset := int64(0)
	for {
		script := fmt.Sprintf(`$fs=[IO.File]::OpenRead(%s);$fs.Seek(%d,'Begin')|Out-Null;`+
			`$b=New-Object byte[] %d;$n=$fs.Read($b,0,%d);$fs.Close();`+
			`if($n -gt 0){[Convert]::ToBase64String($b,0,$n)}`,
			psq(remote), offset, winrmChunkRaw, winrmChunkRaw)
		r, err := w.Exec(ctx, Cmd{Shell: ShellPowerShell, Script: script, TimeoutSec: 120})
		if err != nil {
			return err
		}
		if r.ExitCode != 0 {
			return fmt.Errorf("download %s: %s", remote, r.Stderr)
		}
		payload := strings.TrimSpace(r.Stdout)
		if payload == "" {
			break
		}
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return fmt.Errorf("download decode: %w", err)
		}
		if _, err := io.Copy(f, bytes.NewReader(raw)); err != nil {
			return err
		}
		offset += int64(len(raw))
		if len(raw) < winrmChunkRaw {
			break
		}
	}
	return nil
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
