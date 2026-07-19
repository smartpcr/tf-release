package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// dockerNode embeds a fakeHost and answers the §9.6 docker D-step scripts
// (pull/inspect/run/stop) against a tiny in-memory container state; all other
// scripts (connect/preflight/lock/manifest) delegate to the embedded fake.
type dockerNode struct {
	*fakeHost
	image         string   // current container image id ("" ⇒ container absent)
	runImage      string   // image id a normal (new-ref) docker run switches the container to
	rollbackImage string   // image id a rollback run (ref == this) switches the container to
	pullErr       bool
	runErr        bool     // docker run fails for BOTH the new ref and the rollback ref
	runScripts    []string // every `docker run` script, in order (new ref then rollback ref)
	inspectErr    bool     // ALL `docker inspect {{.Image}}` probes fail with a transport error
	inspectErrAfterRun bool // `docker inspect {{.Image}}` fails only AFTER a docker run has executed
	didRun        bool     // set once any docker run script has been dispatched
	removeScripts []string // standalone `docker rm -f` scripts (rollback name teardown), in order
	removeErr     bool     // standalone `docker rm -f` fails with a GENUINE (non-"No such") error
}

func (n *dockerNode) Exec(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	s := c.Script
	switch {
	case strings.Contains(s, "docker pull"):
		if n.pullErr {
			return transport.Result{ExitCode: 40, Stderr: "pull failed"}, nil
		}
		return ok(""), nil
	case strings.Contains(s, "docker inspect") && strings.Contains(s, "{{.Image}}"):
		// Simulate a genuine transport failure of the D3 inspect. inspectErr fails
		// the PRE-run probe (rollback anchor cannot be recorded); inspectErrAfterRun
		// fails only the POST-run probe (new container already running).
		if n.inspectErr || (n.inspectErrAfterRun && n.didRun) {
			return transport.Result{}, fmt.Errorf("transport: docker inspect unreachable")
		}
		return ok(n.image), nil
	case strings.Contains(s, "docker inspect") && strings.Contains(s, "{{.State.Running}}"):
		if n.image == "" {
			return ok("not_installed"), nil
		}
		return ok("running"), nil
	case strings.Contains(s, "docker run"):
		n.runScripts = append(n.runScripts, s)
		n.didRun = true
		if n.runErr {
			return transport.Result{ExitCode: 44, Stderr: "run failed"}, nil
		}
		// Reflect the image the container now runs: a rollback run (ref == the
		// recorded old image id) restores rollbackImage; a normal run switches to
		// runImage. This lets healthGate distinguish new-vs-restored by n.image.
		switch {
		case n.rollbackImage != "" && strings.Contains(s, n.rollbackImage):
			n.image = n.rollbackImage
		case n.runImage != "":
			n.image = n.runImage
		}
		return ok(""), nil
	case strings.Contains(s, "docker stop"):
		return ok(""), nil
	case strings.Contains(s, "docker rm -f"):
		// Standalone `docker rm -f` (rollback name teardown, or fresh-install
		// failure cleanup) — RunNew's combined rm+run script matched the `docker
		// run` case above, so this only fires for dc.Remove of a container.
		n.removeScripts = append(n.removeScripts, s)
		if n.removeErr {
			// Simulate the daemon REJECTING removal (not a "No such container"):
			// exit 21 is the pattern's genuine-failure sentinel.
			return transport.Result{ExitCode: 21, Stderr: "Error response from daemon: cannot remove container"}, nil
		}
		// A successful force-remove leaves no container behind.
		n.image = ""
		return ok(""), nil
	}
	return n.fakeHost.Exec(ctx, c)
}

