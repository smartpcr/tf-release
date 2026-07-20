package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
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
// Before dispatch it proves the remote workflow matches the registered body (so we
// never smoke-test stale content), dispatches the registered workflow with a good
// version, waits for the NEW run (matched by monotonic id, not a time window), and
// asserts the run is green and the labdeploy-results-<version> artifact actually
// CONTAINS the TRX, summary.json and collected logs — with summary.json reporting
// the test counts. Skips without the L4 lab.
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

	// Ordering guard (item 8): the remote registration MUST match the body we
	// generate before we dispatch it.
	requireRemoteWorkflowCurrent(ctx, t, owner, repo, branch, token)

	before, err := gh.LatestRunID(ctx, workflow)
	if err != nil {
		t.Fatalf("baseline run id: %v", err)
	}
	if _, err := gh.DispatchWorkflow(ctx, workflow, branch, map[string]string{"version": version, "checksum": checksum}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	runID, err := gh.FindRunAfter(ctx, workflow, branch, before, 3*time.Minute)
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

	// The artifact must exist AND carry the real payload (TRX + summary + logs).
	artName := "labdeploy-results-" + version
	artID, err := gh.FindArtifact(ctx, runID, artName)
	if err != nil {
		t.Fatalf("find artifact: %v", err)
	}
	zipBytes, err := gh.DownloadArtifactZip(ctx, artID)
	if err != nil {
		t.Fatalf("download artifact %q: %v", artName, err)
	}
	assertResultsArtifact(t, zipBytes, artName)
}

