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
	image   string // current container image id ("" ⇒ container absent)
	pullErr bool
	runErr  bool // docker run fails for BOTH the new ref and the rollback ref
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
		if n.runErr {
			return transport.Result{ExitCode: 44, Stderr: "run failed"}, nil
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
