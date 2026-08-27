//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 2.3 (SSH Transport
// and SFTP Transfer).
//
// Both scenarios exercise the REAL transport factory
// (internal/transport.NewTransport) building an SSH transport against an
// ephemeral, in-process stub sshd started in this test (crypto/ssh server +
// pkg/sftp subsystem). No docker, no lab VM, and no live SSH endpoint is
// required — the stub proves the same transport behaviour the plan describes:
//
//   - SSH dial and SFTP round-trip (proof:lab for a live VM; proven here
//     in-process because a local shell cannot exercise ssh/sftp): with the
//     host-key pin satisfied and the password accepted, Connect brings up the
//     ssh + sftp subsystem, Exec surfaces a distinctive NON-ZERO application
//     exit code (7) in Result with no transport error — proving ssh.go's
//     *ssh.ExitError/ExitStatus() surfacing path rather than a hard-coded 0 —
//     and Upload then Download round-trips a byte payload byte-for-byte. A
//     SECOND stub configured to reject the password is
//     dialed with connect_retries=3 and MUST map to ERR_AUTH after EXACTLY ONE
//     TCP connection — auth rejection is never retried (DESIGN §8.1).
//   - Host key mismatch mapping (in-process; deps: none): a wrong base64
//     ssh.host_key pin is set while the stub presents an unrelated key, and
//     Connect must return a *transport.CodedError coded ERR_CONNECT whose detail
//     contains "host key mismatch".
//
// Every Given/When/Then invokes the real transport and asserts on its result.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cucumber/godog"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// sshxferPassEnv is the env-var NAME the ssh transport resolves the password
// from (NewTransport reads secrets by name from the runner environment).
const sshxferPassEnv = "FORGE_E2E_SSH_XFER_PASSWORD"

// sshxferExecExitCode is the distinctive NON-ZERO application exit status the
// stub's exec handler reports on the wire. The round-trip scenario asserts Exec
// returns exactly this value in Result.ExitCode with a nil error, proving the
// ssh transport forwards the wire exit code (via *ssh.ExitError/ExitStatus())
// instead of hard-coding 0 or mapping an application exit to a transport error
// (DESIGN §8.1: "err = transport failure ONLY; app exit codes go in Result").
// Both the stub and the assertion reference this single constant so the two
// ends of the proof cannot silently drift apart.
const sshxferExecExitCode = 7

// --- ephemeral in-process stub sshd -----------------------------------------

// sshxferNewHostKeySigner returns a fresh ed25519 SSH signer plus its public
// wire form (used to pin / mispin the host key).
func sshxferNewHostKeySigner() (ssh.Signer, ssh.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return signer, sshPub, nil
}

// sshxferStub is an in-process SSH endpoint used to prove host-key
// verification, auth classification, and SFTP transfer without a live VM. When
// wantUser/wantPass are both non-empty the server accepts that credential; any
// other password is rejected (used to prove auth rejection maps to ERR_AUTH
// without retry). conns counts accepted TCP connections (retry detector).
type sshxferStub struct {
	ln       net.Listener
	cfg      *ssh.ServerConfig
	conns    int32
	wg       sync.WaitGroup
	wantUser string
	wantPass string
}

func sshxferStartStub(hostSigner ssh.Signer, wantUser, wantPass string) (*sshxferStub, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &sshxferStub{ln: ln, wantUser: wantUser, wantPass: wantPass}
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
	return s, nil
}

func (s *sshxferStub) addr() string   { return s.ln.Addr().String() }
func (s *sshxferStub) connCount() int { return int(atomic.LoadInt32(&s.conns)) }

func (s *sshxferStub) close() {
	_ = s.ln.Close()
	s.wg.Wait()
}

// handle completes the SSH handshake and serves session channels: an "exec"
// request replies success and returns a distinctive NON-ZERO exit-status
// (sshxferExecExitCode), so Exec is proven to surface that application exit
// code in Result.ExitCode with no transport error — exercising ssh.go's
// *ssh.ExitError/ExitStatus() path rather than a hard-coded 0. An sftp
// "subsystem" request serves the real filesystem via pkg/sftp (so
// Upload/Download round-trip). Handshake or auth failures are expected in the
// mismatch/auth-rejection paths and are discarded.
func (s *sshxferStub) handle(conn net.Conn) {
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
				switch {
				case req.Type == "exec":
					_ = req.Reply(true, nil)
					_, _ = ch.Write([]byte("ok\n"))
					_, _ = ch.SendRequest("exit-status", false,
						ssh.Marshal(struct{ Status uint32 }{sshxferExecExitCode}))
					_ = ch.Close()
					return
				case req.Type == "subsystem" && len(req.Payload) >= 4 &&
					string(req.Payload[4:]) == "sftp":
					_ = req.Reply(true, nil)
					srv, err := sftp.NewServer(ch)
					if err == nil {
						_ = srv.Serve()
						_ = srv.Close()
					}
					_ = ch.Close()
					return
				default:
					_ = req.Reply(false, nil)
				}
			}
		}(chReqs, ch)
	}
}

