//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 3.1 (Path Model
// and Target Layout).
//
// Both scenarios exercise the REAL layout package
// (internal/layout.NewPaths + internal/layout.DirScript) in-process — no
// external service is required (setup: inline):
//
//   - Path derivation both OSes -> computes the release tree for a windows and a
//     linux install_root+app and asserts every derived path uses the correct
//     separator and matches the DESIGN §9.1 layout exactly (backslash on
//     windows, forward-slash on linux; releases/current/shared/shared\logs/
//     staging plus manifest.json and .lock).
//   - Layout script golden -> renders DirScript for windows and linux from a
//     ReleaseCtx and asserts the output is byte-identical to the committed
//     golden fixtures under internal/layout/testdata (proof: golden).
//
// Every Given/When/Then invokes the real layout code and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/pattern"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// buildReleaseCtx constructs a real pattern.ReleaseCtx the same way
// engine.releaseCtx does (DESIGN §9.1): derive layout.Paths from install_root +
// app + version, then merge builtin env UNDER the spec environment. This is the
// ReleaseCtx the golden scenario feeds to DirScript, so the golden proof is
// genuinely ReleaseCtx-driven rather than calling NewPaths directly.
func buildReleaseCtx(c layoutOSCase) pattern.ReleaseCtx {
	s := &spec.Deployment{
		Metadata: spec.Metadata{Name: c.app},
		Target:   spec.Target{OS: c.os},
		Artifact: spec.Artifact{Version: c.version},
		Pattern:  spec.Pattern{Type: spec.PatternConsoleApp, InstallRoot: c.installRoot},
	}
	p := layout.NewPaths(c.os, c.installRoot, c.app, c.version)
	env := layout.MergeEnv(layout.BuiltinEnv(s.Metadata.Name, s.Artifact.Version, p, 0), s.Environment)
	return pattern.ReleaseCtx{
		App:     s.Metadata.Name,
		Version: s.Artifact.Version,
		P:       p,
		Spec:    s,
		Env:     env,
	}
}

// layoutRepoRoot walks up from this test source file to the module root so the
// golden fixtures under internal/layout/testdata can be located regardless of
// the test's working directory.
func layoutRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// file = <root>/test/e2e/release-RELEASE-PROVIDER/<this>.go
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found above %s", file)
}

// layoutOSCase is one OS's declared inputs and expected DESIGN §9.1 layout.
type layoutOSCase struct {
	os          spec.OSKind
	installRoot string
	app         string
	version     string
	sep         string
	otherSep    string
	golden      string
	want        map[string]string
}

// layoutWorld carries state across the steps of a single scenario.
type layoutWorld struct {
	cases   []layoutOSCase
	paths   map[spec.OSKind]layout.Paths
	ctxs    map[spec.OSKind]pattern.ReleaseCtx
	scripts map[spec.OSKind]string
}

