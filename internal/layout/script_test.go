package layout

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// -update regenerates the committed golden fixtures under testdata/.
var update = flag.Bool("update", false, "update layout script golden fixtures")

func TestDirScriptGolden(t *testing.T) {
	cases := []struct {
		name        string
		os          spec.OSKind
		installRoot string
		app         string
		golden      string
	}{
		{"windows", spec.OSWindows, `C:\deploy`, "sample-svc", "dir_windows.ps1"},
		{"linux", spec.OSLinux, "/opt/deploy", "svc", "dir_linux.sh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewPaths(c.os, c.installRoot, c.app, "1.2.3")
			got := DirScript(p)
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
				t.Errorf("DirScript mismatch for %s.\n got: %q\nwant: %q", c.name, got, string(want))
			}
		})
	}
}
