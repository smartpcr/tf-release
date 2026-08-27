package layout

import (
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

func TestWindowsPaths(t *testing.T) {
	p := NewPaths(spec.OSWindows, `C:\deploy`, "sample-svc", "1.2.3")
	checks := []struct{ got, want string }{
		{p.Root, `C:\deploy\sample-svc`},
		{p.Releases, `C:\deploy\sample-svc\releases`},
		{p.Release, `C:\deploy\sample-svc\releases\1.2.3`},
		{p.Current, `C:\deploy\sample-svc\current`},
		{p.Shared, `C:\deploy\sample-svc\shared`},
		{p.SharedLogs, `C:\deploy\sample-svc\shared\logs`},
		{p.Staging, `C:\deploy\sample-svc\staging`},
		{p.Manifest, `C:\deploy\sample-svc\manifest.json`},
		{p.Lock, `C:\deploy\sample-svc\.lock`},
		{p.StagePkg, `C:\deploy\sample-svc\staging\pkg.zip`},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
	if strings.Contains(strings.TrimPrefix(p.Root, `C:\`), "/") {
		t.Errorf("windows path must use backslash separators: %q", p.Root)
	}
}

func TestLinuxPaths(t *testing.T) {
	p := NewPaths(spec.OSLinux, "/opt/deploy", "svc", "2.0.0")
	checks := []struct{ got, want string }{
		{p.Root, "/opt/deploy/svc"},
		{p.Releases, "/opt/deploy/svc/releases"},
		{p.Release, "/opt/deploy/svc/releases/2.0.0"},
		{p.Current, "/opt/deploy/svc/current"},
		{p.Shared, "/opt/deploy/svc/shared"},
		{p.SharedLogs, "/opt/deploy/svc/shared/logs"},
		{p.Staging, "/opt/deploy/svc/staging"},
		{p.Manifest, "/opt/deploy/svc/manifest.json"},
		{p.Lock, "/opt/deploy/svc/.lock"},
		{p.StagePkg, "/opt/deploy/svc/staging/pkg.zip"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
	if strings.Contains(p.Root, `\`) {
		t.Errorf("linux path must use forward-slash separators: %q", p.Root)
	}
}

func TestBuiltinEnvAndMerge(t *testing.T) {
	p := NewPaths(spec.OSWindows, `C:\deploy`, "app", "1.0.0")
	env := MergeEnv(BuiltinEnv("app", "1.0.0", p, 0), map[string]string{"MODE": "lab", "LD_APP": "override"})
	if env["LD_APP"] != "override" { // spec env wins over builtins
		t.Errorf("merge precedence: %v", env)
	}
	if env["LD_VERSION"] != "1.0.0" || env["LD_RELEASE_DIR"] == "" || env["LD_SHARED_DIR"] == "" {
		t.Errorf("builtins missing: %v", env)
	}
	if env["MODE"] != "lab" {
		t.Errorf("spec env missing: %v", env)
	}
	if _, ok := env["PORT"]; ok {
		t.Errorf("PORT must be absent for non-node (nodePort=0): %v", env)
	}
}

func TestBuiltinEnvNodePort(t *testing.T) {
	p := NewPaths(spec.OSLinux, "/opt/deploy", "web", "3.1.0")
	env := BuiltinEnv("web", "3.1.0", p, 8080)
	if env["PORT"] != "8080" {
		t.Errorf("node PORT missing/wrong: %v", env)
	}
	// spec environment still wins over the builtin PORT.
	merged := MergeEnv(env, map[string]string{"PORT": "9090"})
	if merged["PORT"] != "9090" {
		t.Errorf("spec PORT should win: %v", merged)
	}
}
