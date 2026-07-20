package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// labEnv returns the value of key, or skips the test when it is unset — the
// documented behavior for the live lab-tier PIP proofs (e2e-scenarios.md Phase 8:
// "skip when the L4 lab is unavailable").
func labEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Skipf("%s not set; L4 lab unavailable — skipping live pipeline scenario", key)
	}
	return v
}

// splitOwnerRepo splits "owner/repo" into its two parts.
func splitOwnerRepo(t *testing.T, ownerRepo string) (string, string) {
	t.Helper()
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("LD_GH_LAB_REPO must be owner/repo, got %q", ownerRepo)
	}
	return parts[0], parts[1]
}

// TestPIP_LINT_01_ShippedPipelinesWellFormed is the gate-tier structural proof: the
// two committed reference pipelines exist, are non-empty, and carry their
// always()-guarded publish steps. (The deep topology assertions live in
// examples/examples_test.go; this keeps Phase 8 self-contained gate coverage in the
// pipeline package.)
func TestPIP_LINT_01_ShippedPipelinesWellFormed(t *testing.T) {
	gh, err := os.ReadFile(ShippedGitHubWorkflowPath())
	if err != nil {
		t.Fatalf("read shipped github-deploy.yml: %v", err)
	}
	if !strings.Contains(string(gh), "actions/upload-artifact") || !strings.Contains(string(gh), "if: always()") {
		t.Errorf("github-deploy.yml must upload artifacts with `if: always()`")
	}
	ado, err := os.ReadFile(ShippedAzurePipelinePath())
	if err != nil {
		t.Fatalf("read shipped azure-pipelines.yml: %v", err)
	}
	if !strings.Contains(string(ado), "PublishTestResults@2") || !strings.Contains(string(ado), "condition: always()") {
		t.Errorf("azure-pipelines.yml must publish test results with `condition: always()`")
	}
}

// TestPIP_REG_01_RegisteredWorkflowEqualsShippedPlusEnvBinding is the gate-tier
// proof that RegisterWorkflow adds ONLY the `environment: lab` binding: the
// registered body binds it on both jobs, and stripping exactly those lines yields a
// byte-for-byte sha256 match to the shipped template.
func TestPIP_REG_01_RegisteredWorkflowEqualsShippedPlusEnvBinding(t *testing.T) {
	shipped, err := ReadShippedGitHubWorkflow()
	if err != nil {
		t.Fatalf("read shipped github-deploy.yml: %v", err)
	}
	registered, err := RegisterWorkflow(shipped)
	if err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	// Both jobs must carry the environment binding.
	if got := strings.Count(string(registered), envBindingLine+"\n") + boolToInt(strings.HasSuffix(string(registered), envBindingLine)); got != len(jobHeaders) {
		t.Errorf("registered workflow must bind `environment: lab` on %d jobs, found %d occurrences", len(jobHeaders), got)
	}
	// The binding must sit directly under each job header.
	for _, h := range jobHeaders {
		if !strings.Contains(string(registered), h+"\n"+envBindingLine+"\n") {
			t.Errorf("expected `environment: lab` immediately after job header %q", strings.TrimSpace(h))
		}
	}

	// Stripping the binding must reproduce the shipped template exactly.
	roundTrip := StripEnvironmentBinding(registered)
	if SHA256Hex(roundTrip) != SHA256Hex(shipped) {
		t.Errorf("stripping `environment: lab` did not reproduce the shipped template:\n shipped sha256   = %s\n stripped sha256  = %s",
			SHA256Hex(shipped), SHA256Hex(roundTrip))
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestPIP_REG_02_RemoteWorkflowMatchesGeneratedBody is the live pre-dispatch proof:
// it fetches the ACTUAL registered workflow from the lab repo default branch and
// byte-compares it to RegisterWorkflow's output, so a stale/hand-edited remote
// registration fails here instead of dispatching altered content. Skips when the L4
// lab is unavailable.
func TestPIP_REG_02_RemoteWorkflowMatchesGeneratedBody(t *testing.T) {
	ownerRepo := labEnv(t, "LD_GH_LAB_REPO")
	token := labEnv(t, "LD_GH_TOKEN")
	owner, repo := splitOwnerRepo(t, ownerRepo)
	branch := envOr("LD_GH_LAB_BRANCH", "main")
	workflowPath := envOr("LD_GH_LAB_WORKFLOW_PATH", ".github/workflows/labdeploy-e2e.yml")

	shipped, err := ReadShippedGitHubWorkflow()
	if err != nil {
		t.Fatalf("read shipped github-deploy.yml: %v", err)
	}
	want, err := RegisterWorkflow(shipped)
	if err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got, err := FetchRemoteWorkflow(ctx, owner, repo, workflowPath, branch, token)
	if err != nil {
		t.Fatalf("FetchRemoteWorkflow: %v", err)
	}
	if SHA256Hex(got) != SHA256Hex(want) {
		t.Fatalf("[ERR_STALE_WORKFLOW] remote %s@%s does not match the registered body:\n remote sha256    = %s\n expected sha256  = %s",
			workflowPath, branch, SHA256Hex(got), SHA256Hex(want))
	}
}

// envOr returns the env value for key or def when unset.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
