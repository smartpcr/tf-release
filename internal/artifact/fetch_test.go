package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// -update regenerates the committed golden fixtures under testdata/ from the
// current implementation. Run: go test ./internal/artifact -run Golden -update
var update = flag.Bool("update", false, "update golden fixtures")

// sampleZip builds a tiny deterministic zip payload and returns bytes + its
// lowercase sha256 hex.
func sampleZip(t *testing.T) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("hello.txt")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte("hello labdeploy")); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	b := buf.Bytes()
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func codeOf(err error) string {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// T5 — Fetch sha verify and 404 (DESIGN ART-02/ART-03, §8.2).
func TestFetchShaVerifyAnd404(t *testing.T) {
	body, goodHex := sampleZip(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/missing.zip", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such package", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("good sha passes", func(t *testing.T) {
		a := &spec.Artifact{
			Type:     spec.ArtifactZip,
			Version:  "1.0.0",
			Checksum: "sha256:" + goodHex,
			Source:   spec.Source{Type: "http", URL: srv.URL + "/pkg.zip"},
		}
		got, err := Fetch(context.Background(), a)
		if err != nil {
			t.Fatalf("Fetch: unexpected error: %v", err)
		}
		defer os.Remove(got.LocalPath)
		if got.Sha256 != goodHex {
			t.Errorf("sha = %s, want %s", got.Sha256, goodHex)
		}
		if got.Size != int64(len(body)) {
			t.Errorf("size = %d, want %d", got.Size, len(body))
		}
		if _, err := os.Stat(got.LocalPath); err != nil {
			t.Errorf("temp file not present: %v", err)
		}
	})

	t.Run("404 yields ERR_ARTIFACT_FETCH with status", func(t *testing.T) {
		a := &spec.Artifact{
			Type:     spec.ArtifactZip,
			Version:  "1.0.0",
			Checksum: "sha256:" + goodHex,
			Source:   spec.Source{Type: "http", URL: srv.URL + "/missing.zip"},
		}
		_, err := Fetch(context.Background(), a)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if c := codeOf(err); c != "ERR_ARTIFACT_FETCH" {
			t.Errorf("code = %q, want ERR_ARTIFACT_FETCH", c)
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("error missing status 404: %v", err)
		}
	})

	t.Run("checksum mismatch deletes temp and codes", func(t *testing.T) {
		bad := strings.Repeat("0", 64)
		a := &spec.Artifact{
			Type:     spec.ArtifactZip,
			Version:  "1.0.0",
			Checksum: "sha256:" + bad,
			Source:   spec.Source{Type: "http", URL: srv.URL + "/pkg.zip"},
		}
		got, err := Fetch(context.Background(), a)
		if err == nil {
			if got != nil {
				os.Remove(got.LocalPath)
			}
			t.Fatal("expected checksum error, got nil")
		}
		if c := codeOf(err); c != "ERR_CHECKSUM_MISMATCH" {
			t.Errorf("code = %q, want ERR_CHECKSUM_MISMATCH", c)
		}
	})
}

// nugetFixture is the deterministic artifact behind the committed goldens.
func nugetFixture() *spec.Artifact {
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
	goldenStagingWin   = `C:\staging\pkg.zip`
	goldenStagingLinux = "/opt/staging/pkg.zip"
)

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func maybeWriteGolden(t *testing.T, name, content string) {
	t.Helper()
	if !*update {
		return
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(filepath.Join("testdata", name), []byte(content), 0o644); err != nil {
		t.Fatalf("write golden %s: %v", name, err)
	}
}

// NuGet URL + target-pull script match committed goldens (DESIGN §6.3, §8.3).
func TestNugetURLAndTargetPullGolden(t *testing.T) {
	a := nugetFixture()

	url, err := ResolveURL(a)
	if err != nil {
		t.Fatalf("ResolveURL: %v", err)
	}
	wantURL := "https://pkgs.example.com/v3/flatcontainer/mycompany.app/1.2.3/mycompany.app.1.2.3.nupkg"
	if url != wantURL {
		t.Errorf("nuget URL:\n got %s\nwant %s", url, wantURL)
	}

	winScript, winEnv, err := TargetPullScriptWindows(a, goldenStagingWin)
	if err != nil {
		t.Fatalf("TargetPullScriptWindows: %v", err)
	}
	linScript, linEnv, err := TargetPullScriptLinux(a, goldenStagingLinux)
	if err != nil {
		t.Fatalf("TargetPullScriptLinux: %v", err)
	}

	// Auth value must be injected via env, never inlined into the script.
	if _, ok := winEnv["LD_AUTH_VALUE"]; !ok {
		t.Error("windows env missing LD_AUTH_VALUE")
	}
	if _, ok := linEnv["LD_AUTH_VALUE"]; !ok {
		t.Error("linux env missing LD_AUTH_VALUE")
	}
	if strings.Contains(winScript, "Bearer") || strings.Contains(linScript, "Bearer") {
		t.Error("auth scheme/token leaked into generated script")
	}

	maybeWriteGolden(t, "nuget_url.txt", url)
	maybeWriteGolden(t, "target_pull_win.ps1", winScript)
	maybeWriteGolden(t, "target_pull_linux.sh", linScript)

	if got := readGolden(t, "nuget_url.txt"); got != url {
		t.Errorf("nuget_url golden drift:\n got %q\nwant %q", url, got)
	}
	if got := readGolden(t, "target_pull_win.ps1"); got != winScript {
		t.Errorf("windows script golden drift:\n---got---\n%s\n---want---\n%s", winScript, got)
	}
	if got := readGolden(t, "target_pull_linux.sh"); got != linScript {
		t.Errorf("linux script golden drift:\n---got---\n%s\n---want---\n%s", linScript, got)
	}
}

// file:// sources hash the local file without a network round-trip (§8.3).
func TestFetchFileSource(t *testing.T) {
	body, goodHex := sampleZip(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "pkg.zip")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	a := &spec.Artifact{
		Type:     spec.ArtifactZip,
		Version:  "1.0.0",
		Checksum: "sha256:" + goodHex,
		Source:   spec.Source{Type: "file", Path: p},
	}
	got, err := Fetch(context.Background(), a)
	if err != nil {
		t.Fatalf("Fetch(file): %v", err)
	}
	defer os.Remove(got.LocalPath)
	if got.Sha256 != goodHex {
		t.Errorf("sha = %s, want %s", got.Sha256, goodHex)
	}
}

// docker sources are passthrough: no runner fetch (§8.2, pull deferred to
// the docker pattern §9.6).
func TestDockerNotFetchedOnRunner(t *testing.T) {
	a := &spec.Artifact{
		Type:    spec.ArtifactDocker,
		Version: "1.0.0",
		Source:  spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Tag: "1.0.0"},
	}
	if UseTargetPull(a) {
		t.Error("docker should not use runner target-pull")
	}
	if _, err := Fetch(context.Background(), a); err == nil {
		t.Error("expected Fetch to refuse docker source on the runner")
	} else if c := codeOf(err); c != "ERR_ARTIFACT_FETCH" {
		t.Errorf("code = %q, want ERR_ARTIFACT_FETCH", c)
	}
}