// TestDockerRollbackFailedNoManifest covers evaluator iter6 item 2 / DESIGN
// §10.6: a docker update failure where an UNMANAGED old container exists
// (old image id present) but NO prior manifest does (prev==""), and the
// rollback to the old image also fails. The failed manifest must still be
// persisted (recorded at the ATTEMPTED version) and ERR_ROLLBACK_FAILED
// returned — the old bug passed empty prev to dockerFinalizeFailed which no-op'd.
func TestDockerRollbackFailedNoManifest(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "sha256:oldimg", runErr: true}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_ROLLBACK_FAILED" {
		t.Fatalf("want ERR_ROLLBACK_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "MACHINE IN UNKNOWN STATE host=lab-01") {
		t.Fatalf("want MACHINE IN UNKNOWN STATE host=lab-01 detail, got: %v", err)
	}
	m := string(n.fakeHost.files[`C:\deploy\sample-svc\manifest.json`])
	if m == "" {
		t.Fatalf("docker rollback failure with no prior manifest must still persist a failed manifest; none written (log=%v)", n.fakeHost.log)
	}
	if !strings.Contains(m, `"current_version": "2.0.0"`) || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("failed manifest must record the attempted version 2.0.0 with result=failed: %s", m)
	}
}

// dockerSpec is a minimal docker_container Deployment at the given version.
func dockerSpec(t *testing.T, version string) *spec.Deployment {
	t.Helper()
	t.Setenv("LABDEPLOY_PASSWORD", "pw")
	y := `
apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
  transport: winrm
  hosts: ["lab-01"]
  os: windows
  credentials: { username: u, password_env: LABDEPLOY_PASSWORD }
artifact:
  type: docker_image
  version: ` + version + `
  source: { type: docker_registry, image: registry.example.com/app, tag: "` + version + `" }
pattern:
  type: docker_container
  container_name: sample
health_check:
  type: http
  http: { url: "http://localhost:8080/health" }
  initial_delay_seconds: 1
  interval_seconds: 1
  timeout_seconds: 3
strategy: { keep_releases: 2, rollback_on_failure: true }
`
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}

const dockerManifestPath = `C:\deploy\sample-svc\manifest.json`

// TestDockerDeploySuccessRecordsImageID covers evaluator iter2 item 1 (positive
// half): a successful docker deploy must persist the post-run image id into
// manifest.extra.image_id (DESIGN §9.6 D3 record) so a later rollback has an
// anchor. The post-run inspect returns the NEW image id, which must land in the
// success manifest.
func TestDockerDeploySuccessRecordsImageID(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:newimg"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	if _, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0")); err != nil {
		t.Fatalf("docker deploy should succeed: %v", err)
	}
	m := string(n.fakeHost.files[dockerManifestPath])
	if m == "" {
		t.Fatalf("successful docker deploy must persist a manifest; none written (log=%v)", n.fakeHost.log)
	}
	if !strings.Contains(m, `"current_version": "2.0.0"`) || !strings.Contains(m, `"result": "success"`) {
		t.Fatalf("success manifest must record version 2.0.0 result=success: %s", m)
	}
	if !strings.Contains(m, `"image_id": "sha256:newimg"`) {
		t.Fatalf("success manifest must record the post-run image id in extra.image_id: %s", m)
	}
}

// TestDockerDeployEmptyImageIDFailsClosed covers evaluator iter2 item 1 (the
// durability half): if the post-run inspect yields an EMPTY image id the deploy
// is not durably recorded (no rollback anchor), so the engine must fail closed —
// NOT report success and persist a manifest with an empty image_id.
func TestDockerDeployEmptyImageIDFailsClosed(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: ""} // run ok, inspect stays ""
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	if err == nil {
		t.Fatal("docker deploy must fail closed when the post-run image id is empty")
	}
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_CONNECT" || ce.Step != "FINALIZE" {
		t.Fatalf("empty image id must surface a FINALIZE/ERR_CONNECT coded error, got %v", err)
	}
	if m := string(n.fakeHost.files[dockerManifestPath]); strings.Contains(m, `"result": "success"`) {
		t.Fatalf("must not persist a success manifest with an unrecorded image id: %s", m)
	}
}

