package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

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

// startStubSSHServer accepts a single connection and completes (or fails) the
// SSH handshake using hostSigner. It never authenticates a client — the test
// only exercises the host-key verification that happens during the handshake.
func startStubSSHServer(t *testing.T, hostSigner ssh.Signer) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(hostSigner)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// The handshake fails once the client rejects our host key; we
			// discard the resulting error and drop the connection.
			sconn, _, _, herr := ssh.NewServerConn(conn, cfg)
			if herr == nil && sconn != nil {
				sconn.Close()
			}
			_ = conn.Close()
		}
	}()

	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}

// Scenario: Host key mismatch mapping — Given a pinned WRONG ssh.host_key, when
// Connect runs against a stub server presenting a DIFFERENT key, the error is
// ERR_CONNECT with detail "host key mismatch" and no auth is attempted
// (in-process proof; DESIGN §8.1, implementation-plan Stage 2.3).
func TestSSHConnectHostKeyMismatch(t *testing.T) {
	serverSigner, _ := newHostKeySigner(t)
	// A second, unrelated key is what the client pins — guaranteeing a mismatch.
	_, wrongPub := newHostKeySigner(t)

	addr, stop := startStubSSHServer(t, serverSigner)
	defer stop()

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}

	pinned := base64.StdEncoding.EncodeToString(wrongPub.Marshal())
	// retries=0 so a (correctly non-retried) mismatch never sleeps on backoff.
	tr := newSSH(host, port, "deploy", "secret", "", pinned, 5, 0, spec.OSLinux)

	err = tr.Connect(context.Background())
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

// Scenario: an invalid (non-base64) pin is rejected at Connect as ERR_CONNECT
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
