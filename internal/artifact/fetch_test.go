package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

// isolateTempDir points os.CreateTemp("") at a fresh empty dir for the duration
// of a subtest, so we can prove the runner temp file is deleted (nothing leaks).
// Go's os.TempDir consults TMPDIR (unix) and TMP/TEMP (windows); set all three.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func countTempPkgs(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "labdeploy-") {
			n++
		}
	}
	return n
}

// T5 — Fetch sha verify, header auth injection, and 404 (DESIGN ART-02/ART-03, §8.2).
func TestFetchShaVerifyAnd404(t *testing.T) {
	body, goodHex := sampleZip(t)

	// Large 404 body to prove the diagnostic truncates to the first 256 bytes:
	// 256 'A' (kept) followed by 300 'B' (must be dropped).
	notFoundBody := strings.Repeat("A", 256) + strings.Repeat("B", 300)

	const wantAuth = "Bearer s3cr3t-token"
	var gotAuth string
	var authHits int

	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.zip", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		authHits++
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/missing.zip", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(notFoundBody))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("good sha passes and injects auth header", func(t *testing.T) {
		t.Setenv("LD_HTTP_TOKEN", "s3cr3t-token")
		a := &spec.Artifact{
			Type:     spec.ArtifactZip,
			Version:  "1.0.0",
			Checksum: "sha256:" + goodHex,
			Source: spec.Source{
				Type: "http",
				URL:  srv.URL + "/pkg.zip",
				Auth: spec.SourceAuth{TokenEnv: "LD_HTTP_TOKEN"},
			},
		}
		gotAuth, authHits = "", 0
		got, err := Fetch(context.Background(), a)
		if err != nil {
			t.Fatalf("Fetch: unexpected error: %v", err)
		}
		defer os.Remove(got.LocalPath)
		if authHits != 1 {
			t.Fatalf("server saw %d requests, want 1", authHits)
		}
		if gotAuth != wantAuth {
			t.Errorf("server received Authorization = %q, want %q", gotAuth, wantAuth)
		}
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

	t.Run("404 yields ERR_ARTIFACT_FETCH with status and 256B body", func(t *testing.T) {
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
		msg := err.Error()
		if !strings.Contains(msg, "404") {
			t.Errorf("error missing status 404: %v", err)
		}
		// First 256 bytes ('A'×256) must be present as a contiguous run...
		if !strings.Contains(msg, strings.Repeat("A", 256)) {
			t.Errorf("error missing first 256 body bytes: %v", err)
		}
		// ...but not a 257th body byte, and nothing from the 'B' run past byte 256.
		if strings.Contains(msg, strings.Repeat("A", 257)) {
			t.Errorf("error echoed more than 256 body bytes: %v", err)
		}
		if strings.Contains(msg, "B") {
			t.Errorf("error included body past 256 bytes: %v", err)
		}
	})

	t.Run("checksum mismatch codes and deletes runner temp file", func(t *testing.T) {
		dir := isolateTempDir(t)
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
		if n := countTempPkgs(t, dir); n != 0 {
			t.Errorf("temp file leaked after mismatch: %d labdeploy-* file(s) remain in %s", n, dir)
		}
	})
}

