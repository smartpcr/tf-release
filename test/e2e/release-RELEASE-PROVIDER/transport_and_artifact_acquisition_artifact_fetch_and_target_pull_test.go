//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 2.4 (Artifact
// Fetch and Target Pull).
//
// Both scenarios exercise the REAL internal/artifact package. No external
// service is required (setup: inline):
//
//   - Fetch sha verify and 404 -> stands up an in-process net/http/httptest
//     server (proof: service:httptest, bundled stdlib, no docker) serving a
//     deterministic zip on a good route and a 404 on a missing route. Fetch
//     against the good route must reproduce the payload sha256 and spool the
//     bytes to a runner temp file; Fetch against the 404 route must fail closed
//     with an ERR_ARTIFACT_FETCH coded error whose message carries the 404
//     status (DESIGN ART-01/ART-03, §8.2). The checksum-mismatch fail-closed
//     path (ART-02, ERR_CHECKSUM_MISMATCH with the runner temp file deleted,
//     DESIGN §18.3 / §8.2) is not exercised here; it is covered by the sibling
//     unit test internal/artifact/fetch_test.go.
//   - NuGet URL and target-pull script -> builds the flat-container download
//     URL and the Windows/Linux target-pull scripts for a nuget_feed source and
//     asserts they reproduce the committed goldens under
//     internal/artifact/testdata (proof: golden; DESIGN §6.3, §8.3).
//
// Every Given/When/Then invokes the real artifact functions and asserts on the
// result.
package e2e

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/artifact"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// artifactCodeOf extracts the code from an *artifact.CodedError, if present.
func artifactCodeOf(err error) string {
	var ce *artifact.CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// artifactModuleRoot walks up from this source file to the module root so the
// golden-fixture scenario can read internal/artifact/testdata regardless of the
// working directory go test chooses.
func artifactModuleRoot() (string, error) {
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

func artifactReadGolden(root, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "internal", "artifact", "testdata", name))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n"), nil
}

// artifactSampleZip builds a deterministic zip payload and returns its bytes
// plus the lowercase sha256 hex.
func artifactSampleZip() ([]byte, string, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("hello.txt")
	if err != nil {
		return nil, "", err
	}
	if _, err := w.Write([]byte("hello labdeploy e2e")); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	b := buf.Bytes()
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// artifactNugetFixture mirrors the deterministic artifact behind the committed
// goldens (internal/artifact/testdata).
func artifactNugetFixture() *spec.Artifact {
	return &spec.Artifact{
		Type:     spec.ArtifactNupkg,
		Version:  "1.2.3",
		Checksum: "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		Source: spec.Source{
			Type:      "nuget_feed",
			FeedURL:   "https://pkgs.example.com/v3",
			PackageID: "MyCompany.App",
			Auth:      spec.SourceAuth{TokenEnv: "LD_NUGET_TOKEN", Scheme: "bearer"},
		},
	}
}

const (
	artifactGoldenStagingWin   = `C:\staging\pkg.zip`
	artifactGoldenStagingLinux = "/opt/staging/pkg.zip"
)

// artifactWorld carries state across the steps of a single scenario.
type artifactWorld struct {
	srv     *httptest.Server
	zipBody []byte
	goodHex string

	fetched   *artifact.Fetched
	fetchErr  error

	nuget   *spec.Artifact
	url     string
	winScr  string
	linScr  string
}

func (w *artifactWorld) cleanup() {
	if w.srv != nil {
		w.srv.Close()
		w.srv = nil
	}
	if w.fetched != nil {
		_ = os.Remove(w.fetched.LocalPath)
		w.fetched = nil
	}
}

// --- Fetch sha verify and 404 ----------------------------------------------

func (w *artifactWorld) httptestServerGoodAnd404() error {
	body, goodHex, err := artifactSampleZip()
	if err != nil {
		return err
	}
	w.zipBody = body
	w.goodHex = goodHex

	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.zip", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/zip")
		_, _ = rw.Write(body)
	})
	mux.HandleFunc("/missing.zip", func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte("not found"))
	})
	w.srv = httptest.NewServer(mux)
	return nil
}

func (w *artifactWorld) fetchGoodRoute() error {
	a := &spec.Artifact{
		Type:     spec.ArtifactZip,
		Version:  "1.0.0",
		Checksum: "sha256:" + w.goodHex,
		Source:   spec.Source{Type: "http", URL: w.srv.URL + "/pkg.zip"},
	}
	w.fetched, w.fetchErr = artifact.Fetch(context.Background(), a)
	return nil
}

func (w *artifactWorld) goodShaMatchesAndSpooled() error {
	if w.fetchErr != nil {
		return fmt.Errorf("Fetch(good) unexpected error: %w", w.fetchErr)
	}
	if w.fetched == nil {
		return fmt.Errorf("Fetch(good) returned nil result")
	}
	if w.fetched.Sha256 != w.goodHex {
		return fmt.Errorf("sha = %s, want %s", w.fetched.Sha256, w.goodHex)
	}
	if w.fetched.Size != int64(len(w.zipBody)) {
		return fmt.Errorf("size = %d, want %d", w.fetched.Size, len(w.zipBody))
	}
	if _, err := os.Stat(w.fetched.LocalPath); err != nil {
		return fmt.Errorf("temp file not present: %w", err)
	}
	return nil
}

