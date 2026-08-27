//go:build e2e

package provider

// This file is compiled ONLY under `-tags e2e`. It exposes narrow, behaviour-
// preserving wrappers around the (unexported) Stage 5.2 deployment-resource
// internals — the id formula and the engine/transport boundary — so the
// out-of-package godog acceptance suite under test/e2e can drive the REAL impl
// (DESIGN §5.2, §12, §17) without duplicating it. Production builds never see
// these symbols.

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// DeploymentIDForE2E exposes the real, unexported deploymentID formula
// (sha1(sorted(lower(hosts))+"/"+name)[0:12] + ":" + name) so the acceptance suite
// can prove the id is byte-identical under host reordering (DESIGN §5.2).
func DeploymentIDForE2E(d *spec.Deployment) string { return deploymentID(d) }

// e2eDialFailTransport is a transport.Transport whose Connect fails exactly the way
// a real preflight dial failure does: it returns transport.ErrConnect(...), the
// *transport.CodedError{Code:"ERR_CONNECT"} the SSH/WinRM transports emit when the
// TCP dial is refused (transport.go). It is injected through the engine's public
// NewTransport seam (engine.go §17) so the REAL engine.deploySingle → t.Connect →
// wrapTransportErr taxonomy path classifies it into an engine.CodedError with code
// ERR_CONNECT — nothing about the code is fabricated by the test.
type e2eDialFailTransport struct{ host string }

func (t *e2eDialFailTransport) Connect(context.Context) error {
	return transport.ErrConnect(fmt.Errorf("dial tcp %s:5985: connect: connection refused", t.host))
}
func (t *e2eDialFailTransport) Close() error    { return nil }
func (t *e2eDialFailTransport) OS() spec.OSKind { return spec.OSWindows }
func (t *e2eDialFailTransport) Host() string    { return t.host }
func (t *e2eDialFailTransport) Exec(context.Context, transport.Cmd) (transport.Result, error) {
	return transport.Result{}, nil
}
func (t *e2eDialFailTransport) Upload(context.Context, io.Reader, int64, string) error { return nil }
func (t *e2eDialFailTransport) Download(context.Context, string, string) error         { return nil }

// NewDeploymentResourceWithConnectError returns a DeploymentResource wired to the
// REAL engine whose transport factory yields e2eDialFailTransport. Driving Create
// runs the genuine engine deploy path (deploySingle → Connect → wrapTransportErr),
// so the resulting diagnostic Summary "[ERR_CONNECT] ..." originates from
// transport.ErrConnect through the real taxonomy — not a hand-built CodedError
// (DESIGN §12, §17).
func NewDeploymentResourceWithConnectError() *DeploymentResource {
	eng := engine.New()
	eng.NewTransport = func(_ *spec.Target, host string) (transport.Transport, error) {
		return &e2eDialFailTransport{host: host}, nil
	}
	return &DeploymentResource{newEngine: func() deployEngine { return realEngine{eng} }}
}

// DeadlineCapture records the context deadline the CRUD methods hand to the engine,
// so the acceptance suite can prove the schema's timeout DEFAULTS behaviourally:
// with no explicit timeouts in config, Create/Update apply a ~30m deadline and
// Delete a ~15m deadline (DESIGN §5.2). The remaining-duration is captured at the
// engine boundary, i.e. AFTER the real plan.Timeouts.Create(ctx, defaultCreateTimeout)
// resolution and context.WithTimeout wrapping.
type DeadlineCapture struct {
	CreateOrUpdateRemaining time.Duration
	DeleteRemaining         time.Duration
}

// e2eDeadlineEngine is a deployEngine that captures the ctx deadline the resource
// applies (never fabricating one) and returns a benign Status so the CRUD flow
// completes.
type e2eDeadlineEngine struct{ cap *DeadlineCapture }

func (e e2eDeadlineEngine) Update(ctx context.Context, d *spec.Deployment, _ *spec.Deployment) (*engine.Status, error) {
	if dl, ok := ctx.Deadline(); ok {
		e.cap.CreateOrUpdateRemaining = time.Until(dl)
	}
	return e.okStatus(d), nil
}

func (e e2eDeadlineEngine) ReadStatus(_ context.Context, d *spec.Deployment) (*engine.Status, error) {
	return e.okStatus(d), nil
}

func (e e2eDeadlineEngine) Destroy(ctx context.Context, _ *spec.Deployment, _ string) error {
	if dl, ok := ctx.Deadline(); ok {
		e.cap.DeleteRemaining = time.Until(dl)
	}
	return nil
}

func (e e2eDeadlineEngine) Warns() []string { return nil }

func (e e2eDeadlineEngine) okStatus(d *spec.Deployment) *engine.Status {
	return &engine.Status{
		DeployedVersion: d.Artifact.Version,
		ReleasePath:     "C:/labdeploy/releases/" + d.Artifact.Version,
		ServiceStatus:   "running",
		Hosts:           d.Target.Hosts,
	}
}

// NewDeploymentResourceWithDeadlineCapture returns a DeploymentResource plus a
// DeadlineCapture pointer; driving the real Create/Update/Delete methods through the
// real schema (with no explicit timeouts) records the deadline the CRUD path applies
// from the framework timeouts block + defaults.
func NewDeploymentResourceWithDeadlineCapture() (*DeploymentResource, *DeadlineCapture) {
	cap := &DeadlineCapture{}
	r := &DeploymentResource{newEngine: func() deployEngine { return e2eDeadlineEngine{cap: cap} }}
	return r, cap
}

// NewDeploymentResourceWithTransport returns a DeploymentResource wired to the REAL
// engine.New() whose per-host transport factory is the caller-supplied newT. Driving
// Read/Create against it runs the GENUINE engine reconciliation and deploy paths
// (ReadStatus → Connect → ReadManifest → ReconcileManifest → pattern.Status; apply →
// Update → Deploy → preflight/warnInsecureTransport → …) against a scripted fake
// transport, so the drift outcome (RemoveResource on an absent manifest, the "!failed"
// deployed_version marker, a loud Read ERROR on a failed dial) and the insecure-transport
// WARN are produced by the impl reading scripted Result values — nothing is injected at
// the engine boundary (DESIGN §10.4, §11, §17). This is the Stage 5.3 acceptance seam.
func NewDeploymentResourceWithTransport(newT func(*spec.Target, string) (transport.Transport, error)) *DeploymentResource {
	eng := engine.New()
	eng.NewTransport = newT
	return &DeploymentResource{newEngine: func() deployEngine { return realEngine{eng} }}
}
