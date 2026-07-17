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
// malicious manifest glob: on Linux the pattern is passed as DATA to `find
// -path` (single-quoted), never expanded by the shell, so command substitutions
// are inert; the archive path is shell/PS quoted; on Windows the glob is
// PowerShell single-quoted.
func TestZipScriptQuoting(t *testing.T) {
	evil := "/var/log/$(touch /tmp/pwned)/*.log"
	lin := buildZipScript(spec.OSLinux, []string{evil}, "/tmp/x'y.tar.gz").Script
	// find (not the shell) interprets the wildcards; both args are single-quoted.
	if !strings.Contains(lin, `find "$1" -type f -path "$2"`) {
		t.Fatalf("linux should match via find -path, not shell glob: %s", lin)
	}
	// The dangerous pattern must appear ONLY inside single quotes (as data).
	if !strings.Contains(lin, `'/var/log/$(touch /tmp/pwned)/*.log'`) {
		t.Fatalf("evil glob not passed as single-quoted data: %s", lin)
	}
	// It must NOT appear as an unquoted command substitution the shell would run.
	if strings.Contains(lin, "for f in "+evil) || strings.Contains(lin, "for f in /var/log/$(touch") {
		t.Fatalf("evil glob left unquoted for shell expansion: %s", lin)
	}
	if !strings.Contains(lin, `tar czf '/tmp/x'\''y.tar.gz' -C "$tmp" .`) {
		t.Fatalf("linux archive path not safely quoted: %s", lin)
	}
	win := buildZipScript(spec.OSWindows, []string{"C:\\a'b"}, "C:\\x.zip").Script
	if !strings.Contains(win, `'C:\a''b'`) {
		t.Fatalf("windows glob not safely quoted: %s", win)
	}
}
