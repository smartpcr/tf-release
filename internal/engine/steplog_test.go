package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// ----------------------------------------------------------------------------
// Structured step-logging tests (DESIGN §8.5 / §15). Every fixed step is
// emitted through tflog carrying app/host/step/version and a numeric
// duration_ms; conditional steps are skipped, per-host/repeated steps still
// carry the full field set.
// ----------------------------------------------------------------------------

// stepEntry is one decoded "deploy step" tflog record.
type stepEntry struct {
	step    string
	app     string
	host    string
	version string
	durOK   bool
}

// captureSteps decodes the tflog JSON sink and returns the "deploy step"
// records in emission order, asserting the mandatory field set on each.
func captureSteps(t *testing.T, buf *bytes.Buffer) []stepEntry {
	t.Helper()
	entries, err := tflogtest.MultilineJSONDecode(buf)
	if err != nil {
		t.Fatalf("decode tflog sink: %v\nraw=%s", err, buf.String())
	}
	var out []stepEntry
	for _, e := range entries {
		if e["@message"] != "deploy step" {
			continue
		}
		se := stepEntry{}
		for _, k := range []string{"app", "host", "step", "version", "duration_ms"} {
			v, ok := e[k]
			if !ok || v == nil {
				t.Fatalf("step record missing %q field: %#v", k, e)
			}
		}
		se.step, _ = e["step"].(string)
		se.app, _ = e["app"].(string)
		se.host, _ = e["host"].(string)
		se.version, _ = e["version"].(string)
		// JSON numbers decode to float64; a numeric duration_ms is required.
		if _, ok := e["duration_ms"].(float64); ok {
			se.durOK = true
		} else if n, ok := e["duration_ms"].(json.Number); ok {
			_, ferr := n.Float64()
			se.durOK = ferr == nil
		}
		if !se.durOK {
			t.Fatalf("step %q duration_ms is not numeric: %#v (%T)", se.step, e["duration_ms"], e["duration_ms"])
		}
		out = append(out, se)
	}
	return out
}

func stepNames(steps []stepEntry) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.step
	}
	return out
}

func countStep(steps []stepEntry, name string) int {
	n := 0
	for _, s := range steps {
		if s.step == name {
			n++
		}
	}
	return n
}

// assertSubsequence checks that want appears as a relative-order subsequence of
// got (extra/repeated steps between the wanted ones are allowed).
func assertSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("step order does not contain subsequence\n want=%v\n got =%v (matched %d/%d)", want, got, i, len(want))
	}
}

