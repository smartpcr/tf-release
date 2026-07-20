// Package pipeline implements the Stage 9.2 pipeline-acceptance step
// (implementation-plan.md:484): it registers, fetches and drives the SHIPPED
// reference pipelines (examples/pipelines/github-deploy.yml,
// examples/pipelines/azure-pipelines.yml) against the L4 lab to satisfy DESIGN
// §18.10 (PIP-01..03) and the supplemental PIP_REG-01/PIP_REG-02 proofs
// (docs/stories/release-RELEASE-PROVIDER/e2e-scenarios.md, Phase 8).
//
// The GitHub leg runs the shipped github-deploy.yml with exactly ONE documented
// addition: an `environment: lab` binding on each job so real GitHub Actions
// injects the GitHub environment `lab` secrets. RegisterWorkflow produces that
// body from the committed template; StripEnvironmentBinding is its exact inverse,
// so PIP_REG-01 can prove the ONLY delta is the environment binding (sha256 match
// after stripping). FetchRemoteWorkflow reads the registered workflow back from the
// lab repo's default branch via the GitHub REST contents API (Go net/http only —
// no `gh` CLI) so PIP_REG-02 can fail a stale registration before any dispatch.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// envBindingLine is the single documented addition RegisterWorkflow makes to each
// job of the shipped github-deploy.yml. Jobs live at two-space indent under `jobs:`,
// so job-level keys are four-space indented.
const envBindingLine = "    environment: lab"

// jobHeaders are the exact job-key lines after which the environment binding is
// inserted (the two jobs in the shipped github-deploy.yml).
var jobHeaders = []string{"  deploy:", "  rollback:"}

// RegisterWorkflow returns the registered workflow body: the shipped github-deploy.yml
// with `environment: lab` inserted immediately after each job header. It errors if
// any expected job header is missing so a renamed/dropped job cannot silently skip
// the binding.
func RegisterWorkflow(shipped []byte) ([]byte, error) {
	lines := strings.Split(string(shipped), "\n")
	out := make([]string, 0, len(lines)+len(jobHeaders))
	bound := map[string]bool{}
	for _, ln := range lines {
		out = append(out, ln)
		for _, h := range jobHeaders {
			if ln == h {
				out = append(out, envBindingLine)
				bound[h] = true
			}
		}
	}
	for _, h := range jobHeaders {
		if !bound[h] {
			return nil, fmt.Errorf("[ERR_REGISTER_WORKFLOW] shipped github-deploy.yml is missing job header %q; cannot bind environment: lab", strings.TrimSpace(h))
		}
	}
	return []byte(strings.Join(out, "\n")), nil
}

// StripEnvironmentBinding is the exact inverse of RegisterWorkflow: it removes every
// `    environment: lab` line, yielding the original shipped body. PIP_REG-01 uses
// this to prove RegisterWorkflow adds nothing else.
func StripEnvironmentBinding(registered []byte) []byte {
	lines := strings.Split(string(registered), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if ln == envBindingLine {
			continue
		}
		out = append(out, ln)
	}
	return []byte(strings.Join(out, "\n"))
}

// SHA256Hex returns the lowercase hex sha256 of b (used for the byte-for-byte
// equality proofs in PIP_REG-01 and PIP_REG-02).
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// RepoRoot returns the repository root, resolved from THIS source file's location
// (tests/e2e/pipeline → three levels up) so the shipped-template reads work
// regardless of the test's working directory.
func RepoRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", ".."))
}

// ShippedGitHubWorkflowPath / ShippedAzurePipelinePath locate the committed
// reference pipelines.
func ShippedGitHubWorkflowPath() string {
	return filepath.Join(RepoRoot(), "examples", "pipelines", "github-deploy.yml")
}

func ShippedAzurePipelinePath() string {
	return filepath.Join(RepoRoot(), "examples", "pipelines", "azure-pipelines.yml")
}

// ReadShippedGitHubWorkflow reads the committed github-deploy.yml.
func ReadShippedGitHubWorkflow() ([]byte, error) {
	return os.ReadFile(ShippedGitHubWorkflowPath())
}

// FetchRemoteWorkflow GETs the registered workflow from the lab repo's default
// branch via the GitHub REST contents API using only the Go stdlib (no `gh` CLI),
// authenticating with a bearer token, and returns its decoded bytes. It is the
// PIP_REG-02 pre-dispatch fetch: a stale/hand-edited remote workflow yields bytes
// that differ from RegisterWorkflow's output, failing the run before any dispatch.
func FetchRemoteWorkflow(ctx context.Context, owner, repo, path, ref, token string) ([]byte, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, path)
	if ref != "" {
		u += "?ref=" + ref
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("[ERR_STALE_WORKFLOW] build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[ERR_STALE_WORKFLOW] contents GET: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("[ERR_STALE_WORKFLOW] contents GET %s: status %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("[ERR_STALE_WORKFLOW] decode contents JSON: %w", err)
	}
	if payload.Encoding != "base64" {
		return nil, fmt.Errorf("[ERR_STALE_WORKFLOW] unexpected content encoding %q", payload.Encoding)
	}
	// The GitHub API wraps base64 at 60 columns with newlines.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(payload.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("[ERR_STALE_WORKFLOW] base64-decode content: %w", err)
	}
	return decoded, nil
}
