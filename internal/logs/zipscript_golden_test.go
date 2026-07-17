package logs

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// update regenerates the committed glob-to-zip goldens:
// `go test ./internal/logs -run ZipScript -update`.
var update = flag.Bool("update", false, "update glob-to-zip script golden files")

// TestZipScriptGolden asserts the collection glob-to-zip script generated for
// windows/linux matches the committed golden (Architecture T8: "glob→zip script
// golden"). Covers the multi-glob join and the NOFILES sentinel path.
func TestZipScriptGolden(t *testing.T) {
	cases := []struct {
		os      spec.OSKind
		label   string
		globs   []string
		archive string
	}{
		{spec.OSWindows, "windows",
			[]string{`C:\app\logs\*.log`, `C:\app\_results\*.trx`},
			`C:\Windows\Temp\labdeploy-logs.zip`},
		{spec.OSLinux, "linux",
			[]string{"/var/log/app/*.log", "/opt/app/_results/*.xml"},
			"/tmp/labdeploy-logs.tar.gz"},
	}
	for _, c := range cases {
		cmd := buildZipScript(c.os, c.globs, c.archive)
		golden := filepath.Join("testdata", "collect", "zip_"+c.label+".golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(cmd.Script), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("missing golden %s (run with -update): %v", golden, err)
		}
		if string(want) != cmd.Script {
			t.Errorf("zip script mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s",
				c.label, want, cmd.Script)
		}
	}
}

// TestZipScriptQuoting proves the collection script cannot be hijacked by a
// malicious manifest glob: on Linux the pattern is translated to a POSIX-extended
// regex and passed as DATA to `find -regex` (single-quoted), never expanded by
// the shell, so command substitutions are inert; segment-aware `*` becomes
// `[^/]*`; the archive path is shell/PS quoted; on Windows the glob is
// PowerShell single-quoted.
func TestZipScriptQuoting(t *testing.T) {
	evil := "/var/log/$(touch /tmp/pwned)/*.log"
	lin := buildZipScript(spec.OSLinux, []string{evil}, "/tmp/x'y.tar.gz").Script
	// find (not the shell) matches via a POSIX-extended regex.
	if !strings.Contains(lin, `find "$1" -regextype posix-extended -type f -regex "$2"`) {
		t.Fatalf("linux should match via find -regex, not shell glob: %s", lin)
	}
	// `*` must translate to a segment-aware regex atom (never cross '/').
	if !strings.Contains(lin, `[^/]*`) {
		t.Fatalf("glob '*' not translated to segment-aware regex: %s", lin)
	}
	// The dangerous substitution chars must appear ONLY inside single quotes, with
	// their regex metacharacters escaped — never as a live command substitution.
	if !strings.Contains(lin, `'/var/log/\$\(touch /tmp/pwned\)/[^/]*\.log'`) {
		t.Fatalf("evil glob not passed as escaped single-quoted regex: %s", lin)
	}
	// The original shell-glob form (wildcard intact, unescaped) must be absent —
	// its presence would mean the shell, not find, expands the pattern.
	if strings.Contains(lin, `$(touch /tmp/pwned)/*.log`) {
		t.Fatalf("evil glob left in shell-expandable form: %s", lin)
	}
	// No shell glob-expansion loop over the pattern at all.
	if strings.Contains(lin, "for f in ") {
		t.Fatalf("collection must not shell-expand globs: %s", lin)
	}
	if !strings.Contains(lin, `tar czf '/tmp/x'\''y.tar.gz' -C "$tmp" .`) {
		t.Fatalf("linux archive path not safely quoted: %s", lin)
	}
	win := buildZipScript(spec.OSWindows, []string{"C:\\a'b"}, "C:\\x.zip").Script
	if !strings.Contains(win, `'C:\a''b'`) {
		t.Fatalf("windows glob not safely quoted: %s", win)
	}
}

// TestGlobToRegexSegmentAware verifies `*`/`?` never cross '/' (evaluator item 3)
// and that regex metacharacters in literals are escaped.
func TestGlobToRegexSegmentAware(t *testing.T) {
	cases := map[string]string{
		"/logs/*.log":    `/logs/[^/]*\.log`,
		"logs/*.log":     `logs/[^/]*\.log`,
		"a/?.trx":        `a/[^/]\.trx`,
		"expected/*.trx": `expected/[^/]*\.trx`,
		"a.b+c(d)":       `a\.b\+c\(d\)`,
		"[!x]y":          `[^x]y`,
	}
	for in, want := range cases {
		if got := globToRegex(in); got != want {
			t.Errorf("globToRegex(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGlobRootBounded verifies the find search root is the deepest non-wildcard
// dir and is `./`-prefixed for relative globs so find output matches (item 4).
func TestGlobRootBounded(t *testing.T) {
	cases := map[string]string{
		"/var/log/app/*.log": "/var/log/app",
		"/var/*/app.log":     "/var",
		"/*.log":             "/",
		"logs/*.log":         "./logs",
		"*.log":              ".",
		"a/b/c/*.trx":        "./a/b/c",
	}
	for in, want := range cases {
		if got := globRoot(in); got != want {
			t.Errorf("globRoot(%q) = %q, want %q", in, got, want)
		}
	}
}
