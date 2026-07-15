package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"golang.org/x/crypto/ssh"
)

type sshTransport struct {
	host, user, pass, keyPEM, pinnedKey string
	port, dialTimeout, retries          int
	osKind                              spec.OSKind
	client                              *ssh.Client
	sftpc                               *sftp.Client
}

func newSSH(host string, port int, user, pass, keyPEM, pinnedKey string,
	dialTimeout, retries int, osKind spec.OSKind) *sshTransport {
	if dialTimeout <= 0 {
		dialTimeout = 30
	}
	return &sshTransport{host: host, port: port, user: user, pass: pass,
		keyPEM: keyPEM, pinnedKey: pinnedKey, dialTimeout: dialTimeout,
		retries: retries, osKind: osKind}
}

func (s *sshTransport) OS() spec.OSKind { return s.osKind }
func (s *sshTransport) Host() string    { return s.host }

func (s *sshTransport) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if s.pinnedKey == "" { // lab default: accept-any (WARN diag emitted by engine)
		return ssh.InsecureIgnoreHostKey(), nil
	}
	raw, err := base64.StdEncoding.DecodeString(s.pinnedKey)
	if err != nil {
		return nil, fmt.Errorf("ssh.host_key: not base64: %w", err)
	}
	pk, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("ssh.host_key: %w", err)
	}
	return ssh.FixedHostKey(pk), nil
}

func (s *sshTransport) Connect(ctx context.Context) error {
	var auth []ssh.AuthMethod
	if s.keyPEM != "" {
		signer, err := ssh.ParsePrivateKey([]byte(s.keyPEM))
		if err != nil {
			return ErrAuth(fmt.Errorf("private key parse: %w", err))
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if s.pass != "" {
		auth = append(auth, ssh.Password(s.pass))
	}
	hk, err := s.hostKeyCallback()
	if err != nil {
		return ErrConnect(err)
	}
	cfg := &ssh.ClientConfig{
		User:            s.user,
		Auth:            auth,
		HostKeyCallback: hk,
		Timeout:         time.Duration(s.dialTimeout) * time.Second,
	}
	addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
	var lastErr error
	for i := 0; i <= s.retries; i++ {
		cli, err := ssh.Dial("tcp", addr, cfg)
		if err == nil {
			s.client = cli
			sc, err := sftp.NewClient(cli)
			if err != nil {
				cli.Close()
				return ErrConnect(fmt.Errorf("sftp subsystem: %w", err))
			}
			s.sftpc = sc
			return nil
		}
		es := err.Error()
		if strings.Contains(es, "unable to authenticate") ||
			strings.Contains(es, "no supported methods remain") {
			return ErrAuth(err) // not retried
		}
		if strings.Contains(es, "host key mismatch") || strings.Contains(es, "key mismatch") {
			return ErrConnect(fmt.Errorf("host key mismatch: %w", err))
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ErrConnect(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	return ErrConnect(fmt.Errorf("after %d attempts: %w", s.retries+1, lastErr))
}

func (s *sshTransport) Close() error {
	if s.sftpc != nil {
		s.sftpc.Close()
	}
	if s.client != nil {
		return s.client.Close()
	}
	return nil
}

func (s *sshTransport) Exec(ctx context.Context, c Cmd) (Result, error) {
	if s.client == nil {
		return Result{}, ErrConnect(fmt.Errorf("not connected"))
	}
	line, err := BuildCommandLine(s.osKind, c)
	if err != nil {
		return Result{}, err
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return Result{}, ErrConnect(fmt.Errorf("session: %w", err))
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(line) }()
	var timeout <-chan time.Time
	if c.TimeoutSec > 0 {
		timeout = time.After(time.Duration(c.TimeoutSec) * time.Second)
	}
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return Result{}, ErrConnect(ctx.Err())
	case <-timeout:
		_ = sess.Signal(ssh.SIGKILL)
		return Result{ExitCode: -1, Stdout: stdout.String(), Stderr: stderr.String()},
			fmt.Errorf("exec timed out after %ds", c.TimeoutSec)
	}
	res := Result{ExitCode: 0, Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			res.ExitCode = ee.ExitStatus()
			return res, nil // app-level exit: not a transport error
		}
		return res, ErrConnect(fmt.Errorf("ssh run: %w", err))
	}
	return res, nil
}

func (s *sshTransport) Upload(ctx context.Context, local io.Reader, size int64, remote string) error {
	remote = filepath.ToSlash(remote)
	if s.osKind == spec.OSLinux {
		if err := s.sftpc.MkdirAll(path(remote)); err != nil {
			return err
		}
	} else {
		// windows sftp servers accept forward slashes; ensure dir via PS
		dir := remote[:strings.LastIndex(remote, "/")]
		r, err := s.Exec(ctx, Cmd{Shell: ShellPowerShell,
			Script: fmt.Sprintf(`New-Item -ItemType Directory -Force -Path %s | Out-Null`, psq(strings.ReplaceAll(dir, "/", `\`)))})
		if err != nil || r.ExitCode != 0 {
			return fmt.Errorf("mkdir %s: err=%v stderr=%s", dir, err, r.Stderr)
		}
	}
	f, err := s.sftpc.Create(remote)
	if err != nil {
		return fmt.Errorf("sftp create %s: %w", remote, err)
	}
	defer f.Close()
	_, err = io.Copy(f, local)
	return err
}

func (s *sshTransport) Download(ctx context.Context, remote, local string) error {
	remote = filepath.ToSlash(remote)
	rf, err := s.sftpc.Open(remote)
	if err != nil {
		return fmt.Errorf("sftp open %s: %w", remote, err)
	}
	defer rf.Close()
	lf, err := os.Create(local)
	if err != nil {
		return err
	}
	defer lf.Close()
	_, err = io.Copy(lf, rf)
	return err
}

func path(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}
