package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// Stage 9.1 — Windows and Linux Single Target Acceptance harness.
//
// This file wires the terraform-plugin-testing acceptance harness behind
// TF_ACC=1 with env-provided W1 (Windows Server 2022, WinRM https) and L1
// (Linux VM, ssh) targets, and provides the cross-cutting invariant every
// DESIGN §18 scenario must satisfy: after each scenario `.lock` is ABSENT on
// every touched host (DESIGN §18 line 830: "Every scenario ends with: assert
// `.lock` absent on all touched hosts.").
//
// Env-gating contract (mirrors internal/transport/ssh_acc_test.go):
//   - TF_ACC unset  ⇒ every acceptance test self-SKIPS (never touches a host);
//     the plain `go test` gate is unaffected.
//   - TF_ACC=1      ⇒ the run is a real lab gate. A scenario that needs W1/L1
//     but whose target env is missing FAILS (t.Fatal), never silently skips,
//     so a mis-configured lab run cannot masquerade as a pass.
//
// The VAL scenarios are host-independent (validation fires before any dial), so
// they run under a real `terraform` binary with only TF_ACC=1 and need no lab
// infra. Every other group (CON/ART/CAP/WSV/NOD/NET/DRF/DST/IDP/LCK/RBK) needs a
// live target and is gated on the corresponding W1/L1 env block.

// Env var names consumed by the acceptance harness.
const (
	// Windows single target W1.
	envW1Host     = "LABDEPLOY_ACC_W1_HOST"
	envW1Port     = "LABDEPLOY_ACC_W1_PORT"
	envW1User     = "LABDEPLOY_ACC_W1_USER"
	envW1Password = "LABDEPLOY_ACC_W1_PASSWORD"

	// Linux single target L1.
	envL1Host     = "LABDEPLOY_ACC_L1_HOST"
	envL1Port     = "LABDEPLOY_ACC_L1_PORT"
	envL1User     = "LABDEPLOY_ACC_L1_USER"
	envL1Password = "LABDEPLOY_ACC_L1_PASSWORD"
	envL1Key      = "LABDEPLOY_ACC_L1_PRIVATE_KEY"
	envL1HostKey  = "LABDEPLOY_ACC_L1_HOST_KEY"

	// Artifact host serving the sample-svc packages (DESIGN §18: versions
	// 1.0.0, 1.1.0, 1.2.0-bad). The <ver> and <ext> tokens are substituted per
	// scenario. Optional; scenarios that need it Fatal when it is unset.
	envArtifactBaseURL = "LABDEPLOY_ACC_ARTIFACT_BASE_URL"
)

// accPreCheck skips the calling test unless TF_ACC=1. Every acceptance test in
// this stage calls it first so the standard `go test ./...` gate never dials a
// lab host.
func accPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("acceptance test; set TF_ACC=1 (plus the scenario's W1/L1 env) to run against the lab")
	}
}

// w1Target reads the W1 (Windows) target env and returns the spec.Target used to
// probe `.lock` and the YAML `target:` block used in scenario specs. Under
// TF_ACC=1 a missing required var is a FAILURE, never a skip.
type accTarget struct {
	tgt      spec.Target
	host     string
	yaml     string // the `target:` block body (indented two spaces), no leading key
	passName string // env var NAME the spec references for the password
}

// requireW1 returns the configured Windows target or fails the test.
func requireW1(t *testing.T) accTarget {
	t.Helper()
	host := mustEnv(t, envW1Host, "W1 Windows target")
	user := mustEnv(t, envW1User, "W1 Windows target")
	if os.Getenv(envW1Password) == "" {
		t.Fatalf("TF_ACC=1 requires %s (the W1 account password) to be set", envW1Password)
	}
	port := optPort(t, envW1Port, 5986)
	insecure := true
	https := true
	tgt := spec.Target{
		Transport: spec.TransportWinRM,
		Hosts:     []string{host},
		OS:        spec.OSWindows,
		Port:      port,
	}
	tgt.Credentials.Username = user
	tgt.Credentials.PasswordEnv = envW1Password
	tgt.WinRM.UseHTTPS = &https
	tgt.WinRM.InsecureSkipVerify = &insecure
	yaml := fmt.Sprintf(`  transport: winrm
  hosts: [%q]
  os: windows
  port: %d
  credentials: { username: %q, password_env: %s }
  winrm: { use_https: true, insecure_skip_verify: true }`, host, port, user, envW1Password)
	return accTarget{tgt: tgt, host: host, yaml: yaml, passName: envW1Password}
}

