package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/logs"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// TestOutcome mirrors labdeploy_e2e_test computed attrs (DESIGN §5.3).
type TestOutcome struct {
	Passed          bool
	ExitCode        int
	Total           int
	PassedTests     int
	FailedTests     int
	SkippedTests    int
	ResultsDir      string
	DurationSeconds int
	Summary         string
}

// runnerCommand renders the concrete command per runner.type (DESIGN §7.2).
func runnerCommand(t *spec.TestRun) string {
	r := t.Runner
	switch r.Type {
	case "exec":
		if len(r.Args) > 0 {
			return r.Command + " " + strings.Join(r.Args, " ")
		}
		return r.Command
	case "vstest":
		asm := strings.Join(r.Assemblies, " ")
		extra := strings.Join(r.ExtraArgs, " ")
		return strings.TrimSpace(fmt.Sprintf(`vstest.console.exe %s /Logger:trx /ResultsDirectory:.\TestResults %s`, asm, extra))
	case "dotnet_test":
		proj := r.Project
		extra := strings.Join(r.ExtraArgs, " ")
		return strings.TrimSpace(fmt.Sprintf(`dotnet test %s --logger trx --results-directory .\TestResults %s`, proj, extra))
	case "npm":
		script := r.Script
		if script == "" {
			script = "test"
		}
		return "npm run " + script
	}
	return r.Command
}

