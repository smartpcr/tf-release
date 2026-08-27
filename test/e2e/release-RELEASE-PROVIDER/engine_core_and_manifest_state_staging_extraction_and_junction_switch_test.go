//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 3.3 (Staging
// Extraction and Junction Switch).
//
// Both scenarios exercise the REAL internal/engine code in-process — no
// external service is required (setup: inline):
//
//   - Switch and extract scripts golden -> derives layout.Paths from a windows
//     and a linux install_root+app+version (the ReleaseCtx layout, DESIGN §9.1)
//     and renders the REAL engine.ExtractScript / engine.SwitchScript for each
//     OS, asserting each is byte-identical to the committed golden fixtures
//     under internal/engine/testdata (extract_windows.ps1, extract_linux.sh,
//     switch_windows.ps1, switch_linux.sh). The switch goldens carry the
//     exit-42 (ERR_SWITCH) guard, so the golden proof also pins that guard
//     (proof: golden).
//   - Prune keeps previous -> drives the REAL engine.(*Engine).PruneReleases
//     against an in-process fake Transport with a scripted release listing and
//     keep_releases=2 plus a previous_version. The oldest non-protected
//     releases must be removed while current + previous are retained (DESIGN
//     §10.1 step 13, CAP-02).
//
// Every Given/When/Then invokes the real engine code and asserts on its result.
package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/layout"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// stagingModuleRoot walks up from this source file to the module root so the
// golden-fixture scenario can read internal/engine/testdata regardless of the
// working directory go test chooses.
func stagingModuleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 12; i++ {
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

func stagingReadGolden(root, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "internal", "engine", "testdata", name))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stagingOSCase is one OS's declared inputs and the goldens the engine must
// reproduce for it.
type stagingOSCase struct {
	os          spec.OSKind
	installRoot string
	app         string
	version     string
	extractGold string
	switchGold  string
}

func stagingCases() []stagingOSCase {
	return []stagingOSCase{
		{spec.OSWindows, `C:\deploy`, "sample-svc", "1.2.3", "extract_windows.ps1", "switch_windows.ps1"},
		{spec.OSLinux, "/opt/deploy", "svc", "1.2.3", "extract_linux.sh", "switch_linux.sh"},
	}
}

// stagingPruneHost is a minimal in-process fake Transport that emulates just
// enough of the Windows PowerShell surface engine.PruneReleases emits: the
// release directory listing (Get-ChildItem) and the recursive removals
// (Remove-Item ... SilentlyContinue). It records every removed path so the
// scenario can assert which releases were pruned vs retained.
type stagingPruneHost struct {
	host    string
	dirs    map[string]int64 // release dir name -> mtime ticks (newest = largest)
	removed []string         // full paths passed to Remove-Item, in order
}

func newStagingPruneHost(name string) *stagingPruneHost {
	return &stagingPruneHost{host: name, dirs: map[string]int64{}}
}

func (f *stagingPruneHost) Connect(ctx context.Context) error { return nil }
func (f *stagingPruneHost) Close() error                      { return nil }
func (f *stagingPruneHost) OS() spec.OSKind                   { return spec.OSWindows }
func (f *stagingPruneHost) Host() string                      { return f.host }

func (f *stagingPruneHost) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "Get-ChildItem -Directory"):
		// Deterministic listing "<name>|<ticks>" newest-first is not required;
		// the engine sorts by ticks itself.
		var names []string
		for name := range f.dirs {
			names = append(names, name)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, name := range names {
			fmt.Fprintf(&b, "%s|%d\n", name, f.dirs[name])
		}
		return transport.Result{ExitCode: 0, Stdout: b.String()}, nil

	case strings.Contains(s, "Remove-Item -Recurse -Force -ErrorAction SilentlyContinue"):
		// removePath: extract the single-quoted target path, record it, and
		// report success (the re-check Test-Path is satisfied because the dir
		// is now gone).
		if i := strings.Index(s, "'"); i >= 0 {
			rest := s[i+1:]
			if j := strings.Index(rest, "'"); j >= 0 {
				path := rest[:j]
				f.removed = append(f.removed, path)
				// Drop from the model so the removePath Test-Path re-check passes.
				base := path[strings.LastIndexAny(path, `\/`)+1:]
				delete(f.dirs, base)
			}
		}
		return transport.Result{ExitCode: 0}, nil
	}
	return transport.Result{ExitCode: 0}, nil
}