// requireL1 returns the configured Linux target or fails the test.
func requireL1(t *testing.T) accTarget {
	t.Helper()
	host := mustEnv(t, envL1Host, "L1 Linux target")
	user := mustEnv(t, envL1User, "L1 Linux target")
	pass := os.Getenv(envL1Password)
	key := os.Getenv(envL1Key)
	if pass == "" && key == "" {
		t.Fatalf("TF_ACC=1 requires %s or %s for the L1 Linux target", envL1Password, envL1Key)
	}
	port := optPort(t, envL1Port, 22)
	tgt := spec.Target{
		Transport: spec.TransportSSH,
		Hosts:     []string{host},
		OS:        spec.OSLinux,
		Port:      port,
	}
	tgt.Credentials.Username = user
	credLine := ""
	passName := ""
	if pass != "" {
		tgt.Credentials.PasswordEnv = envL1Password
		passName = envL1Password
		credLine = fmt.Sprintf("credentials: { username: %q, password_env: %s }", user, envL1Password)
	} else {
		tgt.Credentials.PrivateKeyEnv = envL1Key
		passName = envL1Key
		credLine = fmt.Sprintf("credentials: { username: %q, private_key_env: %s }", user, envL1Key)
	}
	sshBlock := ""
	if hk := os.Getenv(envL1HostKey); hk != "" {
		tgt.SSH.HostKey = hk
		sshBlock = fmt.Sprintf("\n  ssh: { host_key: %q }", hk)
	}
	yaml := fmt.Sprintf(`  transport: ssh
  hosts: [%q]
  os: linux
  port: %d
  %s%s`, host, port, credLine, sshBlock)
	return accTarget{tgt: tgt, host: host, yaml: yaml, passName: passName}
}

func mustEnv(t *testing.T, name, why string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("TF_ACC=1 requires %s for the %s", name, why)
	}
	return v
}

func optPort(t *testing.T, name string, def int) int {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("%s=%q: %v", name, v, err)
		}
		return n
	}
	return def
}

// artifactURL builds the scenario artifact URL from the env base. The base may
// contain the tokens {ver} and {ext} which are substituted; otherwise the file
// name is appended. Scenarios that need an artifact call this and Fatal when the
// base is unset under TF_ACC=1.
func artifactURL(t *testing.T, version, ext string) string {
	t.Helper()
	base := os.Getenv(envArtifactBaseURL)
	if base == "" {
		t.Fatalf("TF_ACC=1 requires %s to serve sample-svc %s.%s", envArtifactBaseURL, version, ext)
	}
	if strings.Contains(base, "{ver}") || strings.Contains(base, "{ext}") {
		u := strings.ReplaceAll(base, "{ver}", version)
		return strings.ReplaceAll(u, "{ext}", ext)
	}
	return strings.TrimRight(base, "/") + fmt.Sprintf("/sample-svc-%s.%s", version, ext)
}

// checkLockAbsent returns a resource.TestCheckFunc that connects to every touched
// host and asserts the deployment `.lock` file is ABSENT — the invariant DESIGN
// §18 requires at the end of every scenario. installRoot/app derive the lock path
// via the SAME layout.NewPaths the engine uses, so the probe path is exact.
func checkLockAbsent(tgt spec.Target, installRoot, app string) func(*terraform.State) error {
	return func(*terraform.State) error {
		return assertLockAbsent(tgt, installRoot, app)
	}
}

// assertLockAbsent probes each host in tgt and returns an error if `.lock` still
// exists. It is safe to call from negative scenarios (nothing deployed ⇒ absent).
func assertLockAbsent(tgt spec.Target, installRoot, app string) error {
	lock := layout.NewPaths(tgt.OS, installRoot, app, "").Lock
	for _, host := range tgt.Hosts {
		tr, err := transport.NewTransport(&tgt, host)
		if err != nil {
			return fmt.Errorf("lock-probe %s: build transport: %w", host, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := tr.Connect(ctx); err != nil {
			cancel()
			return fmt.Errorf("lock-probe %s: connect: %w", host, err)
		}
		present, perr := lockPresent(ctx, tr, lock)
		_ = tr.Close()
		cancel()
		if perr != nil {
			return fmt.Errorf("lock-probe %s: %w", host, perr)
		}
		if present {
			return fmt.Errorf("post-scenario invariant violated: %s still exists on %s (DESIGN §18: .lock must be absent)", lock, host)
		}
	}
	return nil
}

// lockPresent probes a single host for the lock file, dispatching on the target
// OS shell. It prints a stable token so the result is unambiguous regardless of
// remote locale.
func lockPresent(ctx context.Context, tr transport.Transport, lockPath string) (bool, error) {
	var cmd transport.Cmd
	switch tr.OS() {
	case spec.OSWindows:
		cmd = transport.Cmd{
			Shell:  transport.ShellPowerShell,
			Script: fmt.Sprintf("if (Test-Path -LiteralPath '%s') { 'LOCK_PRESENT' } else { 'LOCK_ABSENT' }", strings.ReplaceAll(lockPath, "'", "''")),
		}
	default:
		cmd = transport.Cmd{
			Shell:  transport.ShellSh,
			Script: fmt.Sprintf("if [ -e '%s' ]; then echo LOCK_PRESENT; else echo LOCK_ABSENT; fi", strings.ReplaceAll(lockPath, "'", `'\''`)),
		}
	}
	res, err := tr.Exec(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("probe exec: %w", err)
	}
	if strings.Contains(res.Stdout, "LOCK_PRESENT") {
		return true, nil
	}
	if strings.Contains(res.Stdout, "LOCK_ABSENT") {
		return false, nil
	}
	return false, fmt.Errorf("probe returned neither token (exit=%d stdout=%q stderr=%q)", res.ExitCode, res.Stdout, res.Stderr)
}
