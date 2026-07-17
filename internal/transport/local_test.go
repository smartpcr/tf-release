package transport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// gateOS returns the spec.OSKind matching the host the tests run on.
func gateOS() spec.OSKind {
	if runtime.GOOS == "windows" {
		return spec.OSWindows
	}
	return spec.OSLinux
}

// otherOS returns a spec.OSKind that never matches the gate host.
func otherOS() spec.OSKind {
	if runtime.GOOS == "windows" {
		return spec.OSLinux
	}
	return spec.OSWindows
}

func localTarget(os spec.OSKind) *spec.Target {
	return &spec.Target{
		Transport: spec.TransportLocal,
		Hosts:     []string{"localhost"},
		OS:        os,
	}
}

// Scenario: Local round-trip — Exec + Upload + Download on the gate OS. A file
// survives the round-trip and Exec surfaces the app exit code in Result while
// returning a nil transport error.
func TestLocalRoundTrip(t *testing.T) {
	tr, err := NewTransport(localTarget(gateOS()), "localhost")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("Connect on gate OS should succeed: %v", err)
	}
	defer tr.Close()

	if got := tr.OS(); got != gateOS() {
		t.Fatalf("OS()=%s want %s", got, gateOS())
	}
	if tr.Host() != "localhost" {
		t.Fatalf("Host()=%s want localhost", tr.Host())
	}

	dir := t.TempDir()
	remote := filepath.Join(dir, "sub", "payload.bin")
	local := filepath.Join(dir, "roundtrip.bin")
	want := []byte("release-provider round-trip \x00\x01\x02 data")

	if err := tr.Upload(ctx, bytes.NewReader(want), int64(len(want)), remote); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := tr.Download(ctx, remote, local); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}

	// Exec returns the application exit code in Result, not as a transport error.
	var okCmd, failCmd Cmd
	if gateOS() == spec.OSWindows {
		okCmd = Cmd{Shell: ShellCmd, Script: "exit 0"}
		failCmd = Cmd{Shell: ShellCmd, Script: "exit 7"}
	} else {
		okCmd = Cmd{Shell: ShellSh, Script: "exit 0"}
		failCmd = Cmd{Shell: ShellSh, Script: "exit 7"}
	}

	res, err := tr.Exec(ctx, okCmd)
	if err != nil {
		t.Fatalf("Exec ok: unexpected transport error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec ok: ExitCode=%d want 0", res.ExitCode)
	}

	res, err = tr.Exec(ctx, failCmd)
	if err != nil {
		t.Fatalf("Exec fail: non-zero app exit must NOT be a transport error, got %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("Exec fail: ExitCode=%d want 7", res.ExitCode)
	}
}

// Scenario: OS mismatch rejected — Connect must error before executing any
// command when target.os does not match runtime.GOOS.
func TestLocalConnectOSMismatch(t *testing.T) {
	tr, err := NewTransport(localTarget(otherOS()), "localhost")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = tr.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect must fail when target.os != runtime.GOOS")
	}
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CodedError, got %T: %v", err, err)
	}
	if ce.Code != "ERR_CONNECT" {
		t.Fatalf("mismatch should map to ERR_CONNECT, got %s", ce.Code)
	}
}

// The NewTransport factory dispatches on transport kind and rejects unknown
// kinds — this is the seam reused by the engine's fake-transport tests.
func TestNewTransportFactoryDispatch(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		tr, err := NewTransport(localTarget(gateOS()), "localhost")
		if err != nil {
			t.Fatalf("New local: %v", err)
		}
		if _, ok := tr.(*localTransport); !ok {
			t.Fatalf("expected *localTransport, got %T", tr)
		}
	})

	t.Run("ssh", func(t *testing.T) {
		tgt := &spec.Target{
			Transport:   spec.TransportSSH,
			Hosts:       []string{"host-a"},
			OS:          spec.OSLinux,
			Credentials: spec.Credentials{Username: "deploy"},
		}
		tr, err := NewTransport(tgt, "host-a")
		if err != nil {
			t.Fatalf("New ssh: %v", err)
		}
		if _, ok := tr.(*sshTransport); !ok {
			t.Fatalf("expected *sshTransport, got %T", tr)
		}
		if tr.OS() != spec.OSLinux {
			t.Fatalf("ssh OS()=%s want linux", tr.OS())
		}
	})

	t.Run("winrm", func(t *testing.T) {
		tgt := &spec.Target{
			Transport:   spec.TransportWinRM,
			Hosts:       []string{"host-b"},
			OS:          spec.OSWindows,
			Credentials: spec.Credentials{Username: "admin", PasswordEnv: "PW"},
		}
		tr, err := NewTransport(tgt, "host-b")
		if err != nil {
			t.Fatalf("New winrm: %v", err)
		}
		if _, ok := tr.(*winrmTransport); !ok {
			t.Fatalf("expected *winrmTransport, got %T", tr)
		}
	})

	t.Run("winrm requires windows os", func(t *testing.T) {
		tgt := &spec.Target{
			Transport: spec.TransportWinRM,
			Hosts:     []string{"host-c"},
			OS:        spec.OSLinux,
		}
		if _, err := NewTransport(tgt, "host-c"); err == nil {
			t.Fatal("winrm with os=linux must be rejected")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		tgt := &spec.Target{Transport: spec.TransportKind("carrier-pigeon"), Hosts: []string{"x"}}
		if _, err := NewTransport(tgt, "x"); err == nil {
			t.Fatal("unknown transport kind must be rejected")
		}
	})
}