// PIP-02: GitHub deploy failure triggers auto-rollback.
// Dispatches a bad version; the deploy job must fail with [ERR_SERVICE_START] in
// its log, the rollback job (if: failure()) must end green, and the target must
// serve the recovered good version — a REQUIRED assertion (no opt-out).
func TestPIP_02_GitHubDeployFailureTriggersRollback(t *testing.T) {
	ownerRepo := labEnv(t, "LD_GH_LAB_REPO")
	token := labEnv(t, "LD_GH_TOKEN")
	owner, repo := splitOwnerRepo(t, ownerRepo)
	workflow := envOr("LD_GH_LAB_WORKFLOW", "labdeploy-e2e.yml")
	branch := envOr("LD_GH_LAB_BRANCH", "main")
	badVersion := envOr("LD_PIP_BAD_VERSION", "1.2.0-bad")
	badChecksum := labEnv(t, "LD_PIP_BAD_CHECKSUM")
	goodVersion := envOr("LD_PIP_GOOD_VERSION", "1.1.0")
	// The recovery assertion is REQUIRED (item 12): the health URL must be set so we
	// can prove the rollback restored the good version.
	healthURL := labEnv(t, "LD_PIP_HEALTH_URL")

	gh := NewGitHubClient(owner, repo, token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*runTimeout)
	defer cancel()

	requireRemoteWorkflowCurrent(ctx, t, owner, repo, branch, token)

	before, err := gh.LatestRunID(ctx, workflow)
	if err != nil {
		t.Fatalf("baseline run id: %v", err)
	}
	if _, err := gh.DispatchWorkflow(ctx, workflow, branch, map[string]string{"version": badVersion, "checksum": badChecksum}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	runID, err := gh.FindRunAfter(ctx, workflow, branch, before, 3*time.Minute)
	if err != nil {
		t.Fatalf("find run: %v", err)
	}
	if _, err := gh.WaitRun(ctx, runID, 2*runTimeout); err != nil {
		t.Fatalf("wait run %d: %v", runID, err)
	}
	jobs, err := gh.JobDetails(ctx, runID)
	if err != nil {
		t.Fatalf("job details: %v", err)
	}
	deploy, ok := jobByName(jobs, "deploy")
	if !ok {
		t.Fatalf("PIP-02 run %d has no deploy job; jobs=%v", runID, jobNames(jobs))
	}
	if deploy.Conclusion != "failure" {
		t.Errorf("PIP-02 deploy job concluded %q, want failure", deploy.Conclusion)
	}
	// The deploy must have failed for the RIGHT reason (item 11).
	log, err := gh.DownloadJobLog(ctx, deploy.ID)
	if err != nil {
		t.Fatalf("download deploy job log: %v", err)
	}
	if !strings.Contains(log, "[ERR_SERVICE_START]") {
		t.Errorf("PIP-02 deploy job log does not contain [ERR_SERVICE_START]; failure reason unproven")
	}
	rollback, ok := jobByName(jobs, "rollback")
	if !ok {
		t.Fatalf("PIP-02 run %d has no rollback job; jobs=%v", runID, jobNames(jobs))
	}
	if rollback.Conclusion != "success" {
		t.Fatalf("PIP-02 rollback job concluded %q, want success", rollback.Conclusion)
	}
	// Required recovery assertion (item 12): the target serves the good version.
	assertServesVersion(ctx, t, healthURL, goodVersion)
}

// PIP-03: Azure DevOps pass case publishes to the Tests tab.
// It first proves the pipeline definition points at the shipped
// azure-pipelines.yml, then queues it on pool LabAgents; the run must succeed,
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

	// Item 13: refuse to smoke-test a pipeline whose definition is not the shipped
	// azure-pipelines.yml.
	yamlPath, err := ado.PipelineYAMLPath(ctx, pipelineID)
	if err != nil {
		t.Fatalf("resolve pipeline %d YAML path: %v", pipelineID, err)
	}
	if !strings.HasSuffix(strings.ReplaceAll(yamlPath, "\\", "/"), "azure-pipelines.yml") {
		t.Fatalf("PIP-03 pipeline %d points at %q, want the shipped examples/pipelines/azure-pipelines.yml", pipelineID, yamlPath)
	}

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

// assertResultsArtifact unzips the downloaded results artifact and asserts it
// carries at least one *.trx, a summary.json and at least one collected *.log, and
// that summary.json parses and reports the test counts (total/passed/failed).
func assertResultsArtifact(t *testing.T, zipBytes []byte, name string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("artifact %q is not a valid zip: %v", name, err)
	}
	var haveTRX, haveLog bool
	var summary []byte
	for _, f := range zr.File {
		lower := strings.ToLower(f.Name)
		switch {
		case strings.HasSuffix(lower, ".trx"):
			haveTRX = true
		case strings.HasSuffix(lower, ".log"):
			haveLog = true
		case strings.HasSuffix(lower, "summary.json"):
			rc, err := f.Open()
			if err == nil {
				summary, _ = io.ReadAll(rc)
				_ = rc.Close()
			}
		}
	}
	if !haveTRX {
		t.Errorf("PIP-01 artifact %q contains no *.trx", name)
	}
	if !haveLog {
		t.Errorf("PIP-01 artifact %q contains no collected *.log", name)
	}
	if summary == nil {
		t.Fatalf("PIP-01 artifact %q contains no summary.json", name)
	}
	var s struct {
		Total       *int  `json:"total"`
		PassedTests *int  `json:"passed_tests"`
		Failed      *int  `json:"failed"`
		Passed      *bool `json:"passed"`
	}
	if err := json.Unmarshal(summary, &s); err != nil {
		t.Fatalf("PIP-01 summary.json does not parse: %v", err)
	}
	if s.Total == nil || s.PassedTests == nil || s.Failed == nil || s.Passed == nil {
		t.Fatalf("PIP-01 summary.json does not report the test summary (total/passed_tests/failed/passed); got %s", strings.TrimSpace(string(summary)))
	}
}

// jobByName returns the job with the given name (case-insensitive).
func jobByName(jobs []JobInfo, name string) (JobInfo, bool) {
	for _, j := range jobs {
		if strings.EqualFold(j.Name, name) {
			return j, true
		}
	}
	return JobInfo{}, false
}

func jobNames(jobs []JobInfo) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
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
