package engine

import (
	"context"
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
		return ok(n.image), nil
	case strings.Contains(s, "docker inspect") && strings.Contains(s, "{{.State.Running}}"):
		if n.image == "" {
			return ok("not_installed"), nil
		}
		return ok("running"), nil
	case strings.Contains(s, "docker run"):
		n.runScripts = append(n.runScripts, s)
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

