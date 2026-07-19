//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 8.3 (GoReleaser
// Packaging and Distribution).
//
// Both scenarios exercise the REAL packaging impl in-process against the
// committed .goreleaser.yml and the committed tools/zipbin build hook — the
// only dependency is the gate host's Go toolchain (proof: service:go-toolchain =
// Go compiler + goreleaser producing the inspected binaries). No docker and no
// pre-provisioned service is required.
//
//   - Build matrix -> runs the REAL `goreleaser build --snapshot --clean` on
//     the gate host (the go-toolchain proof provides goreleaser), which builds
//     every goos/goarch in the committed config and fires its post-build hook
//     (`go run ./tools/zipbin …`). Asserts that a
//     `terraform-provider-labdeploy_v<version>_<os>_<arch>.zip` is produced for
//     both linux_amd64 and windows_amd64, matching the archive name_template,
//     and that each zip contains the provider binary named per the build's
//     `binary:` template (with the `.exe` suffix GoReleaser adds for windows).
//     Only when the `goreleaser` binary is genuinely absent from the host does
//     it fall back to faithfully replicating that build from the parsed config
//     plus the same real hook, so the suite still proves packaging off a
//     goreleaser-less dev box; the same assertions scan the dist/ output.
//   - No cgo -> reads each emitted binary straight out of the produced zip with
//     debug/buildinfo and asserts the recorded CGO_ENABLED build setting is "0"
//     (a statically linked, cgo-free build, DESIGN §20).
//
// Every Given/When/Then invokes the real committed config + hook and asserts on
// the artifacts they produce.
package e2e

import (
	"archive/zip"
	"context"
	"debug/buildinfo"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"
	"gopkg.in/yaml.v3"
)

// grBuildVersion is the deterministic snapshot version stamped into the matrix
// build when this suite drives the config directly (mirrors goreleaser's
// `--snapshot` version, but fixed so the emitted artifact names are stable).
const grBuildVersion = "0.0.0-e2e"

// --- typed view of the committed .goreleaser.yml (assert real config nodes) ---

type grConfig struct {
	Builds   []grBuild   `yaml:"builds"`
	Archives []grArchive `yaml:"archives"`
}

type grBuild struct {
	ID      string   `yaml:"id"`
	Binary  string   `yaml:"binary"`
	Main    string   `yaml:"main"`
	Env     []string `yaml:"env"`
	Flags   []string `yaml:"flags"`
	Ldflags []string `yaml:"ldflags"`
	Goos    []string `yaml:"goos"`
	Goarch  []string `yaml:"goarch"`
	Hooks   grHooks  `yaml:"hooks"`
}

type grHooks struct {
	Post []grHook `yaml:"post"`
}

type grHook struct {
	Cmd string `yaml:"cmd"`
}

type grArchive struct {
	NameTemplate string `yaml:"name_template"`
}

// grBuildResult caches the one-time packaging build shared across both
// scenarios so scenario 2 inspects the exact binaries scenario 1 produced.
type grBuildResult struct {
	cfg     grConfig
	build   grBuild
	distDir string
	// zips maps the "<os>_<arch>" target token to the produced zip path.
	zips           map[string]string
	usedGoreleaser bool
	err            error
}

var (
	grOnce   sync.Once
	grResult grBuildResult
)

// grRepoRoot walks up from the test package dir to the module root (nearest
// go.mod) so the committed .goreleaser.yml / tools/zipbin are located
// regardless of the working directory `go test` runs from.
func grRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// grRender resolves the goreleaser text/template tokens this suite uses in the
// build's binary/hook templates against the given target.
func grRender(tmpl, version, goos, goarch, path string) string {
	r := strings.NewReplacer(
		"{{ .Version }}", version, "{{.Version}}", version,
		"{{ .Os }}", goos, "{{.Os}}", goos,
		"{{ .Arch }}", goarch, "{{.Arch}}", goarch,
		"{{ .Path }}", path, "{{.Path}}", path,
	)
	return r.Replace(tmpl)
}