func (f *stagingPruneHost) Upload(ctx context.Context, r io.Reader, size int64, remote string) error {
	return nil
}

func (f *stagingPruneHost) Download(ctx context.Context, remote, local string) error {
	return fmt.Errorf("not needed in staging e2e")
}

var _ transport.Transport = (*stagingPruneHost)(nil)

// stagingWorld carries state across the steps of a single scenario.
type stagingWorld struct {
	// golden scenario
	cases       []stagingOSCase
	extractByOS map[spec.OSKind]string
	switchByOS  map[spec.OSKind]string

	// prune scenario
	host     *stagingPruneHost
	dep      *spec.Deployment
	paths    layout.Paths
	manifest *engine.Manifest
	pruneErr error
}

// --- Switch and extract scripts golden --------------------------------------

func (w *stagingWorld) releaseCtxForBothOSes() error {
	w.cases = stagingCases()
	w.extractByOS = map[spec.OSKind]string{}
	w.switchByOS = map[spec.OSKind]string{}
	// Validate the derived layout (the "ReleaseCtx" the scripts are rendered
	// from) is the DESIGN §9.1 tree before rendering.
	for _, c := range w.cases {
		p := layout.NewPaths(c.os, c.installRoot, c.app, c.version)
		if p.OS != c.os {
			return fmt.Errorf("%s: derived paths OS = %q, want %q", c.os, p.OS, c.os)
		}
		if p.Release == "" || p.Current == "" || p.StagePkg == "" {
			return fmt.Errorf("%s: incomplete layout: %+v", c.os, p)
		}
	}
	return nil
}

func (w *stagingWorld) generateScriptsForEachOS() error {
	if len(w.cases) != 2 {
		return fmt.Errorf("expected windows and linux cases, got %d (Given step did not run)", len(w.cases))
	}
	for _, c := range w.cases {
		p := layout.NewPaths(c.os, c.installRoot, c.app, c.version)
		w.extractByOS[c.os] = engine.ExtractScript(p)
		w.switchByOS[c.os] = engine.SwitchScript(p)
	}
	return nil
}

func (w *stagingWorld) scriptsMatchGoldenWithExit42() error {
	root, err := stagingModuleRoot()
	if err != nil {
		return err
	}
	sawExit42 := false
	for _, c := range w.cases {
		gotExtract, ok := w.extractByOS[c.os]
		if !ok {
			return fmt.Errorf("no extract script generated for %s", c.os)
		}
		gotSwitch, ok := w.switchByOS[c.os]
		if !ok {
			return fmt.Errorf("no switch script generated for %s", c.os)
		}
		wantExtract, err := stagingReadGolden(root, c.extractGold)
		if err != nil {
			return fmt.Errorf("read extract golden %s: %w", c.extractGold, err)
		}
		if gotExtract != wantExtract {
			return fmt.Errorf("extract script for %s golden drift:\n---got---\n%s\n---want---\n%s",
				c.os, gotExtract, wantExtract)
		}
		wantSwitch, err := stagingReadGolden(root, c.switchGold)
		if err != nil {
			return fmt.Errorf("read switch golden %s: %w", c.switchGold, err)
		}
		if gotSwitch != wantSwitch {
			return fmt.Errorf("switch script for %s golden drift:\n---got---\n%s\n---want---\n%s",
				c.os, gotSwitch, wantSwitch)
		}
		// The generated (== golden) switch script must carry the exit-42 guard.
		if !strings.Contains(gotSwitch, "42") {
			return fmt.Errorf("switch script for %s missing exit-42 ERR_SWITCH guard:\n%s", c.os, gotSwitch)
		}
		sawExit42 = true
	}
	if !sawExit42 {
		return fmt.Errorf("no switch script asserted the exit-42 guard")
	}
	return nil
}

// --- Prune keeps previous ---------------------------------------------------

