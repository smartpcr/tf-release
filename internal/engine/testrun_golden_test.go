package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// TestRunnerCommandGolden pins the expanded runner command for each runner.type
// (Stage 7.1 scenario "Runner expansion golden", DESIGN §7.2). The committed
// goldens under testdata/runner/ capture the results-dir logger flags
// (`/Logger:trx /ResultsDirectory:...` for vstest, `--logger trx
// --results-directory ... --no-build` for dotnet_test) so a regression in the
// expanded command line is caught byte-for-byte. Regenerate with `-update`
// (flag declared once in script_golden_test.go).
func TestRunnerCommandGolden(t *testing.T) {
	cases := []struct {
		name string
		tr   *spec.TestRun
	}{
		{"exec", &spec.TestRun{Runner: spec.Runner{
			Type: "exec", Command: `bin\smoke.exe`, Args: []string{"--fast", "--ci"}}}},
		{"exec_no_args", &spec.TestRun{Runner: spec.Runner{
			Type: "exec", Command: "./run-tests.sh"}}},
		{"vstest", &spec.TestRun{Runner: spec.Runner{
			Type: "vstest", Assemblies: []string{`tests\Unit.dll`, `tests\Integration.dll`},
			ExtraArgs: []string{"/Parallel"}}}},
		{"dotnet_test", &spec.TestRun{Runner: spec.Runner{
			Type: "dotnet_test", Project: `tests\Suite.csproj`,
			ExtraArgs: []string{"--filter", "Category=Smoke"}}}},
		{"npm_default", &spec.TestRun{Runner: spec.Runner{Type: "npm"}}},
		{"npm_script", &spec.TestRun{Runner: spec.Runner{Type: "npm", Script: "e2e"}}},
		{"npm_args", &spec.TestRun{Runner: spec.Runner{
			Type: "npm", ExtraArgs: []string{"run", "test:ci", "--", "--reporter=junit"}}}},
	}
	for _, c := range cases {
		got := runnerCommand(c.tr)
		golden := filepath.Join("testdata", "runner", c.name+".golden")
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("missing golden %s (run with -update): %v", golden, err)
		}
		if string(want) != got {
			t.Errorf("runner command mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s",
				c.name, want, got)
		}
	}
}
