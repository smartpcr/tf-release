package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
		return strings.TrimSpace(fmt.Sprintf(`dotnet test %s --logger trx --results-directory .\TestResults --no-build %s`, proj, extra))
	case "npm":
		// DESIGN §7.2: `npm <args or "test">`. Raw extra_args win; else a named
		// script becomes `npm run <script>`; else the default `npm test`.
		if len(r.ExtraArgs) > 0 {
			return "npm " + strings.Join(r.ExtraArgs, " ")
		}
		if r.Script != "" {
			return "npm run " + r.Script
		}
		return "npm test"
	}
	return r.Command
}

// noResultCounters is the sentinel counter set emitted when results.format is
// none|empty: nothing is parsed, so every counter is -1 (DESIGN §5.3 outputs
// table "-1 when results.format: none"; E2E-07).
func noResultCounters() logs.Counters {
	return logs.Counters{Total: -1, Passed: -1, Failed: -1, Skipped: -1}
}

// evaluatePass applies pass_criteria (DESIGN §7.3) in-process: the runner exit
// code must be in the allowed set AND — only when a result format was parsed and
// total>0 — the pass rate passed/(total-skipped) must be >= min_pass_rate. When
// results.format is none the rate gate is skipped entirely (rateOK=true) and the
// verdict rides purely on exit_codes. Pure function so it is unit-testable
// without a transport.
func evaluatePass(exitCode int, format string, c logs.Counters, pc *spec.PassCriteria) (passed, exitOK, rateOK bool) {
	for _, code := range pc.EffectiveExitCodes() {
		if exitCode == code {
			exitOK = true
			break
		}
	}
	rateOK = true
	if format == "trx" || format == "junit" {
		denom := c.Total - c.Skipped
		rate := 1.0
		if denom > 0 {
			rate = float64(c.Passed) / float64(denom)
		} else if c.Total == 0 {
			rate = 0 // zero tests discovered ⇒ never passes rate gate (E2E-07)
		}
		rateOK = rate >= pc.EffectiveMinPassRate()
	}
	return exitOK && rateOK, exitOK, rateOK
}

