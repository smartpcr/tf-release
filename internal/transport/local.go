package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

type localTransport struct{ osKind spec.OSKind }

func newLocal(osKind spec.OSKind) *localTransport { return &localTransport{osKind: osKind} }

func (l *localTransport) OS() spec.OSKind { return l.osKind }
func (l *localTransport) Host() string    { return "localhost" }
func (l *localTransport) Close() error    { return nil }

func (l *localTransport) Connect(ctx context.Context) error {
	// Verify the declared spec OS matches the actual runner OS (DESIGN §8.1).
	actual := spec.OSLinux
	if runtime.GOOS == "windows" {
		actual = spec.OSWindows
	}
	if actual != l.osKind {
		return ErrConnect(fmt.Errorf("transport local: runner os %s != target.os %s", actual, l.osKind))
	}
	return nil
}

func (l *localTransport) Exec(ctx context.Context, c Cmd) (Result, error) {
	if c.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.TimeoutSec)*time.Second)
		defer cancel()
	}
	var cmd *exec.Cmd
	switch c.Shell {
	case ShellPowerShell:
		line, err := BuildCommandLine(spec.OSWindows, c)
		if err != nil {
			return Result{}, err
		}
		// line = "powershell.exe -NoProfile ... -EncodedCommand XXX"
		cmd = exec.CommandContext(ctx, "powershell.exe",
			"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-EncodedCommand", line[len(line)-encLen(line):])
	case ShellSh:
		// Inject env via the child process environment (correct expansion),
		// not a string prefix inside the script (DESIGN §8.1).
		cmd = exec.CommandContext(ctx, "sh", "-c", c.Script)
		cmd.Env = append(os.Environ(), envKV(c.Env)...)
	case ShellCmd:
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", c.Script)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("local exec: %w", err)
	}
	return res, nil
}

// encLen extracts the -EncodedCommand payload length from a built line.
func encLen(line string) int {
	i := len(line)
	for i > 0 && line[i-1] != ' ' {
		i--
	}
	return len(line) - i
}

func (l *localTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
		return err
	}
	f, err := os.Create(remote)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, local)
	return err
}

func (l *localTransport) Download(ctx context.Context, remote, local string) error {
	src, err := os.Open(remote)
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
