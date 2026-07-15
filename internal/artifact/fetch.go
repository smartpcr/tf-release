// Package artifact fetches/verifies deployable packages (DESIGN §8.2, §8.3).
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

type CodedError struct {
	Code string
	Err  error
}

func (e *CodedError) Error() string { return fmt.Sprintf("[%s] %v", e.Code, e.Err) }
func (e *CodedError) Unwrap() error { return e.Err }

type Fetched struct {
	LocalPath string // temp file on the runner (caller deletes)
	Sha256    string
	Size      int64
}

// ResolveURL computes the concrete download URL for http/nuget sources.
// NuGet v3 flat-container layout (DESIGN §6.3).
func ResolveURL(a *spec.Artifact) (string, error) {
	switch a.Source.Type {
	case "http":
		return a.Source.URL, nil
	case "nuget_feed":
		id := strings.ToLower(a.Source.PackageID)
		v := strings.ToLower(a.Version)
		base := strings.TrimRight(a.Source.FeedURL, "/")
		return fmt.Sprintf("%s/flatcontainer/%s/%s/%s.%s.nupkg", base, id, v, id, v), nil
	default:
		return "", fmt.Errorf("no URL for source type %q", a.Source.Type)
	}
}

// AuthHeader resolves the (headerName, headerValue) pair from env-var names.
// Value never logged (DESIGN §11).
func AuthHeader(a *spec.Artifact) (name, value string) {
	au := a.Source.Auth
	if au.TokenEnv == "" {
		return "", ""
	}
	tok := os.Getenv(au.TokenEnv)
	name = au.Header
	if name == "" {
		name = "Authorization"
	}
	switch a.Source.Type {
	case "nuget_feed":
		if au.Scheme == "bearer" {
			return name, "Bearer " + tok
		}
		// ADO PAT convention: Basic base64("pat:" + token) unless user set
		user := au.Username
		if user == "" {
			user = "pat"
		}
		return name, "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok))
	default:
		if au.Header != "" && au.Header != "Authorization" {
			return name, tok // custom header carries raw token
		}
		if strings.Contains(tok, " ") {
			return name, tok // caller supplied full "Scheme value"
		}
		return name, "Bearer " + tok
	}
}

// Fetch downloads zip/nupkg to a runner temp file, verifying sha256 while
// streaming. docker_image never reaches here (pattern pulls on target).
func Fetch(ctx context.Context, a *spec.Artifact) (*Fetched, error) {
	switch a.Source.Type {
	case "file":
		return fetchFile(a)
	case "http", "nuget_feed":
		return fetchHTTP(ctx, a)
	default:
		return nil, &CodedError{"ERR_ARTIFACT_FETCH",
			fmt.Errorf("runner fetch unsupported for source %q", a.Source.Type)}
	}
}

func verify(a *spec.Artifact, gotHex string, tmp string) error {
	want := strings.TrimPrefix(a.Checksum, "sha256:")
	if gotHex != want {
		os.Remove(tmp)
		return &CodedError{"ERR_CHECKSUM_MISMATCH",
			fmt.Errorf("sha256 %s != expected %s", gotHex, want)}
	}
	return nil
}

func fetchFile(a *spec.Artifact) (*Fetched, error) {
	src, err := os.Open(a.Source.Path)
	if err != nil {
		return nil, &CodedError{"ERR_ARTIFACT_FETCH", err}
	}
	defer src.Close()
	return spool(a, src)
}

func fetchHTTP(ctx context.Context, a *spec.Artifact) (*Fetched, error) {
	url, err := ResolveURL(a)
	if err != nil {
		return nil, &CodedError{"ERR_ARTIFACT_FETCH", err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &CodedError{"ERR_ARTIFACT_FETCH", err}
	}
	if hn, hv := AuthHeader(a); hn != "" {
		req.Header.Set(hn, hv)
	}
	cli := &http.Client{Timeout: 30 * time.Minute}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, &CodedError{"ERR_ARTIFACT_FETCH", err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		peek := make([]byte, 256)
		n, _ := io.ReadFull(resp.Body, peek)
		return nil, &CodedError{"ERR_ARTIFACT_FETCH",
			fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, strings.TrimSpace(string(peek[:n])))}
	}
	return spool(a, resp.Body)
}

func spool(a *spec.Artifact, r io.Reader) (*Fetched, error) {
	tmp, err := os.CreateTemp("", "labdeploy-*.pkg")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	cerr := tmp.Close()
	if err != nil || cerr != nil {
		os.Remove(tmp.Name())
		return nil, &CodedError{"ERR_ARTIFACT_FETCH", fmt.Errorf("spool: %v/%v", err, cerr)}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if err := verify(a, got, tmp.Name()); err != nil {
		return nil, err
	}
	return &Fetched{LocalPath: tmp.Name(), Sha256: got, Size: n}, nil
}

// UseTargetPull decides fetch strategy (DESIGN §6.3 default rule).
func UseTargetPull(a *spec.Artifact) bool {
	if a.FetchMode == "runner_push" {
		return false
	}
	if a.FetchMode == "target_pull" {
		return true
	}
	return a.Source.Type == "http" || a.Source.Type == "nuget_feed"
}

// TargetPullScripts return remote scripts that download + sha-verify the
// package into <staging>/pkg.zip. Auth token travels via Cmd.Env only.
// Exit 41 == ERR_CHECKSUM_MISMATCH per DESIGN §8.3.
func TargetPullScriptWindows(a *spec.Artifact, stagingPkg string) (script string, env map[string]string, err error) {
	url, err := ResolveURL(a)
	if err != nil {
		return "", nil, err
	}
	hn, hv := AuthHeader(a)
	env = map[string]string{}
	headerLine := "$h=@{}"
	if hn != "" {
		env["LD_AUTH_VALUE"] = hv
		headerLine = fmt.Sprintf("$h=@{}; if($env:LD_AUTH_VALUE){$h['%s']=$env:LD_AUTH_VALUE}", hn)
	}
	want := strings.TrimPrefix(a.Checksum, "sha256:")
	script = fmt.Sprintf(`$ProgressPreference='SilentlyContinue'
$ErrorActionPreference='Stop'
%s
try { Invoke-WebRequest -UseBasicParsing -Uri '%s' -Headers $h -OutFile '%s' }
catch { Write-Error $_.Exception.Message; exit 40 }
$sha=(Get-FileHash '%s' -Algorithm SHA256).Hash.ToLower()
if($sha -ne '%s'){ Write-Error "checksum mismatch: $sha"; exit 41 }
exit 0`, headerLine, url, stagingPkg, stagingPkg, want)
	return script, env, nil
}

func TargetPullScriptLinux(a *spec.Artifact, stagingPkg string) (script string, env map[string]string, err error) {
	url, err := ResolveURL(a)
	if err != nil {
		return "", nil, err
	}
	hn, hv := AuthHeader(a)
	env = map[string]string{}
	hdr := ""
	if hn != "" {
		env["LD_AUTH_VALUE"] = hv
		hdr = fmt.Sprintf(`-H "%s: $LD_AUTH_VALUE" `, hn)
	}
	want := strings.TrimPrefix(a.Checksum, "sha256:")
	script = fmt.Sprintf(`set -e
curl -fsSL %s-o '%s' '%s' || exit 40
got=$(sha256sum '%s' | cut -d' ' -f1)
[ "$got" = "%s" ] || { echo "checksum mismatch: $got" >&2; exit 41; }
exit 0`, hdr, stagingPkg, url, stagingPkg, want)
	return script, env, nil
}