// RunTest = fetch+extract test package on target, execute runner, collect
// results+logs to runner destination_dir, parse, evaluate pass criteria.
// Collection ALWAYS runs before pass evaluation (E2E-05: collect-then-fail).
func (e *Engine) RunTest(ctx context.Context, tr *spec.TestRun) (*TestOutcome, error) {
	host := strings.ToLower(tr.Target.Hosts[0])
	t, err := e.NewTransport(&tr.Target, host)
	if err != nil {
		return nil, err
	}
	if err := t.Connect(ctx); err != nil {
		return nil, wrapTransportErr(err, host, "CONNECT")
	}
	defer t.Close()

	// Test workspace: <install_root>/<name>-tests/runs/<version>.
	root := tr.EffectiveWorkRoot(tr.Target.OS)
	p := layout.NewPaths(tr.Target.OS, root, tr.Metadata.Name+"-tests", tr.Artifact.Version)
	dep := &spec.Deployment{ // reuse staging plumbing with a synthetic deployment
		APIVersion: tr.APIVersion, Kind: "Deployment", Metadata: tr.Metadata,
		Target: tr.Target, Artifact: tr.Artifact, Environment: tr.Runner.Env,
	}
	if err := ensureLayout(ctx, t, p); err != nil {
		return nil, coded("ERR_CONNECT", host, "STAGE", err)
	}
	cached, err := e.releaseCached(ctx, t, p, tr.Artifact.Checksum)
	if err != nil {
		return nil, coded("ERR_CONNECT", host, "STAGE", err)
	}
	if !cached {
		if err := e.fetchToStaging(ctx, t, dep, p); err != nil {
			return nil, err
		}
		if err := e.extract(ctx, t, p); err != nil {
			return nil, err
		}
		if err := e.writeReleaseMarker(ctx, t, p, dep); err != nil {
			return nil, coded("ERR_CONNECT", host, "STAGE", err)
		}
	}

	env := layout.MergeEnv(layout.BuiltinEnv(tr.Metadata.Name, tr.Artifact.Version, p), tr.Runner.Env)
	cmdline := runnerCommand(tr)
	timeout := tr.Runner.EffectiveTimeout()
	startedAt := time.Now()

	var r transport.Result
	var xerr error
	workDir := p.Release
	if wd := tr.Runner.WorkingDir; wd != "" {
		if t.OS() == spec.OSWindows {
			workDir = p.Release + `\` + strings.ReplaceAll(strings.TrimLeft(wd, `\/`), "/", `\`)
		} else {
			workDir = p.Release + "/" + strings.TrimLeft(wd, "/")
		}
	}
	if t.OS() == spec.OSWindows {
		script := fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(workDir), cmdline)
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, Env: env, TimeoutSec: timeout})
	} else {
		script := fmt.Sprintf("cd '%s' && %s", workDir, cmdline)
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, Env: env, TimeoutSec: timeout})
	}
	duration := int(time.Since(startedAt).Seconds())
	if xerr != nil {
		if strings.Contains(xerr.Error(), "timed out") {
			return nil, coded("ERR_TIMEOUT", host, "TEST", xerr)
		}
		return nil, wrapTransportErr(xerr, host, "TEST")
	}

	// COLLECT (always) — results dirs + configured logs + event logs.
	dest := tr.Collect.EffectiveDestinationDir()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir results dir %s: %w", dest, err)
	}
	var collectWarns []string
	// 1. Pull the whole TestResults / result paths back for parsing.
	resultGlobs := make([]string, 0, len(tr.Results.Paths))
	for _, rp := range tr.Results.Paths {
		if t.OS() == spec.OSWindows {
			resultGlobs = append(resultGlobs, p.Release+`\`+strings.ReplaceAll(strings.TrimLeft(rp, `\/`), "/", `\`))
		} else {
			resultGlobs = append(resultGlobs, p.Release+"/"+strings.TrimLeft(rp, "/"))
		}
	}
	files, warns := logs.CollectFiles(ctx, t, resultGlobs, filepath.Join(dest, "results"))
	collectWarns = append(collectWarns, warns...)
	localResults := filepath.Join(dest, "results", sanitizeHost(host))
	if len(files) > 0 {
		if err := unpackLocal(files[0], localResults); err != nil {
			collectWarns = append(collectWarns, fmt.Sprintf("unpack results: %v", err))
		}
	}
	// 2. Extra log globs.
	if len(tr.Collect.Logs) > 0 {
		_, w := logs.CollectFiles(ctx, t, tr.Collect.Logs, filepath.Join(dest, "logs"))
		collectWarns = append(collectWarns, w...)
	}
	// 3. Windows event logs since test start.
	if len(tr.Collect.WindowsEventLogs) > 0 {
		_, w := logs.CollectEventLogs(ctx, t, tr.Collect.WindowsEventLogs, startedAt.Add(-time.Minute), filepath.Join(dest, "events"))
		collectWarns = append(collectWarns, w...)
	}
	for _, w := range collectWarns {
		e.warnf("%s", w)
	}
	// Stdout/stderr tails always captured (DESIGN §7.4).
	_ = os.WriteFile(filepath.Join(dest, "runner-stdout.txt"), []byte(tail(r.Stdout, 200_000)), 0o644)
	_ = os.WriteFile(filepath.Join(dest, "runner-stderr.txt"), []byte(tail(r.Stderr, 200_000)), 0o644)

	out := &TestOutcome{ExitCode: r.ExitCode, ResultsDir: dest, DurationSeconds: duration}

	// PARSE results if a format is configured.
	if f := tr.Results.Format; f == "trx" || f == "junit" {
		c, matched, perr := logs.SumResults(f, localResults, flatten(tr.Results.Paths))
		if perr != nil {
			// Missing/corrupt results with format set ⇒ ERR_TEST_FAILED (E2E-06).
			return out, coded("ERR_TEST_FAILED", host, "TEST",
				fmt.Errorf("result parsing (%s, matched=%d): %v", f, len(matched), perr))
		}
		out.Total, out.PassedTests, out.FailedTests, out.SkippedTests = c.Total, c.Passed, c.Failed, c.Skipped
	}

	// PASS CRITERIA (DESIGN §7.3): exit code ∈ allowed AND pass-rate ≥ min.
	exitOK := false
	for _, c := range tr.PassCriteria.EffectiveExitCodes() {
		if r.ExitCode == c {
			exitOK = true
			break
		}
	}
	rateOK := true
	if tr.Results.Format == "trx" || tr.Results.Format == "junit" {
		denom := out.Total - out.SkippedTests
		rate := 1.0
		if denom > 0 {
			rate = float64(out.PassedTests) / float64(denom)
		} else if out.Total == 0 {
			rate = 0 // zero tests discovered ⇒ never passes rate gate (E2E-07)
		}
		rateOK = rate >= tr.PassCriteria.EffectiveMinPassRate()
	}
	out.Passed = exitOK && rateOK
	out.Summary = summarize(out, exitOK, rateOK)
	writeSummaryJSON(dest, tr, out, startedAt)
	return out, nil
}

func summarize(o *TestOutcome, exitOK, rateOK bool) string {
	return fmt.Sprintf("passed=%t exit=%d(ok=%t) tests=%d passed=%d failed=%d skipped=%d rate_ok=%t duration=%ds",
		o.Passed, o.ExitCode, exitOK, o.Total, o.PassedTests, o.FailedTests, o.SkippedTests, rateOK, o.DurationSeconds)
}

// writeSummaryJSON emits the machine-readable summary.json (DESIGN §7.4).
func writeSummaryJSON(dest string, tr *spec.TestRun, o *TestOutcome, started time.Time) {
	doc := map[string]interface{}{
		"schema": 1, "name": tr.Metadata.Name, "version": tr.Artifact.Version,
		"host":   strings.ToLower(tr.Target.Hosts[0]),
		"passed": o.Passed, "exit_code": o.ExitCode,
		"total": o.Total, "passed_tests": o.PassedTests,
		"failed_tests": o.FailedTests, "skipped_tests": o.SkippedTests,
		"duration_seconds": o.DurationSeconds,
		"started_utc":      started.UTC().Format(time.RFC3339),
		"finished_utc":     time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	_ = os.WriteFile(filepath.Join(dest, "summary.json"), b, 0o644)
}

// unpackLocal expands the downloaded logs.zip/tar.gz into dir for parsing.
func unpackLocal(archive, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(archive, ".zip") {
		return runLocal("unzip", "-o", "-q", archive, "-d", dir)
	}
	return runLocal("tar", "xzf", archive, "-C", dir)
}

func runLocal(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %v: %s", name, args, err, tail(string(out), 500))
	}
	return nil
}

// flatten strips directory components so globs match against the unpacked
// flat archive layout (Compress-Archive flattens; tar preserves — match base).
func flatten(patterns []string) []string {
	out := make([]string, 0, len(patterns)*2)
	for _, p := range patterns {
		out = append(out, p, filepath.Base(p))
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func sanitizeHost(h string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, strings.ToLower(h))
}