// TestDockerHealthFailureRollsBackPrevVersion covers evaluator iter2 items 2 & 4:
// a health failure on the new version must roll back to the D3-recorded old image
// AND rebuild the container with the PREVIOUS release context, then recheck
// health and surface ERR_HEALTH_CHECK. The restored container must advertise the
// previous version (LD_VERSION=1.0.0), not the failed new version, and the
// rolled_back manifest is persisted at the previous version.
func TestDockerHealthFailureRollsBackPrevVersion(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:v1img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: land 1.0.0 so a prior manifest (previous version) exists.
	if _, err := eng.Deploy(context.Background(), dockerSpec(t, "1.0.0")); err != nil {
		t.Fatalf("phase-1 deploy 1.0.0 should succeed: %v", err)
	}

	// Phase 2: 2.0.0 runs, health fails while the NEW image is live, then passes
	// once the rollback restores the previous image.
	n.runImage = "sha256:v2img"
	n.rollbackImage = "sha256:v1img"
	n.runScripts = nil
	n.fakeHost.healthGate = func() bool { return n.image == "sha256:v2img" }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("health failure must surface ERR_HEALTH_CHECK, got %v", err)
	}

	// The rolled_back manifest records the PREVIOUS version and the old image id.
	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "rolled_back"`) {
		t.Fatalf("rollback must persist a rolled_back manifest at previous version 1.0.0: %s", m)
	}
	if !strings.Contains(m, `"image_id": "sha256:v1img"`) {
		t.Fatalf("rolled_back manifest must record the restored old image id: %s", m)
	}

	// Two run scripts in phase 2: the new-version run, then the rollback run.
	if len(n.runScripts) < 2 {
		t.Fatalf("expected a new-version run then a rollback run, got %d: %v", len(n.runScripts), n.runScripts)
	}
	var rollbackRun string
	for _, s := range n.runScripts {
		if strings.Contains(s, "sha256:v1img") {
			rollbackRun = s
		}
	}
	if rollbackRun == "" {
		t.Fatalf("no rollback run against the recorded old image id sha256:v1img: %v", n.runScripts)
	}
	// Item 4: the restored container must advertise the PREVIOUS version.
	if !strings.Contains(rollbackRun, "LD_VERSION=1.0.0") {
		t.Errorf("rollback run must advertise the previous version LD_VERSION=1.0.0:\n%s", rollbackRun)
	}
	if strings.Contains(rollbackRun, "LD_VERSION=2.0.0") {
		t.Errorf("rollback run must NOT advertise the failed new version LD_VERSION=2.0.0:\n%s", rollbackRun)
	}
	// The new-version run advertised 2.0.0 (sanity).
	if !strings.Contains(n.runScripts[0], "LD_VERSION=2.0.0") {
		t.Errorf("new-version run must advertise LD_VERSION=2.0.0:\n%s", n.runScripts[0])
	}
}

// dockerSpecPorts is dockerSpec plus explicit container ports, letting a test
// mutate the container CONFIGURATION at a fixed version (evaluator iter3 item 3).
func dockerSpecPorts(t *testing.T, version string, ports ...string) *spec.Deployment {
	t.Helper()
	d := dockerSpec(t, version)
	d.Pattern.Ports = ports
	return d
}

// TestDockerPreRunInspectErrorAbortsBeforeRemoval covers evaluator iter3 item 1:
// when the PRE-run D3 inspect fails with a transport error, the engine must abort
// BEFORE D4 `rm -f`/`docker run` — removing the live container without a recorded
// rollback image would strand the target. No run script must execute and no
// success manifest may be written.
func TestDockerPreRunInspectErrorAbortsBeforeRemoval(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "sha256:live", inspectErr: true}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	if err == nil {
		t.Fatal("a failed pre-run D3 inspect must abort the deploy")
	}
	if len(n.runScripts) != 0 {
		t.Fatalf("no container may be removed/run when the rollback image could not be recorded; runScripts=%v", n.runScripts)
	}
	if m := string(n.fakeHost.files[dockerManifestPath]); strings.Contains(m, `"result": "success"`) {
		t.Fatalf("must not persist a success manifest after a pre-run inspect failure: %s", m)
	}
}

// TestDockerPostRunInspectFailurePersistsNewVersionFailedState covers evaluator
// iter3 item 2: when the POST-run inspect fails, the new container is ALREADY
// running, so the engine must not simply error while Terraform still points at
// the old version. It must persist a FAILED manifest at the NEW version so Read
// reports drift and a re-apply reconverges, and surface a coded FINALIZE error.
func TestDockerPostRunInspectFailurePersistsNewVersionFailedState(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:newimg", inspectErrAfterRun: true}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_CONNECT" || ce.Step != "FINALIZE" {
		t.Fatalf("post-run inspect failure must surface a FINALIZE/ERR_CONNECT coded error, got %v", err)
	}
	if len(n.runScripts) == 0 {
		t.Fatalf("the new container must have been started before the post-run inspect: runScripts=%v", n.runScripts)
	}
	m := string(n.fakeHost.files[dockerManifestPath])
	if m == "" {
		t.Fatalf("a post-run inspect failure must persist a failed manifest for reconvergence; none written (log=%v)", n.fakeHost.log)
	}
	if !strings.Contains(m, `"current_version": "2.0.0"`) || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("failed manifest must record the NEW version 2.0.0 with result=failed (so Read shows drift): %s", m)
	}
	if strings.Contains(m, `"result": "success"`) {
		t.Fatalf("must not record success when the image id could not be inspected: %s", m)
	}
}

// TestDockerSameVersionConfigChangeRedeploys covers evaluator iter3 item 3: a
// same-version change to the container configuration (here: ports) must NOT be a
// silent no-op. Idempotency is keyed off ConfigFingerprint, so an identical spec
// re-applies as a no-op while a changed spec triggers a fresh docker run.
func TestDockerSameVersionConfigChangeRedeploys(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: initial deploy of 2.0.0 with a single port.
	if _, err := eng.Deploy(context.Background(), dockerSpecPorts(t, "2.0.0", "8080:8080")); err != nil {
		t.Fatalf("phase-1 deploy should succeed: %v", err)
	}
	afterFirst := len(n.runScripts)
	if afterFirst == 0 {
		t.Fatalf("phase-1 must have run the container")
	}

	// Phase 2: identical spec ⇒ true no-op (same version, same fingerprint, running).
	if _, err := eng.Deploy(context.Background(), dockerSpecPorts(t, "2.0.0", "8080:8080")); err != nil {
		t.Fatalf("phase-2 identical re-apply should succeed: %v", err)
	}
	if len(n.runScripts) != afterFirst {
		t.Fatalf("identical same-version re-apply must be a no-op (no new docker run); runs %d→%d", afterFirst, len(n.runScripts))
	}

	// Phase 3: same version but a CHANGED port ⇒ different fingerprint ⇒ redeploy.
	if _, err := eng.Deploy(context.Background(), dockerSpecPorts(t, "2.0.0", "8080:8080", "9090:9090")); err != nil {
		t.Fatalf("phase-3 config-change re-apply should succeed: %v", err)
	}
	if len(n.runScripts) <= afterFirst {
		t.Fatalf("a same-version CONFIG change must redeploy (new docker run), but runs stayed %d", len(n.runScripts))
	}
}

// TestDockerRollbackRestoresPriorConfig covers evaluator iter5 item 2: when an
// update changes BOTH the version AND the container configuration (ports/env/…)
// and then fails health, the rollback must restore the old image with the OLD
// configuration recorded in the prior manifest's docker_config snapshot — not
// the rejected new ports/env. This matches Terraform's retained prior state.
func TestDockerRollbackRestoresPriorConfig(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:v1img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: land 1.0.0 with port 8080 and env FOO=old.
	s1 := dockerSpec(t, "1.0.0")
	s1.Pattern.Ports = []string{"8080:8080"}
	s1.Environment = map[string]string{"FOO": "old"}
	if _, err := eng.Deploy(context.Background(), s1); err != nil {
		t.Fatalf("phase-1 deploy 1.0.0 should succeed: %v", err)
	}

	// Phase 2: 2.0.0 changes BOTH the version AND the config (port 9090, FOO=new);
	// health fails on the new image, forcing a rollback to the previous release.
	n.runImage = "sha256:v2img"
	n.rollbackImage = "sha256:v1img"
	n.runScripts = nil
	n.fakeHost.healthGate = func() bool { return n.image == "sha256:v2img" }

	s2 := dockerSpec(t, "2.0.0")
	s2.Pattern.Ports = []string{"9090:9090"}
	s2.Environment = map[string]string{"FOO": "new"}
	_, err := eng.Deploy(context.Background(), s2)
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("health failure must surface ERR_HEALTH_CHECK, got %v", err)
	}

	var rollbackRun, newRun string
	for _, s := range n.runScripts {
		if strings.Contains(s, "sha256:v1img") {
			rollbackRun = s // rollback runs the OLD image id explicitly
		} else if strings.Contains(s, "LD_VERSION=2.0.0") {
			newRun = s // the failed new-version run (image ref, not id)
		}
	}
	if rollbackRun == "" {
		t.Fatalf("no rollback run against the old image id sha256:v1img: %v", n.runScripts)
	}
	// The rollback must restore the PRIOR configuration, not the rejected new one.
	if !strings.Contains(rollbackRun, "-p '8080:8080'") {
		t.Errorf("rollback must restore the PRIOR port 8080:\n%s", rollbackRun)
	}
	if strings.Contains(rollbackRun, "-p '9090:9090'") {
		t.Errorf("rollback must NOT apply the rejected new port 9090:\n%s", rollbackRun)
	}
	if !strings.Contains(rollbackRun, "-e 'FOO=old'") {
		t.Errorf("rollback must restore the PRIOR env FOO=old:\n%s", rollbackRun)
	}
	if strings.Contains(rollbackRun, "-e 'FOO=new'") {
		t.Errorf("rollback must NOT apply the rejected new env FOO=new:\n%s", rollbackRun)
	}
	if !strings.Contains(rollbackRun, "LD_VERSION=1.0.0") {
		t.Errorf("rollback must advertise the previous version LD_VERSION=1.0.0:\n%s", rollbackRun)
	}
	// Sanity: the failed new-version run used the NEW config.
	if newRun == "" || !strings.Contains(newRun, "-p '9090:9090'") || !strings.Contains(newRun, "-e 'FOO=new'") {
		t.Errorf("the new-version run should have used the new port 9090 and FOO=new:\n%s", newRun)
	}

	// The rolled_back manifest records the previous version and its config snapshot.
	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "rolled_back"`) {
		t.Fatalf("rollback must persist a rolled_back manifest at 1.0.0: %s", m)
	}
	if !strings.Contains(m, `8080:8080`) || strings.Contains(m, `9090:9090`) {
		t.Fatalf("rolled_back manifest snapshot must record the restored prior port 8080, not 9090: %s", m)
	}
}

