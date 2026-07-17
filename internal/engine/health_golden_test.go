package engine

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// update regenerates the committed golden scripts: `go test ./internal/engine -run Golden -update`.
var update = flag.Bool("update", false, "update health-check script golden files")

// TestHealthScriptGolden asserts that, for each health_check.type, the script
// generated for windows and linux matches the committed golden under
// testdata/health/ and encodes expect_status / expect_body_regex logic
// (Scenario: "Health script generation").
func TestHealthScriptGolden(t *testing.T) {
	cases := []struct {
		name string
		hc   *spec.HealthCheck
	}{
		{"http_default", &spec.HealthCheck{Type: "http", HTTP: spec.HTTPCheck{URL: "http://localhost:8080/health"}}},
		{"http_expect", &spec.HealthCheck{Type: "http", HTTP: spec.HTTPCheck{
			URL: "http://localhost:8080/health", ExpectStatus: 503, ExpectBodyRegex: "v=1\\.2\\.3"}}},
		{"tcp", &spec.HealthCheck{Type: "tcp", TCP: spec.TCPCheck{Port: 5432}}},
		{"exec", &spec.HealthCheck{Type: "exec", Exec: spec.ExecCheck{Command: "bin\\healthcheck.exe --probe"}}},
	}
	oses := []struct {
		kind    spec.OSKind
		label   string
		workDir string
	}{
		{spec.OSWindows, "windows", `C:\deploy\app\releases\1.0.0`},
		{spec.OSLinux, "linux", "/opt/deploy/app/releases/1.0.0"},
	}
	for _, c := range cases {
		for _, o := range oses {
			cmd := buildProbeCmd(o.kind, c.hc, o.workDir, nil)
			golden := filepath.Join("testdata", "health", c.name+"_"+o.label+".golden")
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
				t.Errorf("script mismatch for %s/%s:\n--- want ---\n%s\n--- got ---\n%s",
					c.name, o.label, want, cmd.Script)
			}
		}
	}

	// Type "none" yields an empty script (probe is skipped entirely).
	if s := buildProbeCmd(spec.OSLinux, &spec.HealthCheck{Type: "none"}, "/x", nil).Script; s != "" {
		t.Fatalf("none should produce no script, got %q", s)
	}
}