func (w *stagingWorld) releasesSetWithKeep2AndPrevious() error {
	keep := 2
	w.dep = &spec.Deployment{
		Metadata: spec.Metadata{Name: "sample-svc"},
		Target:   spec.Target{OS: spec.OSWindows, Hosts: []string{"lab-01"}},
		Artifact: spec.Artifact{Version: "1.2.0"},
		Pattern:  spec.Pattern{Type: spec.PatternWindowsService, InstallRoot: `C:\deploy`},
		Strategy: spec.Strategy{KeepReleases: &keep},
	}
	if got := w.dep.Strategy.EffectiveKeepReleases(); got != 2 {
		return fmt.Errorf("keep_releases = %d, want 2", got)
	}
	w.paths = layout.NewPaths(spec.OSWindows, `C:\deploy`, "sample-svc", "1.2.0")
	w.host = newStagingPruneHost("lab-01")
	// keep_releases=2 is the TOTAL budget (DESIGN CAP-02). current (1.2.0) +
	// previous (1.0.0) are always protected and fill the whole budget, so BOTH
	// non-protected dirs (1.1.0, 0.9.0) must be pruned — even though 1.1.0 is
	// newer than previous (proving protection is by identity, not age).
	w.host.dirs = map[string]int64{"1.2.0": 400, "1.1.0": 300, "1.0.0": 200, "0.9.0": 100}
	w.manifest = &engine.Manifest{CurrentVersion: "1.2.0", PreviousVersion: "1.0.0"}
	return nil
}

func (w *stagingWorld) pruneRunsAgainstFakeTransport() error {
	if w.host == nil || w.dep == nil {
		return fmt.Errorf("prune preconditions not set (Given step did not run)")
	}
	eng := engine.New()
	w.pruneErr = eng.PruneReleases(context.Background(), w.host, w.dep, w.paths, w.manifest)
	return nil
}

func (w *stagingWorld) oldestRemovedPreviousRetained() error {
	if w.pruneErr != nil {
		return fmt.Errorf("prune returned error: %w", w.pruneErr)
	}
	joined := strings.Join(w.host.removed, ">")
	// Non-protected releases must be pruned.
	for _, del := range []string{"1.1.0", "0.9.0"} {
		want := `C:\deploy\sample-svc\releases\` + del
		if !strings.Contains(joined, want) {
			return fmt.Errorf("non-protected release %s must be pruned (keep=2 total); removed=%v",
				del, w.host.removed)
		}
	}
	// Protected current + previous must be retained.
	for _, keep := range []string{"1.2.0", "1.0.0"} {
		unwanted := `C:\deploy\sample-svc\releases\` + keep
		if strings.Contains(joined, unwanted) {
			return fmt.Errorf("protected release %s (current/previous) must be retained; removed=%v",
				keep, w.host.removed)
		}
	}
	return nil
}

// InitializeScenario_engine_core_and_manifest_state_staging_extraction_and_junction_switch
// registers the step definitions for the Stage 3.3 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_engine_core_and_manifest_state_staging_extraction_and_junction_switch(ctx *godog.ScenarioContext) {
	w := &stagingWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = stagingWorld{}
		return c, nil
	})

	ctx.Step(`^a ReleaseCtx for windows and linux$`, w.releaseCtxForBothOSes)
	ctx.Step(`^the extract and junction-switch scripts are generated for each OS$`, w.generateScriptsForEachOS)
	ctx.Step(`^they match the committed golden fixtures including the exit-42 guard$`, w.scriptsMatchGoldenWithExit42)

	ctx.Step(`^a releases set with keep_releases 2 and a previous_version$`, w.releasesSetWithKeep2AndPrevious)
	ctx.Step(`^prune runs against a fake transport$`, w.pruneRunsAgainstFakeTransport)
	ctx.Step(`^the oldest releases are removed and the previous_version is retained$`, w.oldestRemovedPreviousRetained)
}

// TestE2E_engine_core_and_manifest_state_staging_extraction_and_junction_switch
// is the go test entrypoint for the Stage 3.3 godog suite.
func TestE2E_engine_core_and_manifest_state_staging_extraction_and_junction_switch(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_engine_core_and_manifest_state_staging_extraction_and_junction_switch,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"engine_core_and_manifest_state_staging_extraction_and_junction_switch.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