// TestDockerRollbackReplaysPriorHealthCheck covers evaluator iter7 item 1 /
// DESIGN §10.2: when a failed update also CHANGES the health probe, the rollback
// must re-check HEALTH(prev) — the probe the restored release was validated with
// — not the rejected desired probe. Here the NEW health URL always fails; a
// rollback that (incorrectly) re-probed the desired URL would report
// ERR_ROLLBACK_FAILED even though the old container came back healthy. The prior
// URL passes, so the correct behavior is ERR_HEALTH_CHECK + a rolled_back manifest.
func TestDockerRollbackReplaysPriorHealthCheck(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:v1img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: land 1.0.0 with the ORIGINAL health URL (/health).
	if _, err := eng.Deploy(context.Background(), dockerSpec(t, "1.0.0")); err != nil {
		t.Fatalf("phase-1 deploy 1.0.0 should succeed: %v", err)
	}

	// Phase 2: 2.0.0 changes the health URL to /newhealth, which ALWAYS fails.
	// The new-version probe fails (forcing rollback); the rollback must re-probe
	// the PRIOR /health (which passes), not /newhealth.
	n.runImage = "sha256:v2img"
	n.rollbackImage = "sha256:v1img"
	n.runScripts = nil
	n.fakeHost.healthScriptGate = func(script string) bool { return strings.Contains(script, "/newhealth") }

	s2 := dockerSpec(t, "2.0.0")
	s2.HealthCheck.HTTP.URL = "http://localhost:8080/newhealth"
	_, err := eng.Deploy(context.Background(), s2)
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("a changed health URL must not break rollback: want ERR_HEALTH_CHECK (rollback probes prior /health), got %v", err)
	}
	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "rolled_back"`) {
		t.Fatalf("rollback re-checking HEALTH(prev) must succeed and persist a rolled_back manifest at 1.0.0: %s", m)
	}
}

// TestDockerRollbackRenamedContainerRemovesFailed covers evaluator iter7 item 2:
// when a failed update also RENAMES the container, the failed container (started
// under the NEW name) must be torn down before the old container is restored
// under the PRIOR name — otherwise both run — and the rolled_back manifest must
// record the restored PRIOR container name, not the rejected new one.
func TestDockerRollbackRenamedContainerRemovesFailed(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:v1img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: land 1.0.0 under container name sample-old.
	s1 := dockerSpec(t, "1.0.0")
	s1.Pattern.ContainerName = "sample-old"
	if _, err := eng.Deploy(context.Background(), s1); err != nil {
		t.Fatalf("phase-1 deploy 1.0.0 should succeed: %v", err)
	}

	// Phase 2: 2.0.0 renames the container to sample-new; health fails on the new
	// image, forcing a rollback to sample-old.
	n.runImage = "sha256:v2img"
	n.rollbackImage = "sha256:v1img"
	n.runScripts = nil
	n.removeScripts = nil
	n.fakeHost.healthGate = func() bool { return n.image == "sha256:v2img" }

	s2 := dockerSpec(t, "2.0.0")
	s2.Pattern.ContainerName = "sample-new"
	_, err := eng.Deploy(context.Background(), s2)
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("health failure must surface ERR_HEALTH_CHECK, got %v", err)
	}

	// The failed container started under the NEW name must be explicitly removed.
	var removedNew bool
	for _, s := range n.removeScripts {
		if strings.Contains(s, "sample-new") {
			removedNew = true
		}
	}
	if !removedNew {
		t.Fatalf("rollback must remove the failed container under the renamed name sample-new: %v", n.removeScripts)
	}

	// The restore must run the OLD image under the PRIOR name sample-old.
	var rollbackRun string
	for _, s := range n.runScripts {
		if strings.Contains(s, "sha256:v1img") {
			rollbackRun = s
		}
	}
	if rollbackRun == "" || !strings.Contains(rollbackRun, "--name 'sample-old'") {
		t.Fatalf("rollback must restore the container under the PRIOR name sample-old:\n%s", rollbackRun)
	}

	// The rolled_back manifest must record the restored PRIOR name, not sample-new.
	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"docker://sample-old"`) || strings.Contains(m, `"docker://sample-new"`) {
		t.Fatalf("rolled_back manifest CurrentRelease must be the prior name docker://sample-old, not the desired name: %s", m)
	}
}

