//go:build e2e

// Package e2e drives the godog acceptance scenarios for Stage 2.1 (Transport
// Interface and Local Execution).
//
// Both scenarios exercise the REAL transport factory
// (internal/transport.NewTransport) against the gate host's own shell via
// transport local — no external service is required (setup: inline;
// proof: service:local-shell = the gate host's own powershell/sh, gated on
// runtime.GOOS per architecture D-gate-1):
//
//   - Local round-trip -> builds a local transport for the gate OS, Connects,
//     Uploads a byte payload to a temp path, Downloads it to a second temp path,
//     and asserts the bytes are identical. It then Execs a script that exits 0
//     and one that exits 7, asserting the application exit code is surfaced in
//     Result.ExitCode with a nil transport error (transport error only on
//     transport failure).
//   - OS mismatch rejected -> builds a local transport whose target.os is the
//     opposite of runtime.GOOS and asserts Connect returns an ERR_CONNECT coded
//     error before any command executes.
//
// Every Given/When/Then invokes the real transport and asserts on its result.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// transportGateOS returns the spec.OSKind matching the host the suite runs on.
func transportGateOS() spec.OSKind {
	if runtime.GOOS == "windows" {
		return spec.OSWindows
	}
	return spec.OSLinux
}

// transportOtherOS returns a spec.OSKind that never matches the gate host.
func transportOtherOS() spec.OSKind {
	if runtime.GOOS == "windows" {
		return spec.OSLinux
	}
	return spec.OSWindows
}

func localTransportTarget(os spec.OSKind) *spec.Target {
	return &spec.Target{
		Transport: spec.TransportLocal,
		Hosts:     []string{"localhost"},
		OS:        os,
	}
}

// transportWorld carries state across the steps of a single scenario.
type transportWorld struct {
	tr transport.Transport

	roundTripBytes []byte

	connectErr error
}

// --- Local round-trip -------------------------------------------------------

func (w *transportWorld) localTransportForGateOS() error {
	tr, err := transport.NewTransport(localTransportTarget(transportGateOS()), "localhost")
	if err != nil {
		return err
	}
	w.tr = tr
	return nil
}

func (w *transportWorld) connectUploadDownload() error {
	ctx := context.Background()
	if err := w.tr.Connect(ctx); err != nil {
		return err
	}
	if got := w.tr.OS(); got != transportGateOS() {
		return fmt.Errorf("OS()=%s want %s", got, transportGateOS())
	}
	if w.tr.Host() != "localhost" {
		return fmt.Errorf("Host()=%s want localhost", w.tr.Host())
	}

	dir, err := os.MkdirTemp("", "e2e-transport-roundtrip-")
	if err != nil {
		return err
	}
	remote := filepath.Join(dir, "sub", "payload.bin")
	local := filepath.Join(dir, "roundtrip.bin")
	want := []byte("release-provider round-trip \x00\x01\x02 data")

	if err := w.tr.Upload(ctx, bytes.NewReader(want), int64(len(want)), remote); err != nil {
		return err
	}
	if err := w.tr.Download(ctx, remote, local); err != nil {
		return err
	}
	got, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	w.roundTripBytes = got
	if !bytes.Equal(got, want) {
		return fmt.Errorf("round-trip mismatch: got %q want %q", got, want)
	}
	return nil
}

func (w *transportWorld) fileSurvivesRoundTrip() error {
	want := []byte("release-provider round-trip \x00\x01\x02 data")
	if !bytes.Equal(w.roundTripBytes, want) {
		return fmt.Errorf("round-trip mismatch: got %q want %q", w.roundTripBytes, want)
	}
	return nil
}

func (w *transportWorld) execReturnsAppExitCodes() error {
	ctx := context.Background()
	var okCmd, failCmd transport.Cmd
	if transportGateOS() == spec.OSWindows {
		okCmd = transport.Cmd{Shell: transport.ShellCmd, Script: "exit 0"}
		failCmd = transport.Cmd{Shell: transport.ShellCmd, Script: "exit 7"}
	} else {
		okCmd = transport.Cmd{Shell: transport.ShellSh, Script: "exit 0"}
		failCmd = transport.Cmd{Shell: transport.ShellSh, Script: "exit 7"}
	}

	res, err := w.tr.Exec(ctx, okCmd)
	if err != nil {
		return fmt.Errorf("Exec ok returned a transport error: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("Exec ok ExitCode=%d want 0", res.ExitCode)
	}

	res, err = w.tr.Exec(ctx, failCmd)
	if err != nil {
		return fmt.Errorf("non-zero app exit must NOT be a transport error: %w", err)
	}
	if res.ExitCode != 7 {
		return fmt.Errorf("Exec fail ExitCode=%d want 7", res.ExitCode)
	}
	return nil
}

// --- OS mismatch rejected ---------------------------------------------------

func (w *transportWorld) localTransportWithMismatchedOS() error {
	tr, err := transport.NewTransport(localTransportTarget(transportOtherOS()), "localhost")
	if err != nil {
		return err
	}
	w.tr = tr
	return nil
}

func (w *transportWorld) connectRuns() error {
	w.connectErr = w.tr.Connect(context.Background())
	return nil
}

func (w *transportWorld) connectErrorsBeforeExecuting() error {
	if w.connectErr == nil {
		return fmt.Errorf("Connect must fail when target.os != runtime.GOOS")
	}
	var ce *transport.CodedError
	if !errors.As(w.connectErr, &ce) {
		return fmt.Errorf("expected a *transport.CodedError, got %T: %v", w.connectErr, w.connectErr)
	}
	if ce.Code != "ERR_CONNECT" {
		return fmt.Errorf("coded error=%s want ERR_CONNECT", ce.Code)
	}
	return nil
}

// InitializeScenario_transport_and_artifact_acquisition_transport_interface_and_local_execution
// registers the step definitions for the Stage 2.1 godog suite. The unique name
// prevents collisions with sibling stages sharing the e2e package.
func InitializeScenario_transport_and_artifact_acquisition_transport_interface_and_local_execution(ctx *godog.ScenarioContext) {
	w := &transportWorld{}

	ctx.Before(func(c context.Context, _ *godog.Scenario) (context.Context, error) {
		*w = transportWorld{}
		return c, nil
	})

	ctx.Step(`^a local transport for the gate OS$`, w.localTransportForGateOS)
	ctx.Step(`^Connect succeeds and a file is uploaded then downloaded$`, w.connectUploadDownload)
	ctx.Step(`^the file survives the round-trip byte-for-byte$`, w.fileSurvivesRoundTrip)
	ctx.Step(`^Exec returns the application exit code in Result with no transport error$`, w.execReturnsAppExitCodes)

	ctx.Step(`^a local transport whose target os does not match the runner$`, w.localTransportWithMismatchedOS)
	ctx.Step(`^Connect runs$`, w.connectRuns)
	ctx.Step(`^Connect errors before executing any command$`, w.connectErrorsBeforeExecuting)
}

// TestE2E_transport_and_artifact_acquisition_transport_interface_and_local_execution
// is the go test entrypoint for the Stage 2.1 godog suite.
func TestE2E_transport_and_artifact_acquisition_transport_interface_and_local_execution(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_transport_and_artifact_acquisition_transport_interface_and_local_execution,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"transport_and_artifact_acquisition_transport_interface_and_local_execution.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status: godog acceptance scenarios failed")
	}
}
