package pipeline

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ADOClient is a minimal Azure DevOps REST client (Go stdlib only) used by PIP-03
// to queue the shipped azure-pipelines.yml on pool LabAgents, wait for the run,
// and assert the published VSTest results and pipeline artifact.
type ADOClient struct {
	Organization string
	Project      string
	PAT          string
	HTTP         *http.Client
	// baseURL is the ADO REST root (default https://dev.azure.com); overridable so
	// deterministic tests can point the client at an httptest server.
	baseURL string
}

// NewADOClient constructs a client. The PAT is sent as HTTP basic auth with an
// empty username, per the ADO REST convention.
func NewADOClient(org, project, pat string) *ADOClient {
	return &ADOClient{
		Organization: org,
		Project:      project,
		PAT:          pat,
		HTTP:         &http.Client{Timeout: 60 * time.Second},
		baseURL:      "https://dev.azure.com",
	}
}

func (c *ADOClient) base() string {
	if c.baseURL == "" {
		return "https://dev.azure.com"
	}
	return c.baseURL
}

func (c *ADOClient) authHeader() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+c.PAT))
}

func (c *ADOClient) do(ctx context.Context, method, url string, body []byte) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
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

// BuildDefinition captures the identity fields of an ADO build/YAML pipeline
// definition that PIP-03 asserts before queueing: the YAML file it runs, and the
// repository it is bound to. Sourced from the Build Definitions API, which (unlike
// the leaner Pipelines API) exposes the repository name/type/defaultBranch.
type BuildDefinition struct {
	YamlFilename  string
	RepoName      string
	RepoType      string
	DefaultBranch string
}

// GetBuildDefinition returns the identity of build/pipeline definition id. PIP-03
// uses it to refuse smoke-testing any pipeline that is not the shipped
// azure-pipelines.yml bound to the expected lab repository and branch — an id alone
// is not proof of identity.
func (c *ADOClient) GetBuildDefinition(ctx context.Context, id int) (BuildDefinition, error) {
	url := fmt.Sprintf("%s/%s/%s/_apis/build/definitions/%d?api-version=7.1", c.base(), c.Organization, c.Project, id)
	resp, raw, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return BuildDefinition{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return BuildDefinition{}, fmt.Errorf("get build definition %d: status %d: %s", id, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Process struct {
			YamlFilename string `json:"yamlFilename"`
		} `json:"process"`
		Repository struct {
			Name          string `json:"name"`
			Type          string `json:"type"`
			DefaultBranch string `json:"defaultBranch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return BuildDefinition{}, err
	}
	return BuildDefinition{
		YamlFilename:  out.Process.YamlFilename,
		RepoName:      out.Repository.Name,
		RepoType:      out.Repository.Type,
		DefaultBranch: out.Repository.DefaultBranch,
	}, nil
}

// QueueRun queues a run of pipelineID with the given template parameters
// (version, checksum) and returns the run id.
func (c *ADOClient) QueueRun(ctx context.Context, pipelineID int, params map[string]string) (int, error) {
	url := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%d/runs?api-version=7.1", c.base(), c.Organization, c.Project, pipelineID)
	payload, _ := json.Marshal(map[string]interface{}{
		"templateParameters": params,
	})
	resp, raw, err := c.do(ctx, http.MethodPost, url, payload)
	if err != nil {
		return 0, fmt.Errorf("[ERR_ADO_QUEUE] %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("[ERR_ADO_QUEUE] queue pipeline %d: status %d: %s", pipelineID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("[ERR_ADO_QUEUE] decode run: %w", err)
	}
	return out.ID, nil
}

// WaitRun polls run runID of pipelineID until state=completed and returns its
// result (e.g. "succeeded", "failed").
func (c *ADOClient) WaitRun(ctx context.Context, pipelineID, runID int, timeout time.Duration) (string, error) {
	url := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%d/runs/%d?api-version=7.1", c.base(), c.Organization, c.Project, pipelineID, runID)
	deadline := time.Now().Add(timeout)
	for {
		resp, raw, err := c.do(ctx, http.MethodGet, url, nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			var out struct {
				State  string `json:"state"`
				Result string `json:"result"`
			}
			if json.Unmarshal(raw, &out) == nil && out.State == "completed" {
				return out.Result, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("[ERR_ADO_TIMEOUT] run %d did not complete within %s", runID, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// TestResultCount returns the number of VSTest results published for the ADO build
// runID (the Tests tab count asserted by PIP-03). ADO pipeline runs share the id
// with the underlying build, so the Test Management API is queried by buildUri.
func (c *ADOClient) TestResultCount(ctx context.Context, runID int) (int, error) {
	url := fmt.Sprintf("%s/%s/%s/_apis/test/runs?buildUri=vstfs:///Build/Build/%d&api-version=7.1", c.base(), c.Organization, c.Project, runID)
	resp, raw, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("test runs for build %d: status %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Value []struct {
			TotalTests int `json:"totalTests"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, err
	}
	total := 0
	for _, r := range out.Value {
		total += r.TotalTests
	}
	return total, nil
}

// ArtifactNames returns the names of pipeline artifacts published by build runID.
func (c *ADOClient) ArtifactNames(ctx context.Context, runID int) ([]string, error) {
	url := fmt.Sprintf("%s/%s/%s/_apis/build/builds/%d/artifacts?api-version=7.1", c.base(), c.Organization, c.Project, runID)
	resp, raw, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifacts for build %d: status %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Value []struct {
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Value))
	for _, a := range out.Value {
		names = append(names, a.Name)
	}
	return names, nil
}