// RunTest = fetch+extract test package on target, execute runner, collect
// results+logs to runner destination_dir, parse, evaluate pass criteria.
// Collection ALWAYS runs before pass evaluation (E2E-05: collect-then-fail).
func (e *Engine) RunTest(ctx context.Context, tr *spec.TestRun) (outcome *TestOutcome, err error) {
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
	// STAGING WIPE (start): whole-directory wipe on EVERY run, cached or not
	// (DESIGN §9.1 "wiped at start & end of every op").
	if err := e.wipeStaging(ctx, t, p); err != nil {
		return nil, coded("ERR_CONNECT", host, "STAGE", err)
	}
	// STAGING WIPE (end): declared FIRST so it runs LAST (LIFO), after the
	// incomplete-release cleanup below. Surface a cleanup failure as the op error
	// when the run otherwise succeeded; warn (preserving root cause) otherwise.
	defer func() {
		if werr := e.wipeStaging(ctx, t, p); werr != nil {
			if err == nil {
				err = coded("ERR_CONNECT", host, "STAGE", fmt.Errorf("staging cleanup: %w", werr))
				outcome = nil
			} else {
				e.warnf("staging cleanup failed on %s after prior error: %v", host, werr)
			}
		}
	}()
	// INCOMPLETE-RELEASE CLEANUP: declared SECOND so it runs FIRST, while `err`
	// still holds the genuine run result. Only a release THIS run created is
	// removed, and only on a pre-execution failure (DESIGN §10.2). Cleanup
	// failures are joined onto the op error.
	createdRelease := false
	defer func() {
		if err != nil && createdRelease {
			err = e.cleanupIncompleteRelease(ctx, t, p, host, err)
		}
	}()
	cached, cerr := e.releaseCached(ctx, t, p, tr.Artifact.Version, tr.Artifact.Checksum)
	if cerr != nil {
		return nil, coded("ERR_CONNECT", host, "STAGE", cerr)
	}
	if !cached {
		if ferr := e.fetchToStaging(ctx, t, dep, p); ferr != nil {
			return nil, ferr // fetch writes only to staging; release tree untouched
		}
		// extract creates the release dir — from here it is "ours".
		createdRelease = true
		if xerr := e.extract(ctx, t, p); xerr != nil {
			return nil, xerr
		}
		if merr := e.writeReleaseMarker(ctx, t, p, dep); merr != nil {
			return nil, coded("ERR_CONNECT", host, "STAGE", merr)
		}
	}
	// Staging is complete: the release is now fully extracted and marked. Clear
	// the cleanup guard so a LATER failure (test execution, result collection, or
	// result parsing) does NOT delete a good release or its collected `_results`
	// — the incomplete-release cleanup is strictly a pre-execution/staging-only
	// failure remedy (DESIGN §10.2).
	createdRelease = false

	env := layout.MergeEnv(layout.BuiltinEnv(tr.Metadata.Name, tr.Artifact.Version, p, 0), tr.Runner.Env)
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
		script := fmt.Sprintf("cd %s && %s", shq(workDir), cmdline)
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, Env: env, TimeoutSec: timeout})
	}
	duration := int(time.Since(startedAt).Seconds())

	// COLLECT (always) — results dirs + configured logs + event logs. This block
	// runs even when the runner timed out or the transport failed, so partial
	// results/logs are captured BEFORE the error is surfaced (DESIGN §5.3
	// "collection always happens before returning a test-failure error"; E2E-04
	// "partial logs collected" on runner.timeout_seconds). The timeout/transport
	// error is surfaced right after the collection block below.
	dest := tr.Collect.EffectiveDestinationDir(tr.Metadata.Name)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		// If the runner already failed, THAT is the primary error and the
		// missing local results dir is secondary; otherwise the run succeeded
		// and an uncreatable results dir is itself the failure.
		if xerr != nil {
			return nil, testRunErr(xerr, host)
		}
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
	// CollectFiles extracts the archive itself (preserving relative paths) and
	// returns the result files; parse them by walking the whole results tree.
	_, warns := logs.CollectFiles(ctx, t, resultGlobs, filepath.Join(dest, "results"))
	collectWarns = append(collectWarns, warns...)
	localResults := filepath.Join(dest, "results")
	// 2. Extra log globs — CollectFiles extracts them into results_dir/logs/<host>/.
	// Relative globs resolve under the named deployment's shared dir when
	// `collect.app_shared_of` is set (DESIGN §7:373); absolute globs pass through.
	if len(tr.Collect.Logs) > 0 {
		logGlobs := tr.Collect.Logs
		if tr.Collect.AppSharedOf != "" {
			sp := layout.NewPaths(t.OS(), tr.EffectiveWorkRoot(t.OS()), tr.Collect.AppSharedOf, "")
			logGlobs = resolveTargetGlobs(t.OS(), sp.Shared, tr.Collect.Logs)
		}
		_, w := logs.CollectFiles(ctx, t, logGlobs, filepath.Join(dest, "logs"))
		collectWarns = append(collectWarns, w...)
	}
	// 3. Windows event logs since test start.
	if len(tr.Collect.WindowsEventLogs) > 0 {
		_, w := logs.CollectEventLogs(ctx, t, tr.Collect.WindowsEventLogs, startedAt, filepath.Join(dest, "events"))
		collectWarns = append(collectWarns, w...)
	}
	for _, w := range collectWarns {
		e.warnf("%s", w)
	}
	// Stdout/stderr tails always captured (DESIGN §7.4).
	_ = os.WriteFile(filepath.Join(dest, "runner-stdout.txt"), []byte(tail(r.Stdout, 200_000)), 0o644)
	_ = os.WriteFile(filepath.Join(dest, "runner-stderr.txt"), []byte(tail(r.Stderr, 200_000)), 0o644)

	// Best-effort collection has run; NOW surface a runner timeout / transport
	// failure so an always() publish step still sees the partial results_dir
	// (DESIGN §5.3; E2E-04). Result parsing and pass evaluation are skipped.
	if xerr != nil {
		return nil, testRunErr(xerr, host)
	}

	out := &TestOutcome{ExitCode: r.ExitCode, ResultsDir: dest, DurationSeconds: duration}

	// PARSE results if a format is configured; otherwise emit -1 sentinels so
	// consumers can distinguish "not measured" from a genuine zero (E2E-07).
	c := noResultCounters()
	if f := tr.Results.Format; f == "trx" || f == "junit" {
		parsed, matched, perr := logs.SumResults(f, localResults, tr.Results.Paths)
		if perr != nil {
			// Missing/corrupt results with format set ⇒ ERR_TEST_FAILED (E2E-06).
			return out, coded("ERR_TEST_FAILED", host, "TEST",
				fmt.Errorf("result parsing (%s, matched=%d): %v", f, len(matched), perr))
		}
		c = parsed
	}
	out.Total, out.PassedTests, out.FailedTests, out.SkippedTests = c.Total, c.Passed, c.Failed, c.Skipped

	// PASS CRITERIA (DESIGN §7.3): exit code ∈ allowed AND pass-rate ≥ min.
	passed, exitOK, rateOK := evaluatePass(r.ExitCode, tr.Results.Format, c, &tr.PassCriteria)
	out.Passed = passed
	out.Summary = summarize(out, exitOK, rateOK)
	writeSummaryJSON(dest, tr, out, startedAt)
	return out, nil
}

