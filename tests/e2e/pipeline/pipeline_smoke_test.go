package pipeline

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runTimeout bounds a single live pipeline run (deploy + e2e or a rollback).
const runTimeout = 30 * time.Minute

// PIP-01: GitHub deploy workflow publishes results.
// Dispatches the registered workflow with a good version and asserts the run is
// green and uploads the labdeploy-results-<version> artifact. Skips without the L4
// lab.
func TestPIP_01_GitHubDeployPublishesResults(t *testing.T) {
	ownerRepo := labEnv(t, "LD_GH_LAB_REPO")
	token := labEnv(t, "LD_GH_TOKEN")
	owner, repo := splitOwnerRepo(t, ownerRepo)
	workflow := envOr("LD_GH_LAB_WORKFLOW", "labdeploy-e2e.yml")
	branch := envOr("LD_GH_LAB_BRANCH", "main")
	version := envOr("LD_PIP_GOOD_VERSION", "1.1.0")
	checksum := labEnv(t, "LD_PIP_GOOD_CHECKSUM")

	gh := NewGitHubClient(owner, repo, token)
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	since, err := gh.DispatchWorkflow(ctx, workflow, branch, map[string]string{"version": version, "checksum": checksum})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	runID, err := gh.FindRun(ctx, workflow, since, 3*time.Minute)
	if err != nil {
		t.Fatalf("find run: %v", err)
	}
	conclusion, err := gh.WaitRun(ctx, runID, runTimeout)
	if err != nil {
		t.Fatalf("wait run %d: %v", runID, err)
	}
	if conclusion != "success" {
		t.Fatalf("PIP-01 deploy run %d concluded %q, want success", runID, conclusion)
	}
	artifacts, err := gh.ListArtifactNames(ctx, runID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	want := "labdeploy-results-" + version
	if !contains(artifacts, want) {
		t.Fatalf("PIP-01 run %d did not upload artifact %q; got %v", runID, want, artifacts)
	}
}

// PIP-02: GitHub deploy failure triggers auto-rollback.
// Dispatches a bad version; the deploy job must fail (ERR_SERVICE_START) and the
// rollback job (if: failure()) must end green, restoring v=LAST_GOOD_VERSION.
func TestPIP_02_GitHubDeployFailureTriggersRollback(t *testing.T) {
	ownerRepo := labEnv(t, "LD_GH_LAB_REPO")
	token := labEnv(t, "LD_GH_TOKEN")
	owner, repo := splitOwnerRepo(t, ownerRepo)
	workflow := envOr("LD_GH_LAB_WORKFLOW", "labdeploy-e2e.yml")
	branch := envOr("LD_GH_LAB_BRANCH", "main")
	badVersion := envOr("LD_PIP_BAD_VERSION", "1.2.0-bad")
	badChecksum := labEnv(t, "LD_PIP_BAD_CHECKSUM")
	goodVersion := envOr("LD_PIP_GOOD_VERSION", "1.1.0")

	gh := NewGitHubClient(owner, repo, token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*runTimeout)
	defer cancel()

	since, err := gh.DispatchWorkflow(ctx, workflow, branch, map[string]string{"version": badVersion, "checksum": badChecksum})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	runID, err := gh.FindRun(ctx, workflow, since, 3*time.Minute)
	if err != nil {
		t.Fatalf("find run: %v", err)
	}
	if _, err := gh.WaitRun(ctx, runID, 2*runTimeout); err != nil {
		t.Fatalf("wait run %d: %v", runID, err)
	}
	jobs, err := gh.JobConclusions(ctx, runID)
	if err != nil {
		t.Fatalf("job conclusions: %v", err)
	}
	if c := jobs["deploy"]; c != "failure" {
		t.Errorf("PIP-02 deploy job concluded %q, want failure (ERR_SERVICE_START)", c)
	}
	if c := jobs["rollback"]; c != "success" {
		t.Fatalf("PIP-02 rollback job concluded %q, want success", c)
	}
	// Optional recovery assertion: the target serves the last-known-good version.
	if healthURL := strings.TrimSpace(envOr("LD_PIP_HEALTH_URL", "")); healthURL != "" {
		assertServesVersion(ctx, t, healthURL, goodVersion)
	}
}

// PIP-03: Azure DevOps pass case publishes to the Tests tab.
// Queues the shipped azure-pipelines.yml on pool LabAgents; the run must succeed,
// publish 3 VSTest results, and publish the labdeploy-results-<version> artifact.
func TestPIP_03_AzureDevOpsPassPublishesTests(t *testing.T) {
	org := labEnv(t, "LD_ADO_ORG")
	project := labEnv(t, "LD_ADO_PROJECT")
	pat := labEnv(t, "LD_ADO_PAT")
	pipelineID, err := strconv.Atoi(labEnv(t, "LD_ADO_PIPELINE_ID"))
	if err != nil {
		t.Fatalf("LD_ADO_PIPELINE_ID must be an integer: %v", err)
	}
	version := envOr("LD_PIP_GOOD_VERSION", "1.1.0")
	checksum := labEnv(t, "LD_PIP_GOOD_CHECKSUM")

	ado := NewADOClient(org, project, pat)
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	runID, err := ado.QueueRun(ctx, pipelineID, map[string]string{"version": version, "checksum": checksum})
	if err != nil {
		t.Fatalf("queue ADO run: %v", err)
	}
	result, err := ado.WaitRun(ctx, pipelineID, runID, runTimeout)
	if err != nil {
		t.Fatalf("wait ADO run %d: %v", runID, err)
	}
	if result != "succeeded" {
		t.Fatalf("PIP-03 ADO run %d result %q, want succeeded", runID, result)
	}
	count, err := ado.TestResultCount(ctx, runID)
	if err != nil {
		t.Fatalf("test result count: %v", err)
	}
	if count != 3 {
		t.Errorf("PIP-03 Tests tab shows %d VSTest results, want 3", count)
	}
	artifacts, err := ado.ArtifactNames(ctx, runID)
	if err != nil {
		t.Fatalf("artifact names: %v", err)
	}
	want := "labdeploy-results-" + version
	if !contains(artifacts, want) {
		t.Fatalf("PIP-03 run %d did not publish artifact %q; got %v", runID, want, artifacts)
	}
}

// assertServesVersion GETs healthURL and asserts the response body reports version.
func assertServesVersion(ctx context.Context, t *testing.T, healthURL, version string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		t.Fatalf("build health request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health GET %s: %v", healthURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), version) {
		t.Fatalf("PIP-02 recovery: health at %s does not report v=%s; body=%s", healthURL, version, strings.TrimSpace(string(body)))
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
