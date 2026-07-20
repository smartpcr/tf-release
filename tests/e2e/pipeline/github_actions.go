package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHubClient is a minimal GitHub Actions REST client built on the Go stdlib
// (no `gh` CLI). It drives the shipped github-deploy.yml on the L4 lab for
// PIP-01/PIP-02: dispatch a workflow, wait for the run, and inspect its
// conclusion and uploaded artifacts.
type GitHubClient struct {
	Owner string
	Repo  string
	Token string
	HTTP  *http.Client
}

// NewGitHubClient constructs a client for owner/repo authenticated with a bearer
// token.
func NewGitHubClient(owner, repo, token string) *GitHubClient {
	return &GitHubClient{
		Owner: owner,
		Repo:  repo,
		Token: token,
		HTTP:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *GitHubClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, []byte, error) {
	u := "https://api.github.com/repos/" + c.Owner + "/" + c.Repo + path
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw, nil
}

// DispatchWorkflow triggers workflow_dispatch on workflowFile (e.g.
// "labdeploy-e2e.yml") at ref with the given inputs. It returns the UTC time just
// before dispatch so the caller can find the run it created.
func (c *GitHubClient) DispatchWorkflow(ctx context.Context, workflowFile, ref string, inputs map[string]string) (time.Time, error) {
	since := time.Now().UTC().Add(-5 * time.Second)
	payload, _ := json.Marshal(map[string]interface{}{"ref": ref, "inputs": inputs})
	resp, raw, err := c.do(ctx, http.MethodPost, "/actions/workflows/"+workflowFile+"/dispatches", payload)
	if err != nil {
		return since, fmt.Errorf("[ERR_DISPATCH] %w", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		return since, fmt.Errorf("[ERR_DISPATCH] dispatch %s: status %d: %s", workflowFile, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return since, nil
}

type workflowRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CreatedAt  string `json:"created_at"`
	Event      string `json:"event"`
}

// FindRun polls for the workflow_dispatch run created at/after `since` for
// workflowFile and returns its id once one appears.
func (c *GitHubClient) FindRun(ctx context.Context, workflowFile string, since time.Time, timeout time.Duration) (int64, error) {
	deadline := time.Now().Add(timeout)
	for {
		resp, raw, err := c.do(ctx, http.MethodGet, "/actions/workflows/"+workflowFile+"/runs?event=workflow_dispatch&per_page=20", nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			var out struct {
				Runs []workflowRun `json:"workflow_runs"`
			}
			if json.Unmarshal(raw, &out) == nil {
				for _, r := range out.Runs {
					created, perr := time.Parse(time.RFC3339, r.CreatedAt)
					if perr == nil && !created.Before(since.Add(-time.Minute)) {
						return r.ID, nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("[ERR_RUN_NOT_FOUND] no workflow_dispatch run for %s within %s", workflowFile, timeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// WaitRun polls run runID until status=completed and returns its conclusion
// (e.g. "success", "failure").
func (c *GitHubClient) WaitRun(ctx context.Context, runID int64, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	path := fmt.Sprintf("/actions/runs/%d", runID)
	for {
		resp, raw, err := c.do(ctx, http.MethodGet, path, nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			var r workflowRun
			if json.Unmarshal(raw, &r) == nil && r.Status == "completed" {
				return r.Conclusion, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("[ERR_RUN_TIMEOUT] run %d did not complete within %s", runID, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// ListArtifactNames returns the names of the artifacts uploaded by run runID.
func (c *GitHubClient) ListArtifactNames(ctx context.Context, runID int64) ([]string, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/runs/%d/artifacts", runID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list artifacts run %d: status %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Artifacts []struct {
			Name string `json:"name"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Artifacts))
	for _, a := range out.Artifacts {
		names = append(names, a.Name)
	}
	return names, nil
}

// JobConclusions returns each job name → conclusion for run runID (used to assert
// the deploy job failed and the rollback job succeeded in PIP-02).
func (c *GitHubClient) JobConclusions(ctx context.Context, runID int64) (map[string]string, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/runs/%d/jobs", runID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list jobs run %d: status %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Jobs []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, j := range out.Jobs {
		m[j.Name] = j.Conclusion
	}
	return m, nil
}
