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
	HeadBranch string `json:"head_branch"`
}

// normRef strips a refs/heads/ prefix so a dispatch ref ("main" or
// "refs/heads/main") compares equal to a run's head_branch ("main").
func normRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return strings.TrimSpace(ref)
}

// LatestRunID returns the highest run id currently present for workflowFile (0 if
// none). Captured BEFORE a dispatch so FindRunAfter can select strictly the new
// run rather than any concurrent one sharing a creation-time window.
func (c *GitHubClient) LatestRunID(ctx context.Context, workflowFile string) (int64, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, "/actions/workflows/"+workflowFile+"/runs?per_page=1", nil)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("latest run for %s: status %d: %s", workflowFile, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Runs []workflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	var max int64
	for _, r := range out.Runs {
		if r.ID > max {
			max = r.ID
		}
	}
	return max, nil
}

// FindRunAfter polls for the NEW workflow_dispatch run created after afterID for
// workflowFile on ref, and returns its id. Run ids are monotonic, so selecting the
// smallest id strictly greater than the pre-dispatch max — and requiring
// event=workflow_dispatch with a matching head_branch — pins the run THIS test
// created, never a concurrent dispatch sharing a creation-time window.
func (c *GitHubClient) FindRunAfter(ctx context.Context, workflowFile, ref string, afterID int64, timeout time.Duration) (int64, error) {
	wantRef := normRef(ref)
	deadline := time.Now().Add(timeout)
	for {
		resp, raw, err := c.do(ctx, http.MethodGet, "/actions/workflows/"+workflowFile+"/runs?event=workflow_dispatch&per_page=30", nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			var out struct {
				Runs []workflowRun `json:"workflow_runs"`
			}
			if json.Unmarshal(raw, &out) == nil {
				var best int64
				for _, r := range out.Runs {
					if r.ID <= afterID || r.Event != "workflow_dispatch" {
						continue
					}
					if wantRef != "" && normRef(r.HeadBranch) != wantRef {
						continue
					}
					if best == 0 || r.ID < best {
						best = r.ID
					}
				}
				if best != 0 {
					return best, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("[ERR_RUN_NOT_FOUND] no new workflow_dispatch run for %s@%s (id>%d) within %s", workflowFile, wantRef, afterID, timeout)
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
	jobs, err := c.JobDetails(ctx, runID)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, j := range jobs {
		m[j.Name] = j.Conclusion
	}
	return m, nil
}

// JobInfo carries the id/name/conclusion of a workflow-run job so PIP-02 can both
// assert the deploy job's conclusion AND download its log to prove the coded
// failure ([ERR_SERVICE_START]).
type JobInfo struct {
	ID         int64
	Name       string
	Conclusion string
}

// JobDetails returns the id/name/conclusion of every job in run runID.
func (c *GitHubClient) JobDetails(ctx context.Context, runID int64) ([]JobInfo, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/runs/%d/jobs", runID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list jobs run %d: status %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	jobs := make([]JobInfo, 0, len(out.Jobs))
	for _, j := range out.Jobs {
		jobs = append(jobs, JobInfo{ID: j.ID, Name: j.Name, Conclusion: j.Conclusion})
	}
	return jobs, nil
}

// DownloadJobLog returns the plain-text log of job jobID. GitHub answers with a 302
// to a signed URL serving the log; the stdlib client follows the redirect and reads
// the body.
func (c *GitHubClient) DownloadJobLog(ctx context.Context, jobID int64) (string, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/jobs/%d/logs", jobID), nil)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download job %d log: status %d", jobID, resp.StatusCode)
	}
	return string(raw), nil
}

// artifactRef is an artifact's id + name.
type artifactRef struct {
	ID   int64
	Name string
}

// FindArtifact returns the id of the artifact named `name` uploaded by run runID.
func (c *GitHubClient) FindArtifact(ctx context.Context, runID int64, name string) (int64, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/runs/%d/artifacts", runID), nil)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("list artifacts run %d: status %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Artifacts []artifactRef `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	for _, a := range out.Artifacts {
		if a.Name == name {
			return a.ID, nil
		}
	}
	return 0, fmt.Errorf("[ERR_ARTIFACT_MISSING] run %d did not upload artifact %q", runID, name)
}

// DownloadArtifactZip returns the raw zip bytes of artifact artifactID. GitHub
// answers with a 302 to blob storage; the stdlib client follows the redirect and
// reads the zip body.
func (c *GitHubClient) DownloadArtifactZip(ctx context.Context, artifactID int64) ([]byte, error) {
	resp, raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/actions/artifacts/%d/zip", artifactID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download artifact %d: status %d", artifactID, resp.StatusCode)
	}
	return raw, nil
}
