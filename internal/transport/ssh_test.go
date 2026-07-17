package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// newHostKeySigner returns a fresh ed25519 SSH signer plus its public wire form.
func newHostKeySigner(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("new public key: %v", err)
	}
	return signer, sshPub
}

func splitPort(t *testing.T, addr string) (host string, port int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

// stubServer is an in-process SSH endpoint used to prove host-key verification,
// authentication classification, and SFTP transfer without a live VM. wantUser/
// wantPass gate password auth; when both are "" the server rejects every
// password (used to prove auth rejection maps to ERR_AUTH without retry).
type stubServer struct {
	ln       net.Listener
	cfg      *ssh.ServerConfig
	conns    int32 // count of accepted TCP connections (retry detector)
	wg       sync.WaitGroup
	wantUser string
	wantPass string
}

func startStubServer(t *testing.T, hostSigner ssh.Signer, wantUser, wantPass string) *stubServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &stubServer{ln: ln, wantUser: wantUser, wantPass: wantPass}
	s.cfg = &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if wantUser != "" && c.User() == wantUser && string(pass) == wantPass {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("password rejected")
		},
	}
	s.cfg.AddHostKey(hostSigner)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&s.conns, 1)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *stubServer) addr() string   { return s.ln.Addr().String() }
func (s *stubServer) connCount() int { return int(atomic.LoadInt32(&s.conns)) }

// handle completes the SSH handshake and serves the SFTP subsystem on any
// session channel that requests it. Handshake/auth failures are expected in the
// mismatch/auth-rejection tests and are simply discarded.
func (s *stubServer) handle(conn net.Conn) {
	defer conn.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.cfg)
	if err != nil {
		return // host-key rejected by client, or auth rejected — expected
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func(in <-chan *ssh.Request, ch ssh.Channel) {
			for req := range in {
				ok := false
				// subsystem payload = uint32 length prefix + name.
				if req.Type == "subsystem" && len(req.Payload) >= 4 &&
					string(req.Payload[4:]) == "sftp" {
					ok = true
				}
				_ = req.Reply(ok, nil)
				if ok {
					srv, err := sftp.NewServer(ch)
					if err == nil {
						_ = srv.Serve()
						_ = srv.Close()
					}
					_ = ch.Close()
					return
				}
			}
		}(chReqs, ch)
	}
}

// Scenario: Host key mismatch mapping — Given a pinned WRONG ssh.host_key, when
// Connect runs against a stub server presenting a DIFFERENT key, the error is
// ERR_CONNECT with detail "host key mismatch" (in-process proof; DESIGN §8.1,
// implementation-plan Stage 2.3).
func TestSSHConnectHostKeyMismatch(t *testing.T) {
	serverSigner, _ := newHostKeySigner(t)
	_, wrongPub := newHostKeySigner(t) // an unrelated key → guaranteed mismatch

	srv := startStubServer(t, serverSigner, "deploy", "secret")
	host, port := splitPort(t, srv.addr())

	pinned := base64.StdEncoding.EncodeToString(wrongPub.Marshal())
	// retries=0 so a (correctly non-retried) mismatch never sleeps on backoff.
	tr := newSSH(host, port, "deploy", "secret", "", pinned, 5, 0, spec.OSLinux)

	err := tr.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect must fail when the presented host key does not match the pin")
	}
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CodedError, got %T: %v", err, err)
	}
	if ce.Code != "ERR_CONNECT" {
		t.Fatalf("host key mismatch must map to ERR_CONNECT, got %s (%v)", ce.Code, err)
	}
	if !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("error detail must contain %q, got %q", "host key mismatch", err.Error())
	}
}

// Scenario: a malformed (non-base64) pin is rejected at Connect as ERR_CONNECT
// before any dial is attempted.
func TestSSHConnectBadPinnedKey(t *testing.T) {
	tr := newSSH("127.0.0.1", 22, "deploy", "secret", "", "not-base64!!!", 5, 0, spec.OSLinux)
	err := tr.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect must fail on a malformed ssh.host_key pin")
	}
	var ce *CodedError
	if !errors.As(err, &ce) || ce.Code != "ERR_CONNECT" {
		t.Fatalf("malformed pin must map to ERR_CONNECT, got %v", err)
	}
}

// Scenario: auth rejection maps to ERR_AUTH and is NOT retried. Even with
// retries=3, a rejected password must produce exactly ONE dial (DESIGN §8.1:
// "Auth rejection ... is NOT retried ⇒ ERR_AUTH").
func TestSSHConnectAuthRejectedNotRetried(t *testing.T) {
	serverSigner, serverPub := newHostKeySigner(t)
	// Server only accepts deploy/right; the client offers a wrong password.
	srv := startStubServer(t, serverSigner, "deploy", "right")
	host, port := splitPort(t, srv.addr())

	pinned := base64.StdEncoding.EncodeToString(serverPub.Marshal())
	tr := newSSH(host, port, "deploy", "wrong", "", pinned, 5, 3, spec.OSLinux)

	err := tr.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect must fail when the password is rejected")
	}
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CodedError, got %T: %v", err, err)
	}
	if ce.Code != "ERR_AUTH" {
		t.Fatalf("auth rejection must map to ERR_AUTH, got %s (%v)", ce.Code, err)
	}
	if got := srv.connCount(); got != 1 {
		t.Fatalf("auth rejection retried: server saw %d connections, want exactly 1", got)
	}
}

// Scenario (in-process portion of the SFTP round-trip): with host-key pin
// satisfied and password accepted, Upload streams a payload over the SFTP
// subsystem and Download reads it back byte-for-byte. Proves Upload/Download
// and the successful FixedHostKey pin path (DESIGN §8.1 SSH upload/download).
func TestSSHUploadDownloadRoundTrip(t *testing.T) {
	serverSigner, serverPub := newHostKeySigner(t)
	srv := startStubServer(t, serverSigner, "deploy", "secret")
	host, port := splitPort(t, srv.addr())

	pinned := base64.StdEncoding.EncodeToString(serverPub.Marshal())
	tr := newSSH(host, port, "deploy", "secret", "", pinned, 5, 0, spec.OSLinux)

	ctx := context.Background()
	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("Connect over in-process ssh/sftp: %v", err)
	}
	defer tr.Close()

	dir := t.TempDir()
	// The in-process sftp server serves the real filesystem; use OS-native paths.
	remote := filepath.Join(dir, "payload.bin")
	local := filepath.Join(dir, "roundtrip.bin")
	want := []byte("release-provider sftp round-trip \x00\x01\x02 data")

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
		t.Fatalf("sftp round-trip mismatch: got %q want %q", got, want)
	}
}
