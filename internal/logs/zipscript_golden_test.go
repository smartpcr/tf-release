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

// TestZipScriptQuoting proves spec-controlled glob values are shell/PS quoted so
// they cannot break out of the generated script (injection guard).
func TestZipScriptQuoting(t *testing.T) {
	evil := "/tmp/a'; rm -rf /; echo '"
	lin := buildZipScript(spec.OSLinux, []string{evil}, "/tmp/x.tar.gz").Script
	if !strings.Contains(lin, `'/tmp/a'\''; rm -rf /; echo '\'''`) {
		t.Fatalf("linux glob not safely quoted: %s", lin)
	}
	win := buildZipScript(spec.OSWindows, []string{"C:\\a'b"}, "C:\\x.zip").Script
	if !strings.Contains(win, `'C:\a''b'`) {
		t.Fatalf("windows glob not safely quoted: %s", win)
	}
}
