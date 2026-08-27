//go:build e2e

package engine

// This file is compiled ONLY under `-tags e2e`. It exposes narrow, behaviour-
// preserving wrappers around the (unexported) staging-extraction, junction-
// switch, and prune internals so the out-of-package godog acceptance suite
// under test/e2e can drive the REAL Stage 3.3 code (DESIGN §9.1, §10.1 step 13)
// without duplicating it. Production builds never see these symbols.

import (
	"context"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/logs"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// RunnerCommand renders the real expanded runner command line for a TestRun's
// runner.type, including the results-dir logger flags (DESIGN §7.2). Used by the
// Stage 7.1 "Runner expansion golden" e2e.
func RunnerCommand(t *spec.TestRun) string { return runnerCommand(t) }

// EvaluatePass applies the real pass_criteria evaluation (DESIGN §7.3): the
// runner exit code must be allowed AND — only when a format was parsed and
// total>0 — the pass rate must meet min_pass_rate. Used by the Stage 7.1 "Pass
// criteria evaluation" e2e.
func EvaluatePass(exitCode int, format string, c logs.Counters, pc *spec.PassCriteria) (passed, exitOK, rateOK bool) {
	return evaluatePass(exitCode, format, c, pc)
}

// NoResultCounters returns the -1 sentinel counter set emitted when
// results.format is none|empty (DESIGN §5.3; E2E-07). Used by the Stage 7.1 e2e.
func NoResultCounters() logs.Counters { return noResultCounters() }

// BuildProbeCmd renders the real single-attempt health-probe command for the
// given target OS and health_check config, encoding the expect_status /
// expect_body_regex logic (DESIGN §6.5). Used by the Stage 3.4 golden e2e.
func BuildProbeCmd(os spec.OSKind, hc *spec.HealthCheck, workDir string, env map[string]string) transport.Cmd {
	return buildProbeCmd(os, hc, workDir, env)
}

// ExtractScript renders the real staging→release extraction script for p.OS.
func ExtractScript(p layout.Paths) string { return extractScript(p) }

// SwitchScript renders the real `current` junction/symlink repoint script for
// p.OS, including the exit-42 (ERR_SWITCH) guard.
func SwitchScript(p layout.Paths) string { return switchScript(p) }

// PruneReleases drives the real retention prune against a (fake) transport,
// keeping the newest keep_releases dirs plus the always-protected
// current/previous versions (DESIGN §10.1 step 13).
func (e *Engine) PruneReleases(ctx context.Context, t transport.Transport, s *spec.Deployment,
	p layout.Paths, m *Manifest) error {
	return e.pruneReleases(ctx, t, s, p, m)
}