// sshxferSplitPort splits a "host:port" address into host and int port.
func sshxferSplitPort(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

// sshxferTarget builds a merged spec.Target for the ssh transport pointed at a
// stub endpoint. pinned is the base64 wire-format host-key pin ("" accepts any).
func sshxferTarget(host string, port int, pinned string, retries int) *spec.Target {
	r := retries
	return &spec.Target{
		Transport:      spec.TransportSSH,
		Hosts:          []string{host},
		OS:             spec.OSLinux,
		Port:           port,
		Credentials:    spec.Credentials{Username: "deploy", PasswordEnv: sshxferPassEnv},
		SSH:            spec.SSHOpts{HostKey: pinned, TimeoutSeconds: 5},
		ConnectRetries: &r,
	}
}

// --- scenario world ---------------------------------------------------------

type sshxferWorld struct {
	stub       *sshxferStub
	tr         transport.Transport
	connectErr error
}

func (w *sshxferWorld) reset() {
	// Close the transport (client) FIRST so the stub's in-flight sftp Serve
	// goroutine sees the channel close and returns; only then can the stub's
	// wg.Wait() in close() complete. Reversing this order deadlocks teardown.
	if w.tr != nil {
		_ = w.tr.Close()
		w.tr = nil
	}
	if w.stub != nil {
		w.stub.close()
		w.stub = nil
	}
	w.connectErr = nil
}

// --- Scenario: SSH dial and SFTP round-trip ---------------------------------

func (w *sshxferWorld) stubPinnedCorrectly() error {
	if err := os.Setenv(sshxferPassEnv, "secret"); err != nil {
		return err
	}
	signer, pub, err := sshxferNewHostKeySigner()
	if err != nil {
		return err
	}
	stub, err := sshxferStartStub(signer, "deploy", "secret")
	if err != nil {
		return err
	}
	w.stub = stub
	host, port, err := sshxferSplitPort(stub.addr())
	if err != nil {
		return err
	}
	pinned := base64.StdEncoding.EncodeToString(pub.Marshal())
	tr, err := transport.NewTransport(sshxferTarget(host, port, pinned, 0), host)
	if err != nil {
		return err
	}
	w.tr = tr
	return nil
}

func (w *sshxferWorld) connectExecUploadDownload() error {
	ctx := context.Background()
	if err := w.tr.Connect(ctx); err != nil {
		return fmt.Errorf("Connect over in-process ssh/sftp: %w", err)
	}
	if got := w.tr.OS(); got != spec.OSLinux {
		return fmt.Errorf("OS()=%s want linux", got)
	}

	// The Feature requires Exec to "surface application exit codes". The stub
	// reports a distinctive non-zero exit status on the wire, and Exec MUST
	// return it in Result.ExitCode with a nil error — an application exit is
	// NOT a transport failure (DESIGN §8.1). Asserting the exact non-zero value
	// (not merely != 0) is what proves the wire code is forwarded via
	// *ssh.ExitError/ExitStatus() rather than hard-coded; a constant-0 stub
	// could not tell a surfaced code apart from a dropped one.
	res, err := w.tr.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: fmt.Sprintf("exit %d", sshxferExecExitCode)})
	if err != nil {
		return fmt.Errorf("Exec surfaced an application exit as a transport error: %w", err)
	}
	if res.ExitCode != sshxferExecExitCode {
		return fmt.Errorf("Exec ExitCode=%d want %d (application exit code must surface in Result with nil error)", res.ExitCode, sshxferExecExitCode)
	}

	dir, err := os.MkdirTemp("", "e2e-ssh-xfer-")
	if err != nil {
		return err
	}
	remote := filepath.Join(dir, "payload.bin")
	local := filepath.Join(dir, "roundtrip.bin")
	want := []byte("release-provider sftp round-trip \x00\x01\x02 data")

	if err := w.tr.Upload(ctx, bytes.NewReader(want), int64(len(want)), remote); err != nil {
		return fmt.Errorf("Upload: %w", err)
	}
	if err := w.tr.Download(ctx, remote, local); err != nil {
		return fmt.Errorf("Download: %w", err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("round-trip mismatch: got %q want %q", got, want)
	}
	return nil
}