// grSplit splits a hook command line into argv, honoring double quotes so the
// committed `go run ./tools/zipbin "…" "…"` hook parses correctly.
func grSplit(s string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// grEnsureBuilt runs the packaging build exactly once and caches the result.
func grEnsureBuilt() *grBuildResult {
	grOnce.Do(func() {
		grResult = grRunBuild()
	})
	return &grResult
}

func grRunBuild() grBuildResult {
	res := grBuildResult{zips: map[string]string{}}

	root, err := grRepoRoot()
	if err != nil {
		res.err = err
		return res
	}

	raw, err := os.ReadFile(filepath.Join(root, ".goreleaser.yml"))
	if err != nil {
		res.err = fmt.Errorf("read .goreleaser.yml: %w", err)
		return res
	}
	if err := yaml.Unmarshal(raw, &res.cfg); err != nil {
		res.err = fmt.Errorf(".goreleaser.yml not well-formed YAML: %w", err)
		return res
	}
	if len(res.cfg.Builds) == 0 {
		res.err = fmt.Errorf(".goreleaser.yml: no builds defined")
		return res
	}
	res.build = res.cfg.Builds[0]

	// Run the REAL goreleaser build --snapshot on the gate host (the
	// go-toolchain proof provisions goreleaser). Only when the binary is
	// genuinely absent do we fall back to faithfully replicating the build the
	// committed config declares, so a goreleaser-less dev box still proves
	// packaging. `FORGE_NO_GORELEASER=1` forces the replication path (used by
	// unit-level debugging), but the default gate path is the real tool.
	if os.Getenv("FORGE_NO_GORELEASER") != "1" {
		if _, lookErr := exec.LookPath("goreleaser"); lookErr == nil {
			dist, gErr := grRealGoreleaser(root)
			if gErr != nil {
				res.err = gErr
				return res
			}
			res.distDir = dist
			res.usedGoreleaser = true
			grScanZips(&res)
			return res
		}
	}

	res.distDir, res.err = grReplicateBuild(root, res.build)
	if res.err != nil {
		return res
	}
	grScanZips(&res)
	return res
}

// grReplicateBuild executes, for every goos/goarch in the build matrix, the go
// build the config declares (with its exact env/flags/ldflags) and then runs
// the config's real post-build hook (tools/zipbin). It returns the dist dir the
// per-target zips were written into.
func grReplicateBuild(root string, b grBuild) (string, error) {
	dist, err := os.MkdirTemp("", "gr-e2e-dist-")
	if err != nil {
		return "", err
	}
	main := b.Main
	if main == "" {
		main = "."
	}
	for _, goos := range b.Goos {
		for _, goarch := range b.Goarch {
			targetDir := filepath.Join(dist, fmt.Sprintf("%s_%s_%s", b.ID, goos, goarch))
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return "", err
			}
			binName := grRender(b.Binary, grBuildVersion, goos, goarch, "")
			if goos == "windows" {
				binName += ".exe"
			}
			binPath := filepath.Join(targetDir, binName)

			args := []string{"build"}
			args = append(args, b.Flags...)
			if len(b.Ldflags) > 0 {
				ld := grRender(strings.Join(b.Ldflags, " "), grBuildVersion, goos, goarch, "")
				args = append(args, "-ldflags", ld)
			}
			args = append(args, "-o", binPath, main)

			cmd := exec.Command("go", args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch)
			cmd.Env = append(cmd.Env, b.Env...) // e.g. CGO_ENABLED=0
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("go build %s/%s failed: %v\n%s", goos, goarch, err, out)
			}

			// Run the config's real post-build hook (tools/zipbin), rendered
			// against this target, with the module root as its working dir.
			for _, h := range b.Hooks.Post {
				line := grRender(h.Cmd, grBuildVersion, goos, goarch, binPath)
				argv := grSplit(line)
				if len(argv) == 0 {
					continue
				}
				hook := exec.Command(argv[0], argv[1:]...)
				hook.Dir = root
				hook.Env = os.Environ()
				if out, err := hook.CombinedOutput(); err != nil {
					return "", fmt.Errorf("post hook %q failed: %v\n%s", line, err, out)
				}
			}
		}
	}
	return dist, nil
}

// grRealGoreleaser runs `goreleaser build --snapshot --clean` and returns the
// dist directory it wrote into.
func grRealGoreleaser(root string) (string, error) {
	cmd := exec.Command("goreleaser", "build", "--snapshot", "--clean")
	cmd.Dir = root
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("goreleaser build --snapshot: %v\n%s", err, out)
	}
	return filepath.Join(root, "dist"), nil
}

