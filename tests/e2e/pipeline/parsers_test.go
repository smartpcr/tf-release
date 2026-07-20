package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ghLog builds a synthetic GitHub raw job log: each line carries the RFC3339
// timestamp prefix GitHub emits, so the parser's prefix-stripping is exercised.
func ghLog(lines ...string) string {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	for i, l := range lines {
		b.WriteString(ts.Add(time.Duration(i) * time.Millisecond).Format("2006-01-02T15:04:05.0000000Z"))
		b.WriteString(" ")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}

// The logged script source: GitHub prints the run: block before executing it. These
// lines contain the sentinels only INSIDE `echo "..."`, never as bare lines.
var summaryScriptEcho = []string{
	`##[group]Run {`,
	`  echo "### labdeploy test summary"`,
	`  echo "LABDEPLOY_SUMMARY_BEGIN"`,
	`  terraform output -raw test_summary`,
	`  echo ""`,
	`  echo "LABDEPLOY_SUMMARY_END"`,
	`##[endgroup]`,
}

func TestParseJobSummaryEcho_AcceptsRealExecutedBlock(t *testing.T) {
	log := ghLog(append(append([]string{}, summaryScriptEcho...),
		"### labdeploy test summary",
		"LABDEPLOY_SUMMARY_BEGIN",
		"Total: 3, Passed: 3, Failed: 0",
		"",
		"LABDEPLOY_SUMMARY_END",
	)...)

	got, err := parseJobSummaryEcho(log)
	if err != nil {
		t.Fatalf("expected the real executed block to parse, got error: %v", err)
	}
	if got != "Total: 3, Passed: 3, Failed: 0" {
		t.Fatalf("payload = %q, want the echoed terraform summary", got)
	}
	if !containsBareLine(log, summaryMarker) {
		t.Fatalf("containsBareLine should find the executed %q heading", summaryMarker)
	}
}

func TestParseJobSummaryEcho_RejectsScriptOnlyLog(t *testing.T) {
	// The exact false-positive the evaluator flagged: the log contains ONLY the
	// logged script (the sentinels appear inside echo "..."), and `terraform output`
	// produced nothing, so there is no executed block. The old first-marker parser
	// accepted this; the new parser must reject it.
	log := ghLog(summaryScriptEcho...)
	if _, err := parseJobSummaryEcho(log); err == nil {
		t.Fatalf("expected an error when only the logged script is present (no executed summary block)")
	}
	if containsBareLine(log, summaryMarker) {
		t.Fatalf("containsBareLine must NOT match the marker embedded in a logged echo line")
	}
}

func TestParseJobSummaryEcho_RejectsEmptyPayload(t *testing.T) {
	// terraform output produced nothing: BEGIN immediately followed by a blank line
	// and END. Must be rejected — an empty summary does not satisfy the contract.
	log := ghLog(append(append([]string{}, summaryScriptEcho...),
		"### labdeploy test summary",
		"LABDEPLOY_SUMMARY_BEGIN",
		"",
		"LABDEPLOY_SUMMARY_END",
	)...)
	if _, err := parseJobSummaryEcho(log); err == nil {
		t.Fatalf("expected an error for an empty summary payload")
	}
}

func TestParseJobSummaryEcho_RejectsUnterminatedBlock(t *testing.T) {
	log := ghLog(append(append([]string{}, summaryScriptEcho...),
		"LABDEPLOY_SUMMARY_BEGIN",
		"Total: 3, Passed: 3, Failed: 0",
	)...)
	if _, err := parseJobSummaryEcho(log); err == nil {
		t.Fatalf("expected an error for an unterminated summary block")
	}
}

func TestSelectRunCandidates(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	runs := []workflowRun{
		{ID: 10, Event: "workflow_dispatch", HeadBranch: "main", CreatedAt: base.Format(time.RFC3339)},                     // too old (id<=afterID)
		{ID: 21, Event: "push", HeadBranch: "main", CreatedAt: base.Format(time.RFC3339)},                                  // wrong event
		{ID: 22, Event: "workflow_dispatch", HeadBranch: "feature", CreatedAt: base.Format(time.RFC3339)},                  // wrong branch
		{ID: 23, Event: "workflow_dispatch", HeadBranch: "main", CreatedAt: base.Add(-time.Hour).Format(time.RFC3339)},     // before sinceFloor
		{ID: 24, Event: "workflow_dispatch", HeadBranch: "refs/heads/main", CreatedAt: base.Add(time.Minute).Format(time.RFC3339)}, // MATCH (normRef)
	}
	runs[4].Actor.Login = "ci-bot"
	// A run by another actor must be excluded.
	other := workflowRun{ID: 25, Event: "workflow_dispatch", HeadBranch: "main", CreatedAt: base.Add(time.Minute).Format(time.RFC3339)}
	other.Actor.Login = "someone-else"
	runs = append(runs, other)

	got := selectRunCandidates(runs, 20, "main", "ci-bot", base.Add(-time.Minute))
	if len(got) != 1 || got[0] != 24 {
		t.Fatalf("selectRunCandidates = %v, want [24] (only the id>20, dispatch, main, ci-bot, recent run)", got)
	}

	// Two same-actor candidates → both returned so FindRunAfter can flag ambiguity.
	runs[5].Actor.Login = "ci-bot"
	got = selectRunCandidates(runs, 20, "main", "ci-bot", base.Add(-time.Minute))
	if len(got) != 2 {
		t.Fatalf("selectRunCandidates = %v, want two ambiguous candidates", got)
	}
}

func TestFindRunAfter_Stabilizes(t *testing.T) {
	prev := findRunStabilizeInterval
	findRunStabilizeInterval = time.Millisecond
	defer func() { findRunStabilizeInterval = prev }()

	body := func(ids ...int64) string {
		var runs []map[string]any
		for _, id := range ids {
			runs = append(runs, map[string]any{
				"id": id, "event": "workflow_dispatch", "head_branch": "main",
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"actor":      map[string]any{"login": "ci-bot"},
			})
		}
		b, _ := json.Marshal(map[string]any{"workflow_runs": runs})
		return string(b)
	}

	// Single stable candidate across polls → returned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body(42)))
	}))
	defer srv.Close()
	c := NewGitHubClient("o", "r", "tok")
	c.apiBase = srv.URL
	id, err := c.FindRunAfter(context.Background(), "wf.yml", "main", 40, time.Now().Add(-time.Minute), "ci-bot", 5*time.Second)
	if err != nil || id != 42 {
		t.Fatalf("FindRunAfter stable = (%d,%v), want (42,nil)", id, err)
	}

	// Two same-actor candidates → ambiguity error, no silent smallest-id pick.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body(42, 43)))
	}))
	defer srv2.Close()
	c2 := NewGitHubClient("o", "r", "tok")
	c2.apiBase = srv2.URL
	if _, err := c2.FindRunAfter(context.Background(), "wf.yml", "main", 40, time.Now().Add(-time.Minute), "ci-bot", 5*time.Second); err == nil ||
		!strings.Contains(err.Error(), "ERR_RUN_AMBIGUOUS") {
		t.Fatalf("FindRunAfter ambiguous err = %v, want [ERR_RUN_AMBIGUOUS]", err)
	}
}

func TestGetBuildDefinition_ParsesIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/_apis/build/definitions/77") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{
			"process": { "yamlFilename": "examples/pipelines/azure-pipelines.yml" },
			"repository": { "name": "tf-release", "type": "TfsGit", "defaultBranch": "refs/heads/main" }
		}`))
	}))
	defer srv.Close()

	ado := NewADOClient("org", "proj", "pat")
	ado.baseURL = srv.URL
	def, err := ado.GetBuildDefinition(context.Background(), 77)
	if err != nil {
		t.Fatalf("GetBuildDefinition: %v", err)
	}
	if def.YamlFilename != "examples/pipelines/azure-pipelines.yml" ||
		def.RepoName != "tf-release" || def.RepoType != "TfsGit" || def.DefaultBranch != "refs/heads/main" {
		t.Fatalf("GetBuildDefinition parsed %+v, want the canned identity fields", def)
	}
}
