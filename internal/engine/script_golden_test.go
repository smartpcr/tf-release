package engine

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// -update regenerates the committed golden fixtures under testdata/.
var update = flag.Bool("update", false, "update engine script golden fixtures")

// TestScriptGolden pins the extract and junction-switch scripts for win+linux
// against committed fixtures, including the exit-42 guard on the switch
// (Stage 3.3 scenario "Switch and extract scripts golden", DESIGN §9.1).
func TestScriptGolden(t *testing.T) {
	cases := []struct {
		name        string
		os          spec.OSKind
		installRoot string
		app         string
		build       func(layout.Paths) string
		golden      string
	}{
		{"extract-windows", spec.OSWindows, `C:\deploy`, "sample-svc", extractScript, "extract_windows.ps1"},
		{"extract-linux", spec.OSLinux, "/opt/deploy", "svc", extractScript, "extract_linux.sh"},
		{"switch-windows", spec.OSWindows, `C:\deploy`, "sample-svc", switchScript, "switch_windows.ps1"},
		{"switch-linux", spec.OSLinux, "/opt/deploy", "svc", switchScript, "switch_linux.sh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := layout.NewPaths(c.os, c.installRoot, c.app, "1.2.3")
			got := c.build(p)
			path := filepath.Join("testdata", c.golden)
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run with -update): %v", path, err)
			}
			if got != string(want) {
				t.Errorf("%s script mismatch.\n got: %q\nwant: %q", c.name, got, string(want))
			}
		})
	}
}

// TestScriptHostilePathQuoting proves an install_root containing a single quote
// (which spec validation does NOT forbid) is escaped by shq so it cannot break
// out of the quoted shell argument in the generated linux scripts (evaluator
// feedback item 3). The escaped POSIX form of `'` is `'\''`.
func TestScriptHostilePathQuoting(t *testing.T) {
	hostile := `/opt/de'ploy;rm -rf ~` // embedded quote + shell metachars
	p := layout.NewPaths(spec.OSLinux, hostile, "svc", "1.0.0")
	for _, tc := range []struct {
		name  string
		build func(layout.Paths) string
	}{
		{"extract", extractScript},
		{"switch", switchScript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.build(p)
			// The literal quote must appear only as the escaped sequence.
			if strings.Contains(got, `de'ploy`) {
				t.Fatalf("%s: unescaped single quote leaked into script:\n%s", tc.name, got)
			}
			if !strings.Contains(got, `de'\''ploy`) {
				t.Fatalf("%s: expected shq-escaped quote `'\\''`, got:\n%s", tc.name, got)
			}
		})
	}
}
