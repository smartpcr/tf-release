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
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

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
