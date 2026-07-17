package transport

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// TestAccSSHRoundTrip is the lab-gated acceptance proof for Scenario 1
// ("SSH dial and SFTP round-trip"). A local shell cannot exercise live
// ssh/sftp, so this runs ONLY when TF_ACC=1 AND a real Linux sshd endpoint is
// supplied via env; otherwise it is skipped and never runs in the ordinary
// gate suite (DESIGN §8.1; test-scenario proof: lab, acceptance area L2).
//
// Required env when TF_ACC=1:
//
//	SSH_ACC_HOST      target host/IP running sshd
//	SSH_ACC_PORT      port (default 22)
//	SSH_ACC_USER      username
//	SSH_ACC_PASSWORD  password (or set SSH_ACC_PRIVATE_KEY)
//	SSH_ACC_PRIVATE_KEY  PEM private key (optional alternative to password)
//	SSH_ACC_HOST_KEY  optional base64 wire-format host key to pin ("" accepts any)
func TestAccSSHRoundTrip(t *testing.T) {
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("acceptance test; set TF_ACC=1 and SSH_ACC_* to run against a live sshd")
	}
	host := os.Getenv("SSH_ACC_HOST")
	if host == "" {
		t.Skip("SSH_ACC_HOST unset; skipping live ssh round-trip")
	}
	port := 22
	if p := os.Getenv("SSH_ACC_PORT"); p != "" {
		v, err := strconv.Atoi(p)
		if err != nil {
			t.Fatalf("SSH_ACC_PORT=%q: %v", p, err)
		}
		port = v
	}
	user := os.Getenv("SSH_ACC_USER")
	pass := os.Getenv("SSH_ACC_PASSWORD")
	key := os.Getenv("SSH_ACC_PRIVATE_KEY")
	pinned := os.Getenv("SSH_ACC_HOST_KEY")

	tr := newSSH(host, port, user, pass, key, pinned, 30, 2, spec.OSLinux)
	ctx := context.Background()
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer tr.Close()

	// Exec: app exit code is surfaced in Result, not as a transport error, and
	// the Linux env-prepend `K='V' ` reaches the remote environment.
	res, err := tr.Exec(ctx, Cmd{Shell: ShellSh, Script: `test "$FOO" = bar && echo ok`,
		Env: map[string]string{"FOO": "bar"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec ExitCode=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// SFTP round-trip: upload a payload and read it back byte-for-byte.
	remote := "/tmp/tf-release-acc.bin"
	want := []byte("release-provider live sftp round-trip \x00\x01\x02")
	if err := tr.Upload(ctx, bytes.NewReader(want), int64(len(want)), remote); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	local, err := os.CreateTemp(t.TempDir(), "dl-*.bin")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	local.Close()
	if err := tr.Download(ctx, remote, local.Name()); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(local.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}
}