// grZipRe matches the filesystem-mirror artifact name for a given target.
var grZipRe = regexp.MustCompile(`terraform-provider-labdeploy_v.+_(linux|windows)_(amd64|arm64|386)\.zip$`)

// grScanZips walks the dist dir and records, per "<os>_<arch>" token, the
// produced zip path.
func grScanZips(res *grBuildResult) {
	_ = filepath.Walk(res.distDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		m := grZipRe.FindStringSubmatch(name)
		if m == nil {
			return nil
		}
		token := m[1] + "_" + m[2]
		if _, ok := res.zips[token]; !ok {
			res.zips[token] = p
		}
		return nil
	})
}

// grExtractBinary returns the provider-binary entry bytes from the zip, plus its
// entry name. The binary is the entry whose name carries the build's binary
// prefix (not README/LICENSE/CHANGELOG bundled files).
func grExtractBinary(zipPath, binPrefix string) (string, []byte, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", nil, fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, binPrefix) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return "", nil, err
		}
		return f.Name, data, nil
	}
	return "", nil, fmt.Errorf("zip %s: no entry with prefix %q", zipPath, binPrefix)
}

// --- world ------------------------------------------------------------------

type grWorld struct {
	res *grBuildResult
}

func (w *grWorld) committedGoreleaserConfig() error {
	root, err := grRepoRoot()
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(root, ".goreleaser.yml")
	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("committed goreleaser config missing: %w", err)
	}
	hookPath := filepath.Join(root, "tools", "zipbin", "main.go")
	if _, err := os.Stat(hookPath); err != nil {
		return fmt.Errorf("committed tools/zipbin hook missing: %w", err)
	}
	return nil
}

func (w *grWorld) snapshotBuildRuns() error {
	w.res = grEnsureBuilt()
	if w.res.err != nil {
		return w.res.err
	}
	// Sanity: the config must actually declare the linux+windows/amd64 matrix
	// and the cgo-free invariant this stage depends on.
	if !grContains(w.res.build.Goos, "linux") || !grContains(w.res.build.Goos, "windows") {
		return fmt.Errorf("goreleaser build goos must include linux and windows, got %v", w.res.build.Goos)
	}
	if !grContains(w.res.build.Goarch, "amd64") {
		return fmt.Errorf("goreleaser build goarch must include amd64, got %v", w.res.build.Goarch)
	}
	return nil
}

func (w *grWorld) builtProviderBinaries() error {
	// Scenario 2 reuses the exact artifacts produced by the packaging build.
	w.res = grEnsureBuilt()
	return w.res.err
}

func (w *grWorld) zipProducedFor(target string) error {
	if w.res == nil {
		w.res = grEnsureBuilt()
		if w.res.err != nil {
			return w.res.err
		}
	}
	zipPath, ok := w.res.zips[target]
	if !ok {
		return fmt.Errorf("no filesystem-mirror zip produced for %s (produced: %v)", target, grKeys(w.res.zips))
	}
	// Assert the archive name matches the config's name_template for this target.
	parts := strings.SplitN(target, "_", 2)
	goos, goarch := parts[0], parts[1]
	wantName := grExpectedZipName(w.res, goos, goarch)
	if filepath.Base(zipPath) != wantName {
		return fmt.Errorf("%s zip name = %q, want %q", target, filepath.Base(zipPath), wantName)
	}
	return nil
}

func (w *grWorld) eachZipNamedAndContainsBinary() error {
	if len(w.res.zips) == 0 {
		return fmt.Errorf("no zips were produced by the snapshot build")
	}
	for token, zipPath := range w.res.zips {
		parts := strings.SplitN(token, "_", 2)
		goos, goarch := parts[0], parts[1]
		binPrefix := grRender(w.res.build.Binary, grVersionFromZip(zipPath), goos, goarch, "")
		// GoReleaser appends the platform executable suffix; windows binaries
		// (both from the real tool and the faithful replication) carry `.exe`.
		if goos == "windows" {
			binPrefix += ".exe"
		}
		name, data, err := grExtractBinary(zipPath, "terraform-provider-labdeploy_v")
		if err != nil {
			return err
		}
		if name != binPrefix {
			return fmt.Errorf("%s: zip binary entry = %q, want %q", token, name, binPrefix)
		}
		if len(data) == 0 {
			return fmt.Errorf("%s: zip binary entry %q is empty", token, name)
		}
	}
	return nil
}

