package layout

import (
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

func TestWindowsPaths(t *testing.T) {
	p := NewPaths(spec.OSWindows, `C:\deploy`, "sample-svc", "1.2.3")
	want := map[string]string{
		p.Root:     `C:\deploy\sample-svc`,
		p.Releases: `C:\deploy\sample-svc\releases`,
		p.Release:  `C:\deploy\sample-svc\releases\1.2.3`,
		p.Current:  `C:\deploy\sample-svc\current`,
		p.Shared:   `C:\deploy\sample-svc\shared`,
		p.Manifest: `C:\deploy\sample-svc\manifest.json`,
		p.Lock:     `C:\deploy\sample-svc\.lock`,
	}
	for got, exp := range want {
		if got != exp {
			t.Errorf("got %q want %q", got, exp)
		}
	}
	if !strings.HasPrefix(p.StagePkg, `C:\deploy\sample-svc\staging\`) {
		t.Errorf("stage pkg: %q", p.StagePkg)
	}
}

func TestLinuxPaths(t *testing.T) {
	p := NewPaths(spec.OSLinux, "/opt/deploy", "svc", "2.0.0")
	if p.Release != "/opt/deploy/svc/releases/2.0.0" || p.Current != "/opt/deploy/svc/current" {
		t.Errorf("linux layout wrong: %+v", p)
	}
}

func TestBuiltinEnvAndMerge(t *testing.T) {
	p := NewPaths(spec.OSWindows, `C:\deploy`, "app", "1.0.0")
	env := MergeEnv(BuiltinEnv("app", "1.0.0", p), map[string]string{"MODE": "lab", "LD_APP": "override"})
	if env["LD_APP"] != "override" { // spec env wins over builtins
		t.Errorf("merge precedence: %v", env)
	}
	if env["LD_VERSION"] != "1.0.0" || env["LD_RELEASE_DIR"] == "" || env["LD_SHARED_DIR"] == "" {
		t.Errorf("builtins missing: %v", env)
	}
	if env["MODE"] != "lab" {
		t.Errorf("spec env missing: %v", env)
	}
}