func (w *layoutWorld) reset() {
	w.cases = []layoutOSCase{
		{
			os:          spec.OSWindows,
			installRoot: `C:\deploy`,
			app:         "sample-svc",
			version:     "1.2.3",
			sep:         `\`,
			otherSep:    "/",
			golden:      "dir_windows.ps1",
			want: map[string]string{
				"Root":       `C:\deploy\sample-svc`,
				"Releases":   `C:\deploy\sample-svc\releases`,
				"Release":    `C:\deploy\sample-svc\releases\1.2.3`,
				"Current":    `C:\deploy\sample-svc\current`,
				"Shared":     `C:\deploy\sample-svc\shared`,
				"SharedLogs": `C:\deploy\sample-svc\shared\logs`,
				"Staging":    `C:\deploy\sample-svc\staging`,
				"Manifest":   `C:\deploy\sample-svc\manifest.json`,
				"Lock":       `C:\deploy\sample-svc\.lock`,
				"StagePkg":   `C:\deploy\sample-svc\staging\pkg.zip`,
			},
		},
		{
			os:          spec.OSLinux,
			installRoot: "/opt/deploy",
			app:         "svc",
			version:     "2.0.0",
			sep:         "/",
			otherSep:    `\`,
			golden:      "dir_linux.sh",
			want: map[string]string{
				"Root":       "/opt/deploy/svc",
				"Releases":   "/opt/deploy/svc/releases",
				"Release":    "/opt/deploy/svc/releases/2.0.0",
				"Current":    "/opt/deploy/svc/current",
				"Shared":     "/opt/deploy/svc/shared",
				"SharedLogs": "/opt/deploy/svc/shared/logs",
				"Staging":    "/opt/deploy/svc/staging",
				"Manifest":   "/opt/deploy/svc/manifest.json",
				"Lock":       "/opt/deploy/svc/.lock",
				"StagePkg":   "/opt/deploy/svc/staging/pkg.zip",
			},
		},
	}
	w.paths = map[spec.OSKind]layout.Paths{}
	w.ctxs = map[spec.OSKind]pattern.ReleaseCtx{}
	w.scripts = map[spec.OSKind]string{}
}

// --- Path derivation both OSes ----------------------------------------------

func (w *layoutWorld) installRootAndAppForBothOSes() error {
	if len(w.cases) != 2 {
		return fmt.Errorf("expected windows and linux cases, got %d", len(w.cases))
	}
	return nil
}

func (w *layoutWorld) pathsComputedForEachOS() error {
	for _, c := range w.cases {
		w.paths[c.os] = layout.NewPaths(c.os, c.installRoot, c.app, c.version)
	}
	return nil
}

func (w *layoutWorld) separatorsAndLayoutMatchDesign() error {
	for _, c := range w.cases {
		p, ok := w.paths[c.os]
		if !ok {
			return fmt.Errorf("no computed paths for %s", c.os)
		}
		got := map[string]string{
			"Root":       p.Root,
			"Releases":   p.Releases,
			"Release":    p.Release,
			"Current":    p.Current,
			"Shared":     p.Shared,
			"SharedLogs": p.SharedLogs,
			"Staging":    p.Staging,
			"Manifest":   p.Manifest,
			"Lock":       p.Lock,
			"StagePkg":   p.StagePkg,
		}
		for name, want := range c.want {
			if got[name] != want {
				return fmt.Errorf("%s.%s = %q, want %q", c.os, name, got[name], want)
			}
		}
		// Dirs() must be the §9.1 creation-order set (current is a
		// junction/symlink and manifest/.lock are files, so excluded).
		wantDirs := []string{p.Releases, p.Shared, p.SharedLogs, p.Staging}
		gotDirs := p.Dirs()
		if len(gotDirs) != len(wantDirs) {
			return fmt.Errorf("%s Dirs() = %v, want %v", c.os, gotDirs, wantDirs)
		}
		for i := range wantDirs {
			if gotDirs[i] != wantDirs[i] {
				return fmt.Errorf("%s Dirs()[%d] = %q, want %q", c.os, i, gotDirs[i], wantDirs[i])
			}
		}
		// Separator discipline: the correct separator is present and the
		// foreign separator never leaks (ignoring the windows drive colon).
		body := strings.TrimPrefix(p.Root, `C:`)
		if !strings.Contains(body, c.sep) {
			return fmt.Errorf("%s path %q missing separator %q", c.os, p.Root, c.sep)
		}
		if strings.Contains(body, c.otherSep) {
			return fmt.Errorf("%s path %q leaked foreign separator %q", c.os, p.Root, c.otherSep)
		}
	}
	return nil
}

// --- Layout script golden ---------------------------------------------------

func (w *layoutWorld) releaseCtxForBothOSes() error {
	if len(w.cases) != 2 {
		return fmt.Errorf("expected windows and linux cases, got %d", len(w.cases))
	}
	for _, c := range w.cases {
		rc := buildReleaseCtx(c)
		// The ReleaseCtx must carry the DESIGN §9.1 layout it was derived from.
		if rc.App != c.app || rc.Version != c.version {
			return fmt.Errorf("%s ReleaseCtx app/version = %q/%q, want %q/%q",
				c.os, rc.App, rc.Version, c.app, c.version)
		}
		if rc.P.OS != c.os {
			return fmt.Errorf("%s ReleaseCtx.P.OS = %q, want %q", c.os, rc.P.OS, c.os)
		}
		if want := c.want["Root"]; rc.P.Root != want {
			return fmt.Errorf("%s ReleaseCtx.P.Root = %q, want %q", c.os, rc.P.Root, want)
		}
		w.ctxs[c.os] = rc
	}
	return nil
}

func (w *layoutWorld) dirScriptGeneratedForEachOS() error {
	for _, c := range w.cases {
		rc, ok := w.ctxs[c.os]
		if !ok {
			return fmt.Errorf("no ReleaseCtx for %s (Given step did not run)", c.os)
		}
		// Generate from the ReleaseCtx's carried layout, exactly as the engine
		// does when it renders the dir-creation script for a release.
		w.scripts[c.os] = layout.DirScript(rc.P)
	}
	return nil
}

func (w *layoutWorld) matchesCommittedGolden() error {
	root, err := layoutRepoRoot()
	if err != nil {
		return err
	}
	for _, c := range w.cases {
		got, ok := w.scripts[c.os]
		if !ok {
			return fmt.Errorf("no generated script for %s", c.os)
		}
		path := filepath.Join(root, "internal", "layout", "testdata", c.golden)
		want, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read golden %s: %w", path, err)
		}
		if got != string(want) {
			return fmt.Errorf("DirScript for %s mismatch.\n got: %q\nwant: %q", c.os, got, string(want))
		}
	}
	return nil
}

// InitializeScenario_engine_core_and_manifest_state_path_model_and_target_layout
// registers the step definitions for the Stage 3.1 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_engine_core_and_manifest_state_path_model_and_target_layout(ctx *godog.ScenarioContext) {
	w := &layoutWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		w.reset()
		return c, nil
	})

	ctx.Step(`^install_root and app name for windows and linux$`, w.installRootAndAppForBothOSes)
	ctx.Step(`^the paths are computed for each OS$`, w.pathsComputedForEachOS)
	ctx.Step(`^the separators and layout match DESIGN section 9\.1 exactly$`, w.separatorsAndLayoutMatchDesign)

	ctx.Step(`^a ReleaseCtx for windows and linux$`, w.releaseCtxForBothOSes)
	ctx.Step(`^the dir-creation script is generated for each OS$`, w.dirScriptGeneratedForEachOS)
	ctx.Step(`^it matches the committed golden fixture for each OS$`, w.matchesCommittedGolden)
}

// TestE2E_engine_core_and_manifest_state_path_model_and_target_layout is the go
// test entrypoint for the Stage 3.1 godog suite.
func TestE2E_engine_core_and_manifest_state_path_model_and_target_layout(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_engine_core_and_manifest_state_path_model_and_target_layout,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"engine_core_and_manifest_state_path_model_and_target_layout.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