func (w *grWorld) eachBinaryInspected() error {
	if w.res == nil || w.res.err != nil {
		return fmt.Errorf("no built binaries to inspect")
	}
	if len(w.res.zips) == 0 {
		return fmt.Errorf("no built binaries to inspect")
	}
	return nil
}

func (w *grWorld) everyBinaryIsCgoFree() error {
	for token, zipPath := range w.res.zips {
		_, data, err := grExtractBinary(zipPath, "terraform-provider-labdeploy_v")
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp("", "gr-bin-*")
		if err != nil {
			return err
		}
		path := tmp.Name()
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			os.Remove(path)
			return err
		}
		tmp.Close()

		info, err := buildinfo.ReadFile(path)
		os.Remove(path)
		if err != nil {
			return fmt.Errorf("%s: read buildinfo: %w", token, err)
		}
		var cgo string
		var found bool
		for _, s := range info.Settings {
			if s.Key == "CGO_ENABLED" {
				cgo = s.Value
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: binary has no CGO_ENABLED build setting", token)
		}
		if cgo != "0" {
			return fmt.Errorf("%s: CGO_ENABLED = %q, want \"0\" (must be a static, cgo-free build)", token, cgo)
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

func grContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func grKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// grVersionFromZip extracts the version token from a produced zip filename,
// e.g. terraform-provider-labdeploy_v<version>_<os>_<arch>.zip.
var grVersionRe = regexp.MustCompile(`terraform-provider-labdeploy_v(.+)_(?:linux|windows)_(?:amd64|arm64|386)\.zip$`)

func grVersionFromZip(zipPath string) string {
	m := grVersionRe.FindStringSubmatch(filepath.Base(zipPath))
	if m == nil {
		return grBuildVersion
	}
	return m[1]
}

// grExpectedZipName renders the archive name_template (falling back to the
// documented filesystem-mirror pattern) for the given target.
func grExpectedZipName(res *grBuildResult, goos, goarch string) string {
	version := grBuildVersion
	// When the real zip was produced by a snapshot build, honor its version.
	if z, ok := res.zips[goos+"_"+goarch]; ok {
		version = grVersionFromZip(z)
	}
	tmpl := "terraform-provider-labdeploy_v{{ .Version }}_{{ .Os }}_{{ .Arch }}"
	if len(res.cfg.Archives) > 0 && res.cfg.Archives[0].NameTemplate != "" {
		tmpl = res.cfg.Archives[0].NameTemplate
	}
	return grRender(tmpl, version, goos, goarch, "") + ".zip"
}

// InitializeScenario_docker_examples_and_packaging_goreleaser_packaging_and_distribution
// registers the step definitions for the Stage 8.3 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_docker_examples_and_packaging_goreleaser_packaging_and_distribution(ctx *godog.ScenarioContext) {
	w := &grWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = grWorld{}
		return c, nil
	})

	// Scenario: Build matrix.
	ctx.Step(`^the committed goreleaser config$`, w.committedGoreleaserConfig)
	ctx.Step(`^the goreleaser snapshot build runs on the gate host$`, w.snapshotBuildRuns)
	ctx.Step(`^a zip is produced for "([^"]*)"$`, w.zipProducedFor)
	ctx.Step(`^each zip is named for its target and contains the provider binary$`, w.eachZipNamedAndContainsBinary)

	// Scenario: No cgo.
	ctx.Step(`^the built provider binaries$`, w.builtProviderBinaries)
	ctx.Step(`^each binary is inspected on the gate host$`, w.eachBinaryInspected)
	ctx.Step(`^every binary is statically built with CGO_ENABLED=0$`, w.everyBinaryIsCgoFree)
}

// TestE2E_docker_examples_and_packaging_goreleaser_packaging_and_distribution is
// the go test entrypoint for the Stage 8.3 godog suite.
func TestE2E_docker_examples_and_packaging_goreleaser_packaging_and_distribution(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_docker_examples_and_packaging_goreleaser_packaging_and_distribution,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"docker_examples_and_packaging_goreleaser_packaging_and_distribution.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
