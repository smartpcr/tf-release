//go:build e2e

package e2e

// In-process SSH + SFTP server used to SELF-PROVISION an L1-style Linux target
// on Forge's bare gate (no docker, no external VM). It lets the Linux scenario
// drive the provider's REAL `ssh` transport (Connect over TCP, Exec over an SSH
// "session"/exec channel, Upload/Download over the SFTP subsystem) end to end,
// executing the engine's REAL POSIX deploy scripts through a real Git-for-Windows
// `sh`. This converts "partial POSIX-script execution" into a genuine SSH/SFTP
// transport exercise against a self-provisioned target. Only the privileged
// `ln -sfn` symlink switch (needs SeCreateSymbolicLinkPrivilege/admin) stays
// lab-only; every other Linux deploy step runs over this real SSH channel.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type w91SSHServer struct {
	ln        net.Listener
	port      int
	clientPEM string
	shPath    string
	env       []string
	shimDir   string
	wg        sync.WaitGroup
	closed    int32
}

// w91StartSSHServer binds an ephemeral loopback SSH server with a fresh host key
// and a single authorized ed25519 client key (whose PEM is returned for the
// provider transport to authenticate with). Exec requests run through a real
// POSIX shell; the sftp subsystem serves the real gate filesystem.
func w91StartSSHServer() (*w91SSHServer, error) {
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		return nil, err
	}
	_, cliPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	cliSigner, err := ssh.NewSignerFromKey(cliPriv)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(cliPriv, "labdeploy-e2e")
	if err != nil {
		return nil, err
	}
	authorized := cliSigner.PublicKey().Marshal()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorized) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	// Git-for-Windows ships no `flock`, which the engine's POSIX lock scripts
	// require (`command -v flock || exit 70`). Provide a small PATH-prepended
	// shim so the REAL lock/CAS lifecycle can run over SSH; mutual exclusion is
	// still enforced by the engine's file-content compare-and-swap on `.lock`.
	shimDir, err := w91WriteShims()
	if err != nil {
		ln.Close()
		return nil, err
	}
	s := &w91SSHServer{
		ln:        ln,
		port:      ln.Addr().(*net.TCPAddr).Port,
		clientPEM: string(pem.EncodeToMemory(blk)),
		shPath:    w91ShPath(),
		shimDir:   shimDir,
		env:       w91ServerEnv(shimDir),
	}
	s.wg.Add(1)
	go s.accept(cfg)
	return s, nil
}

// w91WriteShims creates a directory holding a minimal `flock` shim (Git-for-
// Windows lacks it). The shim consumes flock's option/FD arguments and returns
// success without blocking; the engine's `.lock` compare-and-swap still provides
// real mutual exclusion, so LCK contention behaves correctly.
func w91WriteShims() (string, error) {
	dir, err := os.MkdirTemp("", "w91-shims-")
	if err != nil {
		return "", err
	}
	flock := "#!/bin/sh\n" +
		"# minimal flock shim for the Windows gate (Git-bash has no flock).\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    -w|--wait|-t|--timeout) shift 2 ;;\n" +
		"    -c) shift; sh -c \"$1\"; exit $? ;;\n" +
		"    -*) shift ;;\n" +
		"    *) shift ;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "flock"), []byte(flock), 0o755); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// w91ServerEnv ensures the Git-for-Windows POSIX toolchain (sh, unzip,
// sha256sum, curl) plus the flock shim are discoverable by the exec handler
// regardless of the gate's ambient PATH, and asks MSYS to create native
// symlinks so the engine's `ln -sfn` repoint of `current` is a real symlink.
func w91ServerEnv(shimDir string) []string {
	env := os.Environ()
	extra := []string{
		shimDir,
		`C:\Program Files\Git\usr\bin`,
		`C:\Program Files\Git\mingw64\bin`,
		`C:\Program Files\Git\bin`,
	}
	env = append(env, "MSYS=winsymlinks:sys")
	for i, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "PATH=") {
			env[i] = "PATH=" + strings.Join(extra, string(os.PathListSeparator)) + string(os.PathListSeparator) + e[len("PATH="):]
			return env
		}
	}
	return append(env, "PATH="+strings.Join(extra, string(os.PathListSeparator)))
}

func (s *w91SSHServer) accept(cfg *ssh.ServerConfig) {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if atomic.LoadInt32(&s.closed) == 1 {
				return
			}
			return
		}
		go s.handleConn(nc, cfg)
	}
}

func (s *w91SSHServer) handleConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(ch, chReqs)
	}
}

func (s *w91SSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "exec":
			var m struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &m)
			req.Reply(true, nil)
			s.runExec(ch, m.Command)
			return
		case "subsystem":
			var m struct{ Name string }
			_ = ssh.Unmarshal(req.Payload, &m)
			if m.Name == "sftp" {
				req.Reply(true, nil)
				srv, err := sftp.NewServer(ch)
				if err != nil {
					ch.Close()
					return
				}
				_ = srv.Serve()
				ch.Close()
				return
			}
			req.Reply(false, nil)
		case "env", "pty-req", "shell":
			req.Reply(true, nil)
		default:
			req.Reply(false, nil)
		}
	}
}

// runExec runs the received command line (e.g. `K='v' sh -c '<script>'`) through
// a real POSIX shell so the engine's Linux scripts execute exactly as they would
// on L1, then reports the real exit status back over the SSH channel.
func (s *w91SSHServer) runExec(ch ssh.Channel, command string) {
	cmd := w91ShCommand(s.shPath, command)
	cmd.Env = s.env
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()
	exit := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(interface{ ExitCode() int }); ok {
			exit = ee.ExitCode()
		} else {
			exit = 1
		}
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exit)}))
	ch.Close()
}

func (s *w91SSHServer) Close() error {
	atomic.StoreInt32(&s.closed, 1)
	err := s.ln.Close()
	s.wg.Wait()
	if s.shimDir != "" {
		os.RemoveAll(s.shimDir)
	}
	return err
}

// w91SFTPPath maps a Windows absolute path to the forward-slash form the SFTP
// client/server pair round-trips (the engine already forward-slashes remote
// paths for the ssh transport).
func w91SFTPPath(p string) string { return filepath.ToSlash(p) }

// w91ShCommand builds an *exec.Cmd that runs the received command line through a
// real POSIX shell (`sh -c '<line>'`). On Windows this is Git-for-Windows sh.exe.
func w91ShCommand(shPath, command string) *exec.Cmd {
	return exec.Command(shPath, "-c", command)
}