// TestDockerRollbackRenameCleanupFailureAborts covers evaluator iter8 item 1:
// when the failed container was started under a CHANGED name and its `docker rm
// -f` teardown fails for a GENUINE reason (daemon rejects removal — NOT "No such
// container"), the engine must NOT restore the old container alongside the still-
// running failed one and must NOT report rolled_back. It must surface
// ERR_ROLLBACK_FAILED (MACHINE IN UNKNOWN STATE) and never issue the restore run.
func TestDockerRollbackRenameCleanupFailureAborts(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:v1img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: land 1.0.0 under sample-old.
	s1 := dockerSpec(t, "1.0.0")
	s1.Pattern.ContainerName = "sample-old"
	if _, err := eng.Deploy(context.Background(), s1); err != nil {
		t.Fatalf("phase-1 deploy 1.0.0 should succeed: %v", err)
	}

	// Phase 2: rename to sample-new; health fails, so rollback starts — but the
	// teardown of the failed sample-new container is REJECTED by the daemon.
	n.runImage = "sha256:v2img"
	n.rollbackImage = "sha256:v1img"
	n.runScripts = nil
	n.removeScripts = nil
	n.removeErr = true
	n.fakeHost.healthGate = func() bool { return n.image == "sha256:v2img" }

	s2 := dockerSpec(t, "2.0.0")
	s2.Pattern.ContainerName = "sample-new"
	_, err := eng.Deploy(context.Background(), s2)
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_ROLLBACK_FAILED" {
		t.Fatalf("a genuine rename-cleanup removal failure must surface ERR_ROLLBACK_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "MACHINE IN UNKNOWN STATE host=lab-01") {
		t.Fatalf("rollback cleanup failure must report MACHINE IN UNKNOWN STATE: %v", err)
	}
	// The restore run must NOT have executed — restoring next to the un-removed
	// failed container is exactly what we must avoid.
	for _, s := range n.runScripts {
		if strings.Contains(s, "sha256:v1img") {
			t.Fatalf("rollback must NOT restore the old image while the failed container removal failed:\n%s", s)
		}
	}
	// The engine must NOT record a rolled_back state.
	m := string(n.fakeHost.files[dockerManifestPath])
	if strings.Contains(m, `"result": "rolled_back"`) {
		t.Fatalf("a failed rename cleanup must not be recorded as rolled_back: %s", m)
	}
	if !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("a failed rollback must persist a failed manifest: %s", m)
	}
}