// TestStepLogFreshSingleHostOrder — Scenario "Single-host step order logged".
func TestStepLogFreshSingleHostOrder(t *testing.T) {
	payload := []byte("fake zip bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	eng := engineWith(f)

	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	if _, err := eng.Deploy(ctx, winSvcSpec(t, url, sum)); err != nil {
		t.Fatalf("deploy: %v\nlog=%v", err, f.log)
	}

	steps := captureSteps(t, &buf)
	names := stepNames(steps)

	// Fresh single-host deploy executes every fixed step EXACTLY once, in this
	// exact emitted order. The staging steps follow the pinned fixed-step order
	// (implementation-plan.md:224 / DESIGN §8.5): FETCH → CHECKSUM → STAGE →
	// EXTRACT → RENDER. The switchover steps follow the normative state-machine
	// execution order (STOP → SWITCH → CONFIGURE → START, DESIGN §9.2 — see the
	// open question re: §8.5's CONFIGURE-before-STOP name-list order).
	want := []string{
		"VALIDATE", "CONNECT", "PREFLIGHT", "LOCK",
		"FETCH", "CHECKSUM", "STAGE", "EXTRACT", "RENDER",
		"STOP", "SWITCH", "CONFIGURE", "START", "HEALTH",
		"FINALIZE", "PRUNE", "UNLOCK",
	}
	// Exact sequence: no extra, missing, reordered, or duplicated steps.
	if len(names) != len(want) {
		t.Fatalf("expected exactly %d step records, got %d\n want=%v\n got =%v", len(want), len(names), want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("step order mismatch at index %d: want %q got %q\n want=%v\n got =%v", i, want[i], names[i], want, names)
		}
	}
	// Every fixed step occurs EXACTLY once (no duplicates).
	for _, w := range want {
		if c := countStep(steps, w); c != 1 {
			t.Fatalf("fixed step %q must be logged exactly once, got %d; names=%v", w, c, names)
		}
	}

	// Every record carries the correct app/host/version.
	for _, s := range steps {
		if s.app != "sample-svc" || s.host != "lab-01" || s.version != "1.0.0" {
			t.Fatalf("step %q has wrong fields app=%q host=%q version=%q", s.step, s.app, s.host, s.version)
		}
	}
}

// TestStepLogIdempotentNoOp — Scenario "Idempotent short-circuit": no FETCH or
// SWITCH step is logged when the version+checksum already match and the service
// is healthy.
func TestStepLogIdempotentNoOp(t *testing.T) {
	payload := []byte("fake zip bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	f.svc = "Running"
	// Seed a manifest whose version+checksum equal the spec (short-circuit).
	seedManifest(t, f, `C:\deploy\sample-svc\manifest.json`, &Manifest{
		Schema: 1, App: "sample-svc", Pattern: "windows_service",
		CurrentVersion: "1.0.0", ArtifactChecksum: sum,
		CurrentRelease:  `C:\deploy\sample-svc\releases\1.0.0`,
		ProviderVersion: ProviderVersion,
		LastOperation:   LastOp{Type: "deploy", Result: "success"},
	})
	eng := engineWith(f)

	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	st, err := eng.Deploy(ctx, winSvcSpec(t, url, sum))
	if err != nil {
		t.Fatalf("deploy: %v\nlog=%v", err, f.log)
	}
	if st.DeployedVersion != "1.0.0" {
		t.Fatalf("expected NO-OP at 1.0.0, got %+v", st)
	}

	steps := captureSteps(t, &buf)
	names := stepNames(steps)
	if countStep(steps, "FETCH") != 0 {
		t.Fatalf("idempotent no-op logged FETCH: %v", names)
	}
	if countStep(steps, "SWITCH") != 0 {
		t.Fatalf("idempotent no-op logged SWITCH: %v", names)
	}
	// The pre-switch gates still run and are still logged with full fields.
	for _, must := range []string{"VALIDATE", "CONNECT", "PREFLIGHT", "LOCK"} {
		if countStep(steps, must) == 0 {
			t.Fatalf("gate step %q missing on no-op: %v", must, names)
		}
	}
}

// TestStepLogUpdateCachedRollback — Scenario "Conditional and per-host steps
// logged": an update whose new release is CACHED skips FETCH/CHECKSUM, and a
// health failure drives a rollback that REPEATS the SWITCH step. Every emitted
// record (including the repeated SWITCH) still carries the full field set.
func TestStepLogUpdateCachedRollback(t *testing.T) {
	payload := []byte("v2 zip bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	f.svc = "Running" // an existing (previous) deployment is running
	// Previous deployment at 1.0.0 (update path: prev != "").
	seedManifest(t, f, `C:\deploy\sample-svc\manifest.json`, &Manifest{
		Schema: 1, App: "sample-svc", Pattern: "windows_service",
		CurrentVersion: "1.0.0", ArtifactChecksum: "sha256:old",
		CurrentRelease:  `C:\deploy\sample-svc\releases\1.0.0`,
		ProviderVersion: ProviderVersion,
		LastOperation:   LastOp{Type: "deploy", Result: "success"},
	})
	// New release 2.0.0 already fully extracted on the host ⇒ FETCH/CHECKSUM/
	// EXTRACT are skipped (DESIGN §10.5 cache hit).
	seedReleaseMarker(t, f, `C:\deploy\sample-svc\releases\2.0.0`, "2.0.0", sum)

	// New-version HEALTH fails (junction at 2.0.0), rollback HEALTH succeeds
	// once the junction is restored to 1.0.0. Gating on the junction target is
	// robust to health-check retries within a single RunHealthCheck window.
	f.healthGate = func() bool { return strings.Contains(f.current, "2.0.0") }

	eng := engineWith(f)
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	_, err := eng.Deploy(ctx, winSvcSpecVersion(t, url, sum, "2.0.0"))
	if err == nil {
		t.Fatalf("expected rolled-back deploy to return the original error; log=%v", f.log)
	}
	if !strings.Contains(err.Error(), "rolled back to 1.0.0") {
		t.Fatalf("expected rollback-to-previous error, got: %v", err)
	}

	steps := captureSteps(t, &buf)
	names := stepNames(steps)

	// Conditional steps skipped because the release was cached.
	if countStep(steps, "FETCH") != 0 || countStep(steps, "CHECKSUM") != 0 {
		t.Fatalf("cached update should not log FETCH/CHECKSUM: %v", names)
	}
	// SWITCH is executed twice: forward (to 2.0.0) then rollback (to 1.0.0).
	if got := countStep(steps, "SWITCH"); got != 2 {
		t.Fatalf("expected SWITCH twice (forward + rollback), got %d: %v", got, names)
	}
	// A ROLLBACK record must be present.
	if countStep(steps, "ROLLBACK") == 0 {
		t.Fatalf("expected a ROLLBACK step record: %v", names)
	}
	// Every emitted record — including the repeated SWITCH — carries the full
	// field set (captureSteps already fails otherwise) with correct app/host/
	// version (the operation's target version).
	switches := 0
	for _, s := range steps {
		if s.app != "sample-svc" || s.host != "lab-01" || s.version != "2.0.0" {
			t.Fatalf("step %q wrong app/host/version: %+v", s.step, s)
		}
		if s.step == "SWITCH" {
			switches++
		}
	}
	if switches != 2 {
		t.Fatalf("expected forward + rollback SWITCH, got %d: %v", switches, names)
	}
}

// TestStepLogForceKillEscalation — DESIGN §9.2 S2 / WSV-07: when a service
// ignores the graceful stop, the escalation must be observable as its OWN
// structured `FORCE_KILL` step record (with the full app/host/step/version/
// duration_ms field set), not merely tagged inside the wrapping STOP step or
// visible only on error. This drives a cached update whose graceful stop times
// out; the force-kill succeeds and the deploy completes, so the FORCE_KILL step
// is emitted on the SUCCESS path.
func TestStepLogForceKillEscalation(t *testing.T) {
	payload := []byte("v2 zip bytes")
	url, sum, done := testArtifactServer(t, payload)
	defer done()
	f := newFakeHost("lab-01")
	f.svc = "Running"          // an existing deployment is running
	f.fail["stopgrace"] = true // it ignores Stop-Service ⇒ force-kill escalation
	seedManifest(t, f, `C:\deploy\sample-svc\manifest.json`, &Manifest{
		Schema: 1, App: "sample-svc", Pattern: "windows_service",
		CurrentVersion: "1.0.0", ArtifactChecksum: "sha256:old",
		CurrentRelease:  `C:\deploy\sample-svc\releases\1.0.0`,
		ProviderVersion: ProviderVersion,
		LastOperation:   LastOp{Type: "deploy", Result: "success"},
	})
	seedReleaseMarker(t, f, `C:\deploy\sample-svc\releases\2.0.0`, "2.0.0", sum)

	eng := engineWith(f)
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	if _, err := eng.Deploy(ctx, winSvcSpecVersion(t, url, sum, "2.0.0")); err != nil {
		t.Fatalf("deploy with force-kill escalation should succeed: %v\nlog=%v", err, f.log)
	}

	steps := captureSteps(t, &buf)
	names := stepNames(steps)
	// The graceful STOP is still logged, AND a distinct FORCE_KILL step appears.
	if countStep(steps, "STOP") == 0 {
		t.Fatalf("expected a STOP step record: %v", names)
	}
	if countStep(steps, "FORCE_KILL") == 0 {
		t.Fatalf("successful force-kill escalation must emit a structured FORCE_KILL step (WSV-07): %v", names)
	}
	// The FORCE_KILL record must carry the full structured field set with the
	// operation's target version/host (captureSteps already fails on a missing
	// or non-numeric field).
	for _, se := range steps {
		if se.step != "FORCE_KILL" {
			continue
		}
		if se.app != "sample-svc" || se.host != "lab-01" || se.version != "2.0.0" {
			t.Fatalf("FORCE_KILL step wrong fields: %+v", se)
		}
	}
	// The simulated host actually executed the force-kill (taskkill) path.
	joined := strings.Join(f.log, ",")
	if !strings.Contains(joined, "FORCE_KILL") {
		t.Fatalf("force-kill script was not executed on the host: %v", f.log)
	}
}

// ---------------------------------------------------------------------------
// seed helpers
// ---------------------------------------------------------------------------

func seedManifest(t *testing.T, f *fakeHost, path string, m *Manifest) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	f.files[path] = b
}

// winSvcSpecVersion is winSvcSpec with an explicit artifact version (for
// update/rollback scenarios where a previous version is already deployed).
func winSvcSpecVersion(t *testing.T, url, checksum, version string) *spec.Deployment {
	t.Helper()
	d := winSvcSpec(t, url, checksum)
	d.Artifact.Version = version
	return d
}

func seedReleaseMarker(t *testing.T, f *fakeHost, releaseDir, version, checksum string) {
	t.Helper()
	f.dirs[releaseDir] = true
	marker := releaseDir + `\` + releaseMarker
	body := struct {
		Version     string `json:"version"`
		SHA256      string `json:"sha256"`
		ExtractedAt string `json:"extracted_at"`
	}{version, checksum, time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(body)
	f.files[marker] = b
}
