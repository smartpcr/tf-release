package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// consoleSpec builds a console_app Deployment. post_install and verify_command
// carry sentinels (LDPOSTINSTALL / LDVERIFY) the fake transport recognizes so
// their success/failure can be scripted. version defaults to 1.0.0 when empty.
func consoleSpec(t *testing.T, url, checksum, version string) *spec.Deployment {
	t.Helper()
	if version == "" {
		version = "1.0.0"
	}
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
  type: zip
  version: ` + version + `
  checksum: "` + checksum + `"
  source: { type: http, url: "` + url + `" }
pattern:
  type: console_app
  exe: sample-svc.exe
  post_install: "LDPOSTINSTALL"
  verify_command: "LDVERIFY --check"
strategy: { keep_releases: 2, rollback_on_failure: true }
`
	d, _, err := spec.ParseDeployment(y, nil, "")
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return d
}

// noReleaseTree asserts no release tree survives (fresh console failure ⇒
// machine clean, DESIGN §10.2 fresh-install row).
func noReleaseTree(t *testing.T, f *fakeHost) {
	t.Helper()
	for k := range f.files {
		if strings.Contains(k, `\releases\`) {
			t.Fatalf("fresh console failure must leave NO release tree; found %q", k)
		}
	}
}

// TestConsolePostInstallFailureFreshClean covers evaluator iter2 item 2: a
// console_app post_install runs AFTER switchover, so its failure on a fresh
// install must still leave the machine clean — including removing the release
// tree that STAGE created (which stageOnHost's pre-switch cleanup never sees).
func TestConsolePostInstallFailureFreshClean(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("console-pi"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["postinstall"] = true
	eng := engineWith(f)

	_, err := eng.Deploy(context.Background(), consoleSpec(t, url, sum, "1.0.0"))
	if err == nil {
		t.Fatal("expected console post_install failure")
	}
	if !strings.Contains(err.Error(), "ERR_SERVICE_INSTALL") {
		t.Fatalf("console post_install failure must map to ERR_SERVICE_INSTALL; got %v", err)
	}
	if !strings.Contains(err.Error(), "target cleaned") {
		t.Fatalf("fresh console failure must be annotated as cleaned; got %v", err)
	}
	if f.current != "" {
		t.Fatalf("fresh console failure must leave NO junction, got %q", f.current)
	}
	if _, has := f.files[`C:\deploy\sample-svc\manifest.json`]; has {
		t.Fatalf("fresh console failure must leave NO manifest; log=%v", f.log)
	}
	noReleaseTree(t, f)
}

// TestConsoleVerifyFailureFreshClean is the verify_command sibling of the above:
// verify runs post-switch too, and its failure must also leave a clean machine.
func TestConsoleVerifyFailureFreshClean(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("console-vc"))
	defer done()
	f := newFakeHost("lab-01")
	f.fail["verify"] = true
	eng := engineWith(f)

	_, err := eng.Deploy(context.Background(), consoleSpec(t, url, sum, "1.0.0"))
	if err == nil {
		t.Fatal("expected console verify_command failure")
	}
	if !strings.Contains(err.Error(), "ERR_HEALTH_CHECK") {
		t.Fatalf("console verify_command failure must map to ERR_HEALTH_CHECK; got %v", err)
	}
	if f.current != "" {
		t.Fatalf("fresh console failure must leave NO junction, got %q", f.current)
	}
	noReleaseTree(t, f)
}

// TestReadStatusConsoleDriftThroughEngine covers evaluator iter2 item 1: when
// the manifest current_version and the on-host `current\.labdeploy-release.json`
// marker AGREE (both 1.1.0) ReadStatus must report n/a even though the DESIRED
// artifact version (also read from the spec) is irrelevant to Read; when the
// marker lags the manifest it must report drift.
func TestReadStatusConsoleDriftThroughEngine(t *testing.T) {
	url, sum, done := testArtifactServer(t, []byte("console-read"))
	defer done()
	const markerPath = `C:\deploy\sample-svc\current\.labdeploy-release.json`

	seed := func(t *testing.T, markerVer string) *fakeHost {
		f := newFakeHost("lab-01")
		m := &Manifest{
			Schema: 1, App: "sample-svc", Pattern: "console_app",
			CurrentVersion: "1.1.0", CurrentRelease: `C:\deploy\sample-svc\releases\1.1.0`,
			ProviderVersion: ProviderVersion,
			LastOperation:   LastOp{Type: "deploy", Result: "success", Started: nowRFC3339()},
		}
		if err := WriteManifest(context.Background(), f, testPaths(), m); err != nil {
			t.Fatalf("seed manifest: %v", err)
		}
		f.files[markerPath] = []byte(`{"version":"` + markerVer + `","sha256":"x","extracted_at":"2026-01-01T00:00:00Z"}`)
		return f
	}

	// Desired spec version 1.1.0, marker matches manifest ⇒ n/a.
	t.Run("match", func(t *testing.T) {
		eng := engineWith(seed(t, "1.1.0"))
		st, err := eng.ReadStatus(context.Background(), consoleSpec(t, url, sum, "1.1.0"))
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.ServiceStatus != "n/a" {
			t.Fatalf("marker==manifest must be n/a, got %q", st.ServiceStatus)
		}
	})

	// Regression: even when the DESIRED version differs from the deployed one,
	// a marker that matches the manifest must NOT be reported as drift.
	t.Run("desired_differs_but_deployed_matches", func(t *testing.T) {
		eng := engineWith(seed(t, "1.1.0"))
		st, err := eng.ReadStatus(context.Background(), consoleSpec(t, url, sum, "1.2.0"))
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.ServiceStatus != "n/a" {
			t.Fatalf("desired 1.2.0 but deployed marker/manifest 1.1.0 must be n/a, got %q", st.ServiceStatus)
		}
	})

	// Marker lags the manifest (junction repointed) ⇒ drift.
	t.Run("drift", func(t *testing.T) {
		eng := engineWith(seed(t, "1.0.0"))
		st, err := eng.ReadStatus(context.Background(), consoleSpec(t, url, sum, "1.1.0"))
		if err != nil {
			t.Fatalf("ReadStatus: %v", err)
		}
		if st.ServiceStatus != "drift" {
			t.Fatalf("marker 1.0.0 vs manifest 1.1.0 must be drift, got %q", st.ServiceStatus)
		}
	})
}