func (w *sshxferWorld) roundTripAndAuthNotRetried() error {
	// Round-trip byte-identity was asserted in the When step; here prove that an
	// authentication rejection maps to ERR_AUTH after EXACTLY ONE dial even when
	// connect_retries=3 (DESIGN §8.1: auth rejection is NOT retried).
	if err := os.Setenv(sshxferPassEnv, "wrong"); err != nil {
		return err
	}
	signer, pub, err := sshxferNewHostKeySigner()
	if err != nil {
		return err
	}
	authStub, err := sshxferStartStub(signer, "deploy", "right")
	if err != nil {
		return err
	}
	defer authStub.close()
	host, port, err := sshxferSplitPort(authStub.addr())
	if err != nil {
		return err
	}
	pinned := base64.StdEncoding.EncodeToString(pub.Marshal())
	tr, err := transport.NewTransport(sshxferTarget(host, port, pinned, 3), host)
	if err != nil {
		return err
	}
	defer tr.Close()

	err = tr.Connect(context.Background())
	if err == nil {
		return fmt.Errorf("Connect must fail when the password is rejected")
	}
	var ce *transport.CodedError
	if !errors.As(err, &ce) {
		return fmt.Errorf("expected *transport.CodedError, got %T: %v", err, err)
	}
	if ce.Code != "ERR_AUTH" {
		return fmt.Errorf("auth rejection must map to ERR_AUTH, got %s (%v)", ce.Code, err)
	}
	if got := authStub.connCount(); got != 1 {
		return fmt.Errorf("auth rejection retried: server saw %d connections, want exactly 1", got)
	}
	return nil
}

// --- Scenario: Host key mismatch mapping ------------------------------------

func (w *sshxferWorld) stubPinnedWrong() error {
	if err := os.Setenv(sshxferPassEnv, "secret"); err != nil {
		return err
	}
	serverSigner, _, err := sshxferNewHostKeySigner()
	if err != nil {
		return err
	}
	_, wrongPub, err := sshxferNewHostKeySigner() // unrelated key → guaranteed mismatch
	if err != nil {
		return err
	}
	stub, err := sshxferStartStub(serverSigner, "deploy", "secret")
	if err != nil {
		return err
	}
	w.stub = stub
	host, port, err := sshxferSplitPort(stub.addr())
	if err != nil {
		return err
	}
	pinned := base64.StdEncoding.EncodeToString(wrongPub.Marshal())
	// retries=0 so a (correctly non-retried) mismatch never sleeps on backoff.
	tr, err := transport.NewTransport(sshxferTarget(host, port, pinned, 0), host)
	if err != nil {
		return err
	}
	w.tr = tr
	return nil
}

func (w *sshxferWorld) connectRunsAgainstStubKey() error {
	w.connectErr = w.tr.Connect(context.Background())
	return nil
}

func (w *sshxferWorld) errIsConnectHostKeyMismatch() error {
	if w.connectErr == nil {
		return fmt.Errorf("Connect must fail when the presented host key does not match the pin")
	}
	var ce *transport.CodedError
	if !errors.As(w.connectErr, &ce) {
		return fmt.Errorf("expected *transport.CodedError, got %T: %v", w.connectErr, w.connectErr)
	}
	if ce.Code != "ERR_CONNECT" {
		return fmt.Errorf("host key mismatch must map to ERR_CONNECT, got %s (%v)", ce.Code, w.connectErr)
	}
	if !strings.Contains(w.connectErr.Error(), "host key mismatch") {
		return fmt.Errorf("error detail must contain %q, got %q", "host key mismatch", w.connectErr.Error())
	}
	return nil
}

// InitializeScenario_transport_and_artifact_acquisition_ssh_transport_and_sftp_transfer
// registers the step definitions for the Stage 2.3 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_transport_and_artifact_acquisition_ssh_transport_and_sftp_transfer(ctx *godog.ScenarioContext) {
	w := &sshxferWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		w.reset()
		return c, nil
	})
	ctx.After(func(c context.Context, _ *godog.Scenario, err error) (context.Context, error) {
		w.reset()
		return c, nil
	})

	ctx.Step(`^a stub SSH endpoint whose host key is pinned correctly$`, w.stubPinnedCorrectly)
	ctx.Step(`^Connect, Exec, Upload and Download run over the ssh transport$`, w.connectExecUploadDownload)
	ctx.Step(`^data round-trips byte-for-byte and auth rejection is not retried$`, w.roundTripAndAuthNotRetried)

	ctx.Step(`^a stub SSH endpoint whose pinned host key is wrong$`, w.stubPinnedWrong)
	ctx.Step(`^Connect runs over the ssh transport against the stub key$`, w.connectRunsAgainstStubKey)
	ctx.Step(`^the error is ERR_CONNECT with detail host key mismatch$`, w.errIsConnectHostKeyMismatch)
}

// TestE2E_transport_and_artifact_acquisition_ssh_transport_and_sftp_transfer is
// the go test entrypoint for the Stage 2.3 godog suite.
func TestE2E_transport_and_artifact_acquisition_ssh_transport_and_sftp_transfer(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_transport_and_artifact_acquisition_ssh_transport_and_sftp_transfer,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"transport_and_artifact_acquisition_ssh_transport_and_sftp_transfer.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
