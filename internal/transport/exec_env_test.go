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

// Scenario (in-process, always runs): an env value carrying a shell
// metacharacter -- here a single quote, as a secret such as a password passed
// through Cmd.Env might -- is single-quote escaped by shQuote before it precedes
// `sh -c`, so the remote shell cannot re-interpret it. DESIGN §17 lists env
// single-quote escaping (`O'Brien`) as a MUST-cover for the transport package;
// the quote-free cases never exercise the escaping, so a quoting regression on
// this new `sh` env path would otherwise pass CI.
func TestBuildCommandLineShEnvPrefixQuoteEscape(t *testing.T) {
	script := `printf '%s' "$P"`
	line, err := BuildCommandLine(spec.OSLinux, Cmd{
		Shell:  ShellSh,
		Script: script,
		Env:    map[string]string{"P": "O'Brien"},
	})
	if err != nil {
		t.Fatalf("BuildCommandLine: %v", err)
	}
	// `O'Brien` escapes to 'O'\''Brien' (close-quote, escaped quote, reopen) and
	// the whole assignment must PRECEDE `sh -c`.
	const wantPrefix = `P='O'\''Brien' sh -c `
	if !strings.HasPrefix(line, wantPrefix) {
		t.Fatalf("single-quote escaping wrong;\n got %q\nwant prefix %q", line, wantPrefix)
	}
	shIdx := strings.Index(line, "sh -c")
	// The escaped assignment segment must equal shQuote's rendering exactly...
	if got, want := line[:shIdx], "P="+shQuote("O'Brien")+" "; got != want {
		t.Fatalf("assignment segment = %q, want %q", got, want)
	}
	// ...and the script segment must stay the shQuote of the raw script, so the
	// assignment is not folded inline and the script's own quotes survive.
	if quoted := line[shIdx+len("sh -c "):]; quoted != shQuote(script) {
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
	cases := []struct {
		name   string
		script string
		env    map[string]string
		want   string
	}{
		{"plain", `printf '%s' "$FOO"`, map[string]string{"FOO": "bar"}, "bar"},
		// A single quote is a shell metacharacter a secret (e.g. a password
		// passed through Cmd.Env) may carry; it must round-trip back verbatim
		// through the env-injection path (DESIGN §17 transport: `O'Brien`).
		{"single-quote", `printf '%s' "$P"`, map[string]string{"P": "O'Brien"}, "O'Brien"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newLocal(spec.OSLinux) // Exec does not require Connect / OS match
			res, err := tr.Exec(context.Background(), Cmd{
				Shell:  ShellSh,
				Script: tc.script,
				Env:    tc.env,
			})
			if err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if res.ExitCode != 0 {
				t.Fatalf("ExitCode=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			if res.Stdout != tc.want {
				t.Fatalf("injected env not visible to script: stdout=%q want %q", res.Stdout, tc.want)
			}
		})
	}
}