func (w *artifactWorld) fetch404Route() error {
	a := &spec.Artifact{
		Type:     spec.ArtifactZip,
		Version:  "1.0.0",
		Checksum: "sha256:" + w.goodHex,
		Source:   spec.Source{Type: "http", URL: w.srv.URL + "/missing.zip"},
	}
	res, err := artifact.Fetch(context.Background(), a)
	if res != nil {
		_ = os.Remove(res.LocalPath)
	}
	w.fetchErr = err
	return nil
}

func (w *artifactWorld) failsArtifactFetchWith404() error {
	if w.fetchErr == nil {
		return fmt.Errorf("expected an error from the 404 route, got nil")
	}
	if c := artifactCodeOf(w.fetchErr); c != "ERR_ARTIFACT_FETCH" {
		return fmt.Errorf("code = %q, want ERR_ARTIFACT_FETCH", c)
	}
	if !strings.Contains(w.fetchErr.Error(), "404") {
		return fmt.Errorf("error missing status 404: %v", w.fetchErr)
	}
	return nil
}

// --- NuGet URL and target-pull script --------------------------------------

func (w *artifactWorld) nugetSourceArtifact() error {
	w.nuget = artifactNugetFixture()
	return nil
}

func (w *artifactWorld) buildURLAndScripts() error {
	url, err := artifact.ResolveURL(w.nuget)
	if err != nil {
		return fmt.Errorf("ResolveURL: %w", err)
	}
	winScr, _, err := artifact.TargetPullScriptWindows(w.nuget, artifactGoldenStagingWin)
	if err != nil {
		return fmt.Errorf("TargetPullScriptWindows: %w", err)
	}
	linScr, _, err := artifact.TargetPullScriptLinux(w.nuget, artifactGoldenStagingLinux)
	if err != nil {
		return fmt.Errorf("TargetPullScriptLinux: %w", err)
	}
	w.url, w.winScr, w.linScr = url, winScr, linScr
	return nil
}

func (w *artifactWorld) urlAndScriptsMatchGolden() error {
	wantURL := "https://pkgs.example.com/v3/flatcontainer/mycompany.app/1.2.3/mycompany.app.1.2.3.nupkg"
	if w.url != wantURL {
		return fmt.Errorf("nuget URL:\n got %s\nwant %s", w.url, wantURL)
	}
	root, err := artifactModuleRoot()
	if err != nil {
		return err
	}
	goldURL, err := artifactReadGolden(root, "nuget_url.txt")
	if err != nil {
		return fmt.Errorf("read nuget_url golden: %w", err)
	}
	if w.url != goldURL {
		return fmt.Errorf("nuget_url golden drift:\n got %q\nwant %q", w.url, goldURL)
	}
	goldWin, err := artifactReadGolden(root, "target_pull_win.ps1")
	if err != nil {
		return fmt.Errorf("read windows golden: %w", err)
	}
	if w.winScr != goldWin {
		return fmt.Errorf("windows script golden drift:\n---got---\n%s\n---want---\n%s", w.winScr, goldWin)
	}
	goldLin, err := artifactReadGolden(root, "target_pull_linux.sh")
	if err != nil {
		return fmt.Errorf("read linux golden: %w", err)
	}
	if w.linScr != goldLin {
		return fmt.Errorf("linux script golden drift:\n---got---\n%s\n---want---\n%s", w.linScr, goldLin)
	}
	return nil
}

// InitializeScenario_transport_and_artifact_acquisition_artifact_fetch_and_target_pull
// registers the step definitions for the Stage 2.4 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_transport_and_artifact_acquisition_artifact_fetch_and_target_pull(ctx *godog.ScenarioContext) {
	w := &artifactWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		w.cleanup()
		*w = artifactWorld{}
		return c, nil
	})
	ctx.After(func(c context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.cleanup()
		return c, nil
	})

	ctx.Step(`^an httptest server serving a zip with a correct sha256 and a 404 route$`, w.httptestServerGoodAnd404)
	ctx.Step(`^Fetch runs against the good route$`, w.fetchGoodRoute)
	ctx.Step(`^the fetched sha256 matches and the artifact is spooled to a temp file$`, w.goodShaMatchesAndSpooled)
	ctx.Step(`^Fetch runs against the 404 route$`, w.fetch404Route)
	ctx.Step(`^it fails with ERR_ARTIFACT_FETCH including 404$`, w.failsArtifactFetchWith404)

	ctx.Step(`^a nuget_feed source artifact$`, w.nugetSourceArtifact)
	ctx.Step(`^the download URL is built and the target-pull scripts are generated$`, w.buildURLAndScripts)
	ctx.Step(`^the URL matches the flat-container form and the scripts match the committed golden$`, w.urlAndScriptsMatchGolden)
}

// TestE2E_transport_and_artifact_acquisition_artifact_fetch_and_target_pull is
// the go test entrypoint for the Stage 2.4 godog suite.
func TestE2E_transport_and_artifact_acquisition_artifact_fetch_and_target_pull(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_transport_and_artifact_acquisition_artifact_fetch_and_target_pull,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"transport_and_artifact_acquisition_artifact_fetch_and_target_pull.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