// testRunErr classifies a runner transport failure so it can be surfaced AFTER
// best-effort collection (DESIGN §5.3; E2E-04): a "timed out" transport error
// maps to ERR_TIMEOUT, anything else to the standard transport-mapped error.
func testRunErr(xerr error, host string) error {
	if strings.Contains(xerr.Error(), "timed out") {
		return coded("ERR_TIMEOUT", host, "TEST", xerr)
	}
	return wrapTransportErr(xerr, host, "TEST")
}

func summarize(o *TestOutcome, exitOK, rateOK bool) string {
	return fmt.Sprintf("passed=%t exit=%d(ok=%t) tests=%d passed=%d failed=%d skipped=%d rate_ok=%t duration=%ds",
		o.Passed, o.ExitCode, exitOK, o.Total, o.PassedTests, o.FailedTests, o.SkippedTests, rateOK, o.DurationSeconds)
}

// writeSummaryJSON emits the machine-readable summary.json (DESIGN §7.4). Field
// names mirror the design's example document. `pass_rate` is -1 when no result
// format was parsed (counters are the -1 sentinels), matching the counters.
func writeSummaryJSON(dest string, tr *spec.TestRun, o *TestOutcome, started time.Time) {
	passRate := -1.0
	if tr.Results.Format == "trx" || tr.Results.Format == "junit" {
		denom := o.Total - o.SkippedTests
		if denom > 0 {
			passRate = float64(o.PassedTests) / float64(denom)
		} else {
			passRate = 0
		}
	}
	doc := map[string]interface{}{
		"name":             tr.Metadata.Name,
		"version":          tr.Artifact.Version,
		"host":             strings.ToLower(tr.Target.Hosts[0]),
		"passed":           o.Passed,
		"exit_code":        o.ExitCode,
		"total":            o.Total,
		"passed_tests":     o.PassedTests,
		"failed":           o.FailedTests,
		"skipped":          o.SkippedTests,
		"pass_rate":        passRate,
		"duration_seconds": o.DurationSeconds,
		"criteria": map[string]interface{}{
			"exit_codes":    tr.PassCriteria.EffectiveExitCodes(),
			"min_pass_rate": tr.PassCriteria.EffectiveMinPassRate(),
		},
		"error_code":   "",
		"started_utc":  started.UTC().Format(time.RFC3339),
		"finished_utc": time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	_ = os.WriteFile(filepath.Join(dest, "summary.json"), b, 0o644)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
