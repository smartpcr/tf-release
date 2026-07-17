package transport

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// Scenario (in-process, always runs): the Linux `sh -c` env-prepend places the
// `K='V' ` assignments BEFORE `sh -c`, never inside the quoted script. An
// in-script prefix (`sh -c 'FOO=bar cmd $FOO'`) expands `$FOO` with the OLD
// value before the assignment applies, so this shape assertion is what catches
// the regression deterministically without needing a live shell (DESIGN §8.1).
func TestBuildCommandLineShEnvPrefix(t *testing.T) {
	script := `test "$FOO" = bar && echo ok`
	line, err := BuildCommandLine(spec.OSLinux, Cmd{
		Shell:  ShellSh,
		Script: script,
		Env:    map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("BuildCommandLine: %v", err)
	}
	// The assignment must PRECEDE `sh -c`.
	if !strings.HasPrefix(line, "FOO='bar' sh -c ") {
		t.Fatalf("env assignment must precede `sh -c`; got %q", line)
	}
	assignIdx := strings.Index(line, "FOO='bar'")
	shIdx := strings.Index(line, "sh -c")
	if assignIdx < 0 || shIdx < 0 || assignIdx > shIdx {
		t.Fatalf("assignment (%d) must come before `sh -c` (%d): %q", assignIdx, shIdx, line)
	}
	// The quoted script must NOT contain the assignment (no in-script prefix).
	quoted := line[shIdx+len("sh -c "):]
	if strings.Contains(quoted, "FOO=") {
		t.Fatalf("script segment must not carry the assignment inline: %q", quoted)
	}
	if quoted != shQuote(script) {
		t.Fatalf("script segment = %q, want shQuote(script)=%q", quoted, shQuote(script))
	}
}

// Multiple vars are emitted in sorted key order, each preceding `sh -c`.
func TestBuildCommandLineShEnvPrefixSorted(t *testing.T) {
	line, err := BuildCommandLine(spec.OSLinux, Cmd{
		Shell:  ShellSh,
		Script: "true",
		Env:    map[string]string{"BETA": "2", "ALPHA": "1"},
	})
	if err != nil {
		t.Fatalf("BuildCommandLine: %v", err)
	}
	if !strings.HasPrefix(line, "ALPHA='1' BETA='2' sh -c ") {
		t.Fatalf("sorted env prefix wrong: %q", line)
	}
}

// Scenario (behavioural, runs wherever `sh` exists — always on Linux CI): a
// Linux command that references its injected variable observes the value,
// proving the env-prepend fix end-to-end and not only structurally. Skipped
// only when no POSIX sh is on PATH (e.g. a bare Windows gate host).
func TestLocalExecInjectedEnvVisible(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh on PATH; behavioural env-injection proof runs on Linux/CI")
	}
	tr := newLocal(spec.OSLinux) // Exec does not require Connect / OS match
	res, err := tr.Exec(context.Background(), Cmd{
		Shell:  ShellSh,
		Script: `printf '%s' "$FOO"`,
		Env:    map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.Stdout != "bar" {
		t.Fatalf("injected $FOO not visible to script: stdout=%q want %q", res.Stdout, "bar")
	}
}