// TestDockerFailedManifestPreservesRollbackMetadata covers evaluator iter7 item
// 3: a failed-operation manifest must PRESERVE the prior docker_config snapshot
// and config_hash so a later repair attempt can still restore the complete prior
// configuration. Here rollback is disabled and health fails, so the engine writes
// a failed manifest at the previous version — it must carry the prior config
// (port 8080), not strip it down to only image_id.
func TestDockerFailedManifestPreservesRollbackMetadata(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:v1img"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Phase 1: land 1.0.0 with port 8080 so the manifest carries a docker_config.
	s1 := dockerSpec(t, "1.0.0")
	s1.Pattern.Ports = []string{"8080:8080"}
	if _, err := eng.Deploy(context.Background(), s1); err != nil {
		t.Fatalf("phase-1 deploy 1.0.0 should succeed: %v", err)
	}

	// Phase 2: 2.0.0 changes the port to 9090 with rollback DISABLED; health
	// always fails, so the engine records a failed manifest at 1.0.0.
	n.runImage = "sha256:v2img"
	n.runScripts = nil
	n.fakeHost.healthGate = func() bool { return true }
	s2 := dockerSpec(t, "2.0.0")
	s2.Pattern.Ports = []string{"9090:9090"}
	no := false
	s2.Strategy.RollbackOnFailure = &no
	if _, err := eng.Deploy(context.Background(), s2); err == nil {
		t.Fatal("health failure with rollback disabled must return an error")
	}

	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"current_version": "1.0.0"`) || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("failed manifest must be recorded at the prior version 1.0.0: %s", m)
	}
	// The prior docker_config (port 8080) and config_hash must survive so a repair
	// can still restore the complete prior configuration.
	if !strings.Contains(m, `8080:8080`) {
		t.Fatalf("failed manifest must PRESERVE the prior docker_config (port 8080): %s", m)
	}
	if strings.Contains(m, `9090:9090`) {
		t.Fatalf("failed manifest must not overwrite the prior config with the rejected port 9090: %s", m)
	}
	if !strings.Contains(m, `"config_hash"`) {
		t.Fatalf("failed manifest must preserve the prior config_hash: %s", m)
	}
}

// TestDockerFreshHealthFailureRemovesContainer covers DESIGN §10.2's
// fresh-install row (rollback ENABLED, the default): a FRESH install (no prior
// image to roll back to) whose D5 `docker run` succeeds but whose HEALTH check
// then fails must NOT leave the rejected container running. The engine must tear
// it down (`docker rm -f`) so the machine is left clean AND leave the manifest
// ABSENT ("machine clean, apply error, no TF state") so Read yields
// RemoveResource and the next plan re-creates. This test would FAIL if the
// rejected container remained running (n.image stays set) OR if a failed manifest
// were persisted (evaluator iter15 items 1/2).
func TestDockerFreshHealthFailureRemovesContainer(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:freshimg"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	// Fresh install (no prior manifest ⇒ oldImage==""): D5 run succeeds, health
	// always fails. There is nothing to roll back to, so the fresh container must
	// be removed rather than left running.
	n.fakeHost.healthGate = func() bool { return true }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("fresh health failure must surface ERR_HEALTH_CHECK, got %v", err)
	}
	// The fresh container must have been force-removed (machine left clean).
	if len(n.removeScripts) == 0 {
		t.Fatalf("fresh-install failure must `docker rm -f` the rejected container; no removal issued (log=%v)", n.fakeHost.log)
	}
	if !strings.Contains(n.removeScripts[len(n.removeScripts)-1], "'sample'") {
		t.Fatalf("cleanup must remove the desired container name 'sample': %v", n.removeScripts)
	}
	if n.image != "" {
		t.Fatalf("rejected fresh container must not remain running after cleanup; image=%q", n.image)
	}
	// §10.2: with rollback enabled the machine is left CLEAN with NO TF state —
	// the manifest MUST be absent (not a persisted failed manifest).
	if m := string(n.fakeHost.files[dockerManifestPath]); m != "" {
		t.Fatalf("fresh failure with rollback enabled must leave NO manifest (machine clean, no TF state); found: %s", m)
	}
}

// TestDockerFreshHealthFailureRollbackDisabledKeepsContainer covers DESIGN
// §10.2's "any, rollback_on_failure=false" row for a FRESH install: the engine
// must take NO compensating action — the rejected container is LEFT running for
// operator inspection — and a failed manifest is persisted at the attempted
// version so Read reports drift. This is the opposite of the rollback-enabled
// fresh case above and would FAIL if the engine force-removed the container or
// skipped the failed manifest (evaluator iter15 items 3/4).
func TestDockerFreshHealthFailureRollbackDisabledKeepsContainer(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:freshimg"}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	n.fakeHost.healthGate = func() bool { return true }
	s := dockerSpec(t, "2.0.0")
	no := false
	s.Strategy.RollbackOnFailure = &no

	_, err := eng.Deploy(context.Background(), s)
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_HEALTH_CHECK" {
		t.Fatalf("fresh health failure must surface ERR_HEALTH_CHECK, got %v", err)
	}
	// rollback_on_failure=false ⇒ NO compensating action: the rejected container
	// must be LEFT running (still holding the fresh image) for inspection.
	if len(n.removeScripts) != 0 {
		t.Fatalf("rollback-disabled fresh failure must NOT `docker rm -f` the rejected container; removals issued: %v", n.removeScripts)
	}
	if n.image != "sha256:freshimg" {
		t.Fatalf("rejected fresh container must REMAIN running (image intact) when rollback disabled; image=%q", n.image)
	}
	// A failed manifest at the attempted version must be persisted so Read drifts.
	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"current_version": "2.0.0"`) || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("rollback-disabled fresh failure must persist a failed manifest at attempted version 2.0.0: %s", m)
	}
}

