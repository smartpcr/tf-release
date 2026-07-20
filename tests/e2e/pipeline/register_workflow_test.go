package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
// two committed reference pipelines PARSE as YAML (well-formedness) and, navigated
// structurally, carry their always()-guarded publish steps — a GitHub
// actions/upload-artifact step gated `if: always()` and an ADO PublishTestResults@2
// task gated `condition: always()`. (Deep topology assertions live in
// examples/examples_test.go; this keeps Phase 8 self-contained gate coverage here,
// parsing rather than substring-matching so a malformed file cannot pass.)
func TestPIP_LINT_01_ShippedPipelinesWellFormed(t *testing.T) {
	gh := decodeYAMLDoc(t, ShippedGitHubWorkflowPath())
	top, ok := gh.(map[string]interface{})
	if !ok || top["jobs"] == nil {
		t.Fatalf("github-deploy.yml must be a mapping that defines jobs:")
	}
	if !anyMapping(gh, func(m map[string]interface{}) bool {
		return strings.Contains(yamlStr(m, "uses"), "actions/upload-artifact") &&
			strings.Contains(yamlStr(m, "if"), "always()")
	}) {
		t.Errorf("github-deploy.yml must upload artifacts with `if: always()`")
	}

	ado := decodeYAMLDoc(t, ShippedAzurePipelinePath())
	if _, ok := ado.(map[string]interface{}); !ok {
		t.Fatalf("azure-pipelines.yml must be a YAML mapping")
	}
	if !anyMapping(ado, func(m map[string]interface{}) bool {
		return strings.Contains(yamlStr(m, "task"), "PublishTestResults@2") &&
			strings.Contains(yamlStr(m, "condition"), "always()")
	}) {
		t.Errorf("azure-pipelines.yml must publish test results with `condition: always()`")
	}
}

// decodeYAMLDoc reads and YAML-decodes a shipped pipeline file, FAILING if it is
// not well-formed YAML (the well-formedness half of PIP_LINT-01).
func decodeYAMLDoc(t *testing.T, path string) interface{} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not well-formed YAML: %v", path, err)
	}
	return doc
}

// anyMapping returns true if any mapping node reachable from node satisfies pred.
func anyMapping(node interface{}, pred func(map[string]interface{}) bool) bool {
	switch n := node.(type) {
	case map[string]interface{}:
		if pred(n) {
			return true
		}
		for _, v := range n {
			if anyMapping(v, pred) {
				return true
			}
		}
	case []interface{}:
		for _, v := range n {
			if anyMapping(v, pred) {
				return true
			}
		}
	}
	return false
}

// yamlStr returns m[k] as a string (empty if absent or non-string).
func yamlStr(m map[string]interface{}, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
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

// requireRemoteWorkflowCurrent is the shared pre-dispatch ordering guard (item 8):
// it fetches the ACTUAL registered workflow from the lab repo default branch and
// byte-compares it (sha256) to RegisterWorkflow's output. PIP-01/PIP-02 call it
// BEFORE dispatching, so a stale/hand-edited remote registration fails HERE rather
// than dispatching altered content — regardless of test execution order.
func requireRemoteWorkflowCurrent(ctx context.Context, t *testing.T, owner, repo, branch, token string) {
	t.Helper()
	workflowPath := envOr("LD_GH_LAB_WORKFLOW_PATH", ".github/workflows/labdeploy-e2e.yml")
	shipped, err := ReadShippedGitHubWorkflow()
	if err != nil {
		t.Fatalf("read shipped github-deploy.yml: %v", err)
	}
	want, err := RegisterWorkflow(shipped)
	if err != nil {
		t.Fatalf("RegisterWorkflow: %v", err)
	}
	got, err := FetchRemoteWorkflow(ctx, owner, repo, workflowPath, branch, token)
	if err != nil {
		t.Fatalf("pre-dispatch fetch of %s@%s: %v", workflowPath, branch, err)
	}
	if SHA256Hex(got) != SHA256Hex(want) {
		t.Fatalf("[ERR_STALE_WORKFLOW] remote %s@%s does not match the registered body; refusing to dispatch stale content:\n remote sha256    = %s\n expected sha256  = %s",
			workflowPath, branch, SHA256Hex(got), SHA256Hex(want))
	}
}

// TestPIP_REG_02_RemoteWorkflowMatchesGeneratedBody is the live pre-dispatch proof
// as an independent gate; it delegates to the same requireRemoteWorkflowCurrent
// guard PIP-01/PIP-02 run inline. Skips when the L4 lab is unavailable.
func TestPIP_REG_02_RemoteWorkflowMatchesGeneratedBody(t *testing.T) {
	ownerRepo := labEnv(t, "LD_GH_LAB_REPO")
	token := labEnv(t, "LD_GH_TOKEN")
	owner, repo := splitOwnerRepo(t, ownerRepo)
	branch := envOr("LD_GH_LAB_BRANCH", "main")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	requireRemoteWorkflowCurrent(ctx, t, owner, repo, branch, token)
}

// envOr returns the env value for key or def when unset.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
