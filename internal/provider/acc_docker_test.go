package provider

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Stage 9.2 — docker_container acceptance against L1 (Linux VM, ssh, docker
// installed). The NEGATIVE docker path (ART-06: bad docker login ⇒ ERR_ARTIFACT_FETCH,
// container list unchanged) lives in acc_con_art_cap_test.go. This file adds the
// POSITIVE docker_container pattern proof (DOK-01): a real image pull + `docker run`
// on L1, asserting the container is running and its /health serves the version, and
// that destroy purges the container. Both together cover "docker_container
// acceptance (ART-06 and docker patterns) against L1".

const (
	envDockerImage    = "LABDEPLOY_ACC_DOCKER_IMAGE"    // registry image WITHOUT tag, e.g. registry.lab/sample-svc
	envDockerUser     = "LABDEPLOY_ACC_DOCKER_USER"     // optional registry username
	envDockerPassword = "LABDEPLOY_ACC_DOCKER_PW"       // optional registry password env
	envDockerHealth   = "LABDEPLOY_ACC_DOCKER_HEALTH"   // optional /health URL (default localhost:8080)
	dockerCtrName     = "sample-svc"
)

// dockerHealthURL returns the L1 container /health URL (env-overridable).
func dockerHealthURL() string {
	if u := os.Getenv(envDockerHealth); u != "" {
		return u
	}
	return "http://localhost:8080/health"
}

// dockerSpec builds a docker_container Deployment for L1. When registry credentials
// are provided they are wired via password_env (the spec references the NAME only,
// DESIGN §D6); otherwise the image is assumed public.
func dockerSpec(t *testing.T, at accTarget, version string) string {
	t.Helper()
	image := mustEnv(t, envDockerImage, "docker_container acceptance image")
	auth := ""
	if os.Getenv(envDockerPassword) != "" {
		user := os.Getenv(envDockerUser)
		if user == "" {
			user = "deploy"
		}
		auth = fmt.Sprintf("\n    auth: { username: %q, password_env: %s }", user, envDockerPassword)
	}
	return fmt.Sprintf(`apiVersion: labdeploy/v1
kind: Deployment
metadata: { name: sample-svc }
target:
%s
artifact:
  type: docker_image
  version: %q
  source:
    type: docker_registry
    image: %q%s
pattern:
  type: docker_container
  container_name: %s
  ports: ["8080:8080"]
  restart_policy: unless-stopped
health_check:
  type: http
  http: { url: %q }
`, at.yaml, version, image, auth, dockerCtrName, dockerHealthURL())
}

// checkContainerRunning asserts `docker inspect` reports the named container as
// running on the target.
func checkContainerRunning(at accTarget, name, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at, "echo unsupported",
			fmt.Sprintf("docker inspect -f '{{.State.Running}}' '%s' 2>/dev/null || echo MISSING", shq(name)))
		if err != nil {
			return fmt.Errorf("%s: docker inspect probe: %w", why, err)
		}
		if !strings.Contains(strings.ToLower(out), "true") {
			return fmt.Errorf("%s: container %s is not running (got %q)", why, name, strings.TrimSpace(out))
		}
		return nil
	}
}

// checkContainerAbsent asserts the named container no longer exists (destroy purge).
func checkContainerAbsent(at accTarget, name, why string) func(*terraform.State) error {
	return func(*terraform.State) error {
		out, err := probeHost(at, "echo unsupported",
			fmt.Sprintf("if docker inspect '%s' >/dev/null 2>&1; then echo PRESENT; else echo ABSENT; fi", shq(name)))
		if err != nil {
			return fmt.Errorf("%s: docker inspect probe: %w", why, err)
		}
		if !strings.Contains(out, "ABSENT") {
			return fmt.Errorf("%s: container %s still present after destroy", why, name)
		}
		return nil
	}
}

// DOK-01: docker_container deploy on L1 ⇒ image pulled and `docker run` succeeds;
// the container is Running; GET /health serves v=<version>; destroy purges the
// container. This is the positive counterpart to ART-06 and exercises the
// docker_container pattern end-to-end on the ssh/L1 transport.
func TestAccDOK01_DockerContainerDeploy(t *testing.T) {
	accPreCheck(t)
	at := requireL1(t)
	host, namespace, _ := splitAddress(t, Address)
	t.Setenv("TF_ACC_PROVIDER_HOST", host)
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", namespace)
	cfg := accDeploymentConfig(dockerSpec(t, at, "1.0.0"))
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("labdeploy_deployment.val", "deployed_version", "1.0.0"),
				checkContainerRunning(at, dockerCtrName, "DOK-01: docker_container must be running after apply"),
				checkHealthBody(at, dockerHealthURL(), "v=1.0.0"),
				checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
			),
		}},
		// Destroy must purge the container from L1.
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkContainerAbsent(at, dockerCtrName, "DOK-01: destroy must remove the container"),
			checkLockAbsent(at.tgt, installRootFor(at), "sample-svc"),
		),
	})
}