// AuthHeader resolves exact bearer/basic values from auth.token_env (DESIGN §6.3):
// nuget bearer -> "Bearer <tok>"; nuget default -> Basic base64("pat:<tok>").
func TestNugetAuthHeaderValues(t *testing.T) {
	const tok = "s3cr3t-pat"
	t.Setenv("LD_NUGET_TOKEN", tok)

	t.Run("bearer scheme", func(t *testing.T) {
		a := nugetFixture() // scheme: bearer, token_env: LD_NUGET_TOKEN
		name, value := AuthHeader(a)
		if name != "Authorization" {
			t.Errorf("header name = %q, want Authorization", name)
		}
		if want := "Bearer " + tok; value != want {
			t.Errorf("bearer value = %q, want %q", value, want)
		}
	})

	t.Run("default basic scheme uses pat user", func(t *testing.T) {
		a := nugetFixture()
		a.Source.Auth.Scheme = "" // default -> basic, user=pat
		name, value := AuthHeader(a)
		if name != "Authorization" {
			t.Errorf("header name = %q, want Authorization", name)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pat:"+tok))
		if value != want {
			t.Errorf("basic value = %q, want %q", value, want)
		}
	})

	t.Run("explicit basic with username", func(t *testing.T) {
		a := nugetFixture()
		a.Source.Auth.Scheme = "basic"
		a.Source.Auth.Username = "svc"
		_, value := AuthHeader(a)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc:"+tok))
		if value != want {
			t.Errorf("basic value = %q, want %q", value, want)
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
	// Set a real token so the secret-hygiene assertions below have an actual
	// secret value to hunt for. The generated scripts reference only
	// $env:LD_AUTH_VALUE and never embed the token, so the committed goldens
	// stay stable regardless of this value (DESIGN §11, §8.3).
	t.Setenv("LD_NUGET_TOKEN", "sentinel-secret")
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

	// Auth value must be injected via env, never inlined into the script. The
	// env map must carry the exact "Bearer <token>" value, while the scripts
	// reference only $env:LD_AUTH_VALUE — so neither the secret token value nor
	// the scheme word may appear in the generated script text (DESIGN §11, §8.3).
	const wantAuthValue = "Bearer sentinel-secret"
	if got := winEnv["LD_AUTH_VALUE"]; got != wantAuthValue {
		t.Errorf("windows LD_AUTH_VALUE = %q, want %q", got, wantAuthValue)
	}
	if got := linEnv["LD_AUTH_VALUE"]; got != wantAuthValue {
		t.Errorf("linux LD_AUTH_VALUE = %q, want %q", got, wantAuthValue)
	}
	if strings.Contains(winScript, "sentinel-secret") || strings.Contains(linScript, "sentinel-secret") {
		t.Error("secret token value leaked into generated script")
	}
	if strings.Contains(winScript, "Bearer") || strings.Contains(linScript, "Bearer") {
		t.Error("auth scheme leaked into generated script")
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

// A docker_image never reaches runner-side Fetch in practice: spec validation
// pins docker_image to the docker_container pattern, and the engine
// short-circuits that pattern to deployDocker *before* the staging/fetch path,
// so the image is pulled on the target by the §9.6 D-steps. §8.2's "no fetch on
// runner; returns ref string" and its bad-creds/pull-failure ERR_ARTIFACT_FETCH
// therefore describe the docker *pattern* (§9.6), not this function — so this
// test does NOT cite §8.2 to justify a runner-side refusal.
//
// What it pins is the defensive guard in Fetch's default case: should a future
// caller ever route a docker source here, Fetch must fail closed with
// ERR_ARTIFACT_FETCH rather than silently return an empty/unhashed Fetched.
// This is guard behavior, not §8.2 spec behavior.
func TestDockerNotFetchedOnRunner(t *testing.T) {
	a := &spec.Artifact{
		Type:    spec.ArtifactDocker,
		Version: "1.0.0",
		Source:  spec.Source{Type: "docker_registry", Image: "registry.example.com/app", Tag: "1.0.0"},
	}
	// docker defaults to runner_push (UseTargetPull=false, §8.3); the guard below
	// is the backstop that stops the runner_push branch from ever spooling a
	// docker source into a temp package.
	if UseTargetPull(a) {
		t.Error("docker should not use runner target-pull")
	}
	if _, err := Fetch(context.Background(), a); err == nil {
		t.Error("defensive guard: Fetch must fail closed on a docker source, not return a package")
	} else if c := codeOf(err); c != "ERR_ARTIFACT_FETCH" {
		t.Errorf("code = %q, want ERR_ARTIFACT_FETCH", c)
	}
}

// UseTargetPull is the fetch-strategy gate for this workstream (DESIGN §8.3):
// target_pull is the *default* for http/nuget sources, an explicit fetch_mode
// overrides that default in either direction, and file:// is *always*
// runner_push. Every rule gets its own case — a regression that flipped the
// http/nuget default to false would silently fall back to runner_push, making
// the runner proxy large payloads (the exact thing §8.3 forbids) with no other
// failing test. The two override cases deliberately use a source whose default
// is the *opposite* of the override, so they prove fetch_mode actually wins.
func TestUseTargetPull(t *testing.T) {
	cases := []struct {
		name      string
		fetchMode string
		srcType   string
		artType   spec.ArtifactType
		want      bool
	}{
		// Defaults (no fetch_mode): http/nuget pull on the target, all else pushes.
		{"http default pulls", "", "http", spec.ArtifactZip, true},
		{"nuget default pulls", "", "nuget_feed", spec.ArtifactNupkg, true},
		{"file default pushes", "", "file", spec.ArtifactZip, false},
		{"docker default pushes", "", "docker_registry", spec.ArtifactDocker, false},
		// Explicit fetch_mode overrides the source default in both directions.
		{"runner_push overrides http default", "runner_push", "http", spec.ArtifactZip, false},
		{"target_pull overrides file default", "target_pull", "file", spec.ArtifactZip, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &spec.Artifact{
				Type:      tc.artType,
				Version:   "1.0.0",
				FetchMode: tc.fetchMode,
				Source:    spec.Source{Type: tc.srcType},
			}
			if got := UseTargetPull(a); got != tc.want {
				t.Errorf("UseTargetPull(fetch_mode=%q, source=%q) = %v, want %v",
					tc.fetchMode, tc.srcType, got, tc.want)
			}
		})
	}
}
