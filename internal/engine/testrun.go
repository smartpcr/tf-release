package engine

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	// timeoutSentinel is the exit code the runner watchdog exits with after it
	// force-kills a timed-out process tree; it lets the engine surface
	// ERR_TIMEOUT deterministically even when the transport reports no error.
	timeoutSentinel = 124
	// timeoutMarker is echoed by the watchdog on timeout so the engine can
	// recognize a timeout from captured output regardless of transport.
	timeoutMarker = "__LABDEPLOY_TIMEOUT__"
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

// runnerCommand renders the concrete command per runner.type (DESIGN §7.2). The
// normative argument source is `runner.args` for EVERY runner type; the typed
// convenience fields (assemblies/project/script) are folded in additively so a
// spec may supply either. `<args>` is always emitted BEFORE the framework's
// results-dir logger flags exactly as the DESIGN §7 table specifies.
func runnerCommand(t *spec.TestRun) string {
	r := t.Runner
	switch r.Type {
	case "exec":
		args := runnerArgList(r)
		if len(args) > 0 {
			return r.Command + " " + strings.Join(args, " ")
		}
		return r.Command
	case "vstest":
		// vstest.console.exe <assemblies + args> /Logger:trx /ResultsDirectory:<dir>
		args := append(append([]string{}, r.Assemblies...), runnerArgList(r)...)
		cmd := "vstest.console.exe"
		if len(args) > 0 {
			cmd += " " + strings.Join(args, " ")
		}
		return cmd + ` /Logger:trx /ResultsDirectory:.\TestResults`
	case "dotnet_test":
		// dotnet test <project + args> --logger trx --results-directory <dir> --no-build
		var args []string
		if r.Project != "" {
			args = append(args, r.Project)
		}
		args = append(args, runnerArgList(r)...)
		cmd := "dotnet test"
		if len(args) > 0 {
			cmd += " " + strings.Join(args, " ")
		}
		return cmd + ` --logger trx --results-directory .\TestResults --no-build`
	case "npm":
		// DESIGN §7.2: `npm <args or "test">`. runner.args (or extra_args) win;
		// else a named script becomes `npm run <script>`; else default `npm test`.
		if args := runnerArgList(r); len(args) > 0 {
			return "npm " + strings.Join(args, " ")
		}
		if r.Script != "" {
			return "npm run " + r.Script
		}
		return "npm test"
	}
	return r.Command
}

// runnerArgList is the normative `<args>` for a runner: `runner.args` first
// (DESIGN §7 table), then any `extra_args` appended, so both the standard and
// the convenience field flow into the expanded command.
func runnerArgList(r spec.Runner) []string {
	args := make([]string, 0, len(r.Args)+len(r.ExtraArgs))
	args = append(args, r.Args...)
	args = append(args, r.ExtraArgs...)
	return args
}

// noResultCounters is the sentinel counter set emitted when results.format is
// none|empty: nothing is parsed, so every counter is -1 (DESIGN §5.3 outputs
// table "-1 when results.format: none"; E2E-07).
func noResultCounters() logs.Counters {
	return logs.Counters{Total: -1, Passed: -1, Failed: -1, Skipped: -1}
}

// computePassRate is the SINGLE source of truth for the pass_rate VALUE, shared
// by evaluatePass and writeSummaryJSON so the persisted rate agrees with the
// verdict (DESIGN §7.3 `pass_rate = passed/(total-skipped)`). It returns:
//   - -1 when results.format is none|empty (nothing measured),
//   - passed/(total-skipped) when at least one non-skipped test ran,
//   - 0 when a format was parsed but zero tests ran (rate is NOT enforced in
//     this case — see evaluatePass — but 0 is the reported value),
//   - 1 when tests ran but every one was skipped (no failures ⇒ vacuous pass).
func computePassRate(format string, c logs.Counters) float64 {
	if format != "trx" && format != "junit" {
		return -1
	}
	denom := c.Total - c.Skipped
	if denom > 0 {
		return float64(c.Passed) / float64(denom)
	}
	if c.Total == 0 {
		return 0
	}
	return 1
}