// TestDockerFreshHealthFailureCleanupFailureAborts is the negative half of iter12
// item 1: when the fresh-install cleanup `docker rm -f` itself GENUINELY fails
// (daemon rejects removal), the machine is NOT clean — a rejected container may
// still be running — so the engine must surface ERR_ROLLBACK_FAILED / MACHINE IN
// UNKNOWN STATE. Per DESIGN §10.6, that unknown state must ALSO persist a failed
// manifest (last_operation={type:deploy,result:failed}) at the attempted version
// so Read reports drift and a re-apply attempts a repairing deploy (iter17 items 1/2).
func TestDockerFreshHealthFailureCleanupFailureAborts(t *testing.T) {
	n := &dockerNode{fakeHost: newFakeHost("lab-01"), image: "", runImage: "sha256:freshimg", removeErr: true}
	eng := New()
	eng.NewTransport = func(tg *spec.Target, host string) (transport.Transport, error) { return n, nil }

	n.fakeHost.healthGate = func() bool { return true }

	_, err := eng.Deploy(context.Background(), dockerSpec(t, "2.0.0"))
	var ce *CodedError
	if !asCoded(err, &ce) || ce.Code != "ERR_ROLLBACK_FAILED" {
		t.Fatalf("a failed fresh-install cleanup must surface ERR_ROLLBACK_FAILED, got %v", err)
	}
	if !strings.Contains(err.Error(), "MACHINE IN UNKNOWN STATE host=lab-01") {
		t.Fatalf("cleanup failure must report MACHINE IN UNKNOWN STATE: %v", err)
	}
	if len(n.removeScripts) == 0 {
		t.Fatalf("cleanup must have ATTEMPTED a `docker rm -f`; none issued (log=%v)", n.fakeHost.log)
	}
	// §10.6: ERR_ROLLBACK_FAILED MUST persist a failed manifest at the attempted
	// version so the unknown state surfaces as drift on the next Read.
	m := string(n.fakeHost.files[dockerManifestPath])
	if !strings.Contains(m, `"current_version": "2.0.0"`) || !strings.Contains(m, `"result": "failed"`) {
		t.Fatalf("§10.6: cleanup-failure unknown state must persist a failed manifest at attempted version 2.0.0: %s", m)
	}
}


