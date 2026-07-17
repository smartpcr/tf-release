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
// malicious manifest glob: on Linux the pattern is passed as DATA to portable
// `find ... -path` (single-quoted), never expanded by the shell, so command
// substitutions are inert; depth-pinning keeps `*` segment-aware; the archive
// path is shell/PS quoted; on Windows the glob is PowerShell single-quoted.
func TestZipScriptQuoting(t *testing.T) {
	evil := "/var/log/$(touch /tmp/pwned)/*.log"
	lin := buildZipScript(spec.OSLinux, []string{evil}, "/tmp/x'y.tar.gz").Script
	// find (not the shell) matches via portable, segment-aware depth-pinned -path.
	if !strings.Contains(lin, `find "$1" -maxdepth "$2" -mindepth "$2" -type f -path "$3"`) {
		t.Fatalf("linux should match via portable find -path, not shell glob: %s", lin)
	}
	// GNU-only -regextype/-regex must be gone (BSD/BusyBox portability, item 3).
	if strings.Contains(lin, "-regextype") || strings.Contains(lin, "-regex ") {
		t.Fatalf("linux find must not use GNU-only -regextype/-regex: %s", lin)
	}
	// The entire evil glob (root arg AND -path arg) must be single-quoted DATA.
	if !strings.Contains(lin, `'/var/log/$(touch /tmp/pwned)'`) {
		t.Fatalf("evil glob root not passed as single-quoted data: %s", lin)
	}
	if !strings.Contains(lin, `'/var/log/$(touch /tmp/pwned)/*.log'`) {
		t.Fatalf("evil glob -path not passed as single-quoted data: %s", lin)
	}
	// find errors must be surfaced, not silently degraded to NOFILES (item 3).
	if !strings.Contains(lin, "echo FINDERR") || !strings.Contains(lin, "exit 3") {
		t.Fatalf("linux find failure not surfaced as FINDERR/exit 3: %s", lin)
	}
	// No shell glob-expansion loop over the pattern at all.
	if strings.Contains(lin, "for f in ") {
		t.Fatalf("collection must not shell-expand globs: %s", lin)
	}
	if !strings.Contains(lin, `tar czf '/tmp/x'\''y.tar.gz' -C "$stagedir" .`) {
		t.Fatalf("linux archive path not safely quoted: %s", lin)
	}
	win := buildZipScript(spec.OSWindows, []string{"C:\\a'b"}, "C:\\x.zip").Script
	if !strings.Contains(win, `'C:\a''b'`) {
		t.Fatalf("windows glob not safely quoted: %s", win)
	}
}

// TestWindowsDrivePreservesIdentifier proves identically named files on different
// drives/shares stage under distinct directories (evaluator item 5), so
// Copy-Item -Force cannot silently overwrite a same-basename cross-drive file.
func TestWindowsDrivePreservesIdentifier(t *testing.T) {
	win := buildZipScript(spec.OSWindows, []string{`C:\logs\*.log`, `D:\logs\*.log`}, `C:\x.zip`).Script
	// Drive letter becomes a top-level staging directory (C\..., D\...).
	if !strings.Contains(win, `$rel = $Matches[1] + '\' + ($p -replace '^[A-Za-z]:[\\/]+','')`) {
		t.Fatalf("windows staging must preserve the drive letter as a directory: %s", win)
	}
	// UNC paths are namespaced under UNC\ so \\a\share and \\b\share don't collide.
	if !strings.Contains(win, `$rel = 'UNC\' + ($p -replace '^[\\/]{2}','')`) {
		t.Fatalf("windows staging must namespace UNC paths: %s", win)
	}
	// The old drive-stripping form (which collided C:\ and D:\) must be gone.
	if strings.Contains(win, `-replace '^[A-Za-z]:[\\/]+','' -replace '^[\\/]+',''`) {
		t.Fatalf("windows staging still strips drive letters (collision risk): %s", win)
	}
}

// TestFindDepth verifies the search depth pinned for each glob equals the number
// of path components below its non-wildcard root, so `find -path`'s wildcards stay
// segment-aware without GNU-only -regex (evaluator items 3 & 4).
func TestFindDepth(t *testing.T) {
	cases := map[string]int{
		"/var/log/app/*.log": 1,
		"/var/*/app.log":     2,
		"/*.log":             1,
		"logs/*.log":         1,
		"*.log":              1,
		"a/b/c/*.trx":        1,
	}
	for in, want := range cases {
		if got := findDepth(in); got != want {
			t.Errorf("findDepth(%q) = %d, want %d", in, got, want)
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