// evaluatePass applies pass_criteria (DESIGN §7.3) in-process: the runner exit
// code must be in the allowed set AND — only when a result format was parsed and
// at least one test was reported (`total > 0`, DESIGN §7 line 372) — the pass
// rate (see computePassRate) must be >= min_pass_rate. When results.format is
// none OR no tests were reported, the rate gate is skipped entirely
// (rateOK=true) and the verdict rides purely on exit_codes. Pure function so it
// is unit-testable without a transport.
func evaluatePass(exitCode int, format string, c logs.Counters, pc *spec.PassCriteria) (passed, exitOK, rateOK bool) {
	for _, code := range pc.EffectiveExitCodes() {
		if exitCode == code {
			exitOK = true
			break
		}
	}
	rateOK = true
	// min_pass_rate is enforced ONLY when a format was parsed and total > 0
	// (DESIGN §7 line 372); a zero-test run is exit-code-only.
	if (format == "trx" || format == "junit") && c.Total > 0 {
		rateOK = computePassRate(format, c) >= pc.EffectiveMinPassRate()
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
		script := runnerScriptWindows(workDir, cmdline, timeout)
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellPowerShell, Script: script, Env: env, TimeoutSec: backstopTimeout(timeout)})
	} else {
		script := runnerScriptLinux(workDir, cmdline, timeout)
		r, xerr = t.Exec(ctx, transport.Cmd{Shell: transport.ShellSh, Script: script, Env: env, TimeoutSec: backstopTimeout(timeout)})
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
	// A timeout is recognized DETERMINISTICALLY (watchdog sentinel/marker OR a
	// transport-level timeout error) so ERR_TIMEOUT is reliable across the
	// ssh/winrm/local transports (E2E-04).
	if timedOut := runnerTimedOut(r, xerr); timedOut || xerr != nil {
		if timedOut {
			return nil, coded("ERR_TIMEOUT", host, "TEST",
				fmt.Errorf("runner exceeded %ds; process tree killed", timeout))
		}
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
	// The summary.json is a REQUIRED artifact (DESIGN §7.4); a marshal/write
	// failure must fail the op rather than silently yield an outcome without it.
	if werr := writeSummaryJSON(dest, tr, out, startedAt); werr != nil {
		return out, coded("ERR_TEST_FAILED", host, "TEST", werr)
	}
	return out, nil
}

// testRunErr classifies a runner transport failure so it can be surfaced AFTER
// best-effort collection (DESIGN §5.3; E2E-04): a timeout-shaped transport error
// maps to ERR_TIMEOUT, anything else to the standard transport-mapped error.
func testRunErr(xerr error, host string) error {
	if isTimeoutErr(xerr) {
		return coded("ERR_TIMEOUT", host, "TEST", xerr)
	}
	return wrapTransportErr(xerr, host, "TEST")
}

// backstopTimeout is the transport-level timeout for the runner exec. The
// in-script watchdog owns the primary kill at exactly `timeout`s (and emits the
// process-tree kill), so the transport backstop is set slightly longer to let
// the watchdog fire first and yield a deterministic sentinel exit; the backstop
// only matters if the watchdog itself is wedged. timeout<=0 ⇒ 0 (no limit).
func backstopTimeout(timeout int) int {
	if timeout <= 0 {
		return 0
	}
	return timeout + 30
}

// runnerScriptWindows wraps cmdline in a PowerShell watchdog that force-kills the
// WHOLE child process TREE (`taskkill /PID <id> /T /F`) when timeout is exceeded,
// prints the timeout marker, and exits with the timeout sentinel (DESIGN §5.3;
// E2E-04 "process-tree kill on runner.timeout_seconds"). timeout<=0 runs the
// command unwrapped.
func runnerScriptWindows(workDir, cmdline string, timeout int) string {
	if timeout <= 0 {
		return fmt.Sprintf("Set-Location %s\n%s\nexit $LASTEXITCODE", psq(workDir), cmdline)
	}
	return fmt.Sprintf(`Set-Location %s
$__pi = Start-Process -FilePath 'cmd.exe' -ArgumentList '/c',%s -PassThru -NoNewWindow
if ($__pi.WaitForExit(%d)) { exit $__pi.ExitCode }
taskkill /PID $__pi.Id /T /F | Out-Null
Write-Output '%s'
exit %d`, psq(workDir), psq(cmdline), timeout*1000, timeoutMarker, timeoutSentinel)
}

// runnerScriptLinux wraps cmdline in an sh watchdog that runs the command in a
// NEW SESSION (`setsid` ⇒ its pid is the process-GROUP id) and force-kills the
// whole group (`kill -9 -<pgid>`) when timeout is exceeded, prints the timeout
// marker, and exits with the timeout sentinel (DESIGN §5.3; E2E-04). timeout<=0
// runs the command unwrapped.
func runnerScriptLinux(workDir, cmdline string, timeout int) string {
	if timeout <= 0 {
		return fmt.Sprintf("cd %s && %s", shq(workDir), cmdline)
	}
	return fmt.Sprintf(`cd %s || exit 1
setsid sh -c %s &
__pgid=$!
( sleep %d; kill -9 -$__pgid 2>/dev/null ) &
__watch=$!
wait $__pgid
__rc=$?
if kill -0 $__watch 2>/dev/null; then
  kill $__watch 2>/dev/null
else
  echo '%s' >&2
  __rc=%d
fi
exit $__rc`, shq(workDir), shq(cmdline), timeout, timeoutMarker, timeoutSentinel)
}

// runnerTimedOut reports whether the runner exec hit its timeout, recognized
// deterministically across transports: the watchdog's sentinel exit code, the
// watchdog marker on either stream, OR a transport-level timeout error. This is
// what makes ERR_TIMEOUT reliable regardless of which transport served the exec
// (the prior substring-only check missed winrm/local context-deadline timeouts).
func runnerTimedOut(r transport.Result, xerr error) bool {
	if r.ExitCode == timeoutSentinel ||
		strings.Contains(r.Stdout, timeoutMarker) ||
		strings.Contains(r.Stderr, timeoutMarker) {
		return true
	}
	return isTimeoutErr(xerr)
}

// isTimeoutErr recognizes the several shapes a runner timeout takes across
// transports: ssh's "exec timed out after Ns", winrm/local context deadline
// (context.DeadlineExceeded / "deadline exceeded"), and a process killed by the
// context cancellation ("signal: killed").
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := err.Error()
	for _, m := range []string{"timed out", "deadline exceeded", "signal: killed"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// DeleteTestDir best-effort removes the remote test workspace
// (`<install_root>/<name>-tests`) created by RunTest. It is called from the
// resource Delete and MUST NOT fail a Terraform destroy: a missing dir is a
// no-op (removePath is idempotent) and any transport/connect error is returned
// for the caller to surface as a WARNING only (DESIGN §5.3 "Delete removes the
// remote test dir best effort, never fails destroy").
func (e *Engine) DeleteTestDir(ctx context.Context, tr *spec.TestRun) error {
	host := strings.ToLower(tr.Target.Hosts[0])
	t, err := e.NewTransport(&tr.Target, host)
	if err != nil {
		return err
	}
	if err := t.Connect(ctx); err != nil {
		return wrapTransportErr(err, host, "DELETE")
	}
	defer t.Close()
	root := tr.EffectiveWorkRoot(tr.Target.OS)
	p := layout.NewPaths(tr.Target.OS, root, tr.Metadata.Name+"-tests", tr.Artifact.Version)
	if err := e.removePath(ctx, t, p.Root); err != nil {
		return coded("ERR_CONNECT", host, "DELETE", err)
	}
	return nil
}

func summarize(o *TestOutcome, exitOK, rateOK bool) string {
	return fmt.Sprintf("passed=%t exit=%d(ok=%t) tests=%d passed=%d failed=%d skipped=%d rate_ok=%t duration=%ds",
		o.Passed, o.ExitCode, exitOK, o.Total, o.PassedTests, o.FailedTests, o.SkippedTests, rateOK, o.DurationSeconds)
}

// writeSummaryJSON emits the machine-readable summary.json (DESIGN §7.4). Field
// names mirror the design's example document. `pass_rate` is shared with the
// pass verdict via computePassRate (-1 when no format was parsed, matching the
// -1 counters). It RETURNS an error on marshal/write failure so the caller can
// treat a missing required artifact as an op failure instead of silently
// succeeding.
func writeSummaryJSON(dest string, tr *spec.TestRun, o *TestOutcome, started time.Time) error {
	passRate := computePassRate(tr.Results.Format, logs.Counters{
		Total: o.Total, Passed: o.PassedTests, Failed: o.FailedTests, Skipped: o.SkippedTests,
	})
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
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal summary.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "summary.json"), b, 0o644); err != nil {
		return fmt.Errorf("write summary.json: %w", err)
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
