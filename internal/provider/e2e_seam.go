//go:build e2e

package provider

// This file is compiled ONLY under `-tags e2e`. It exposes narrow, behaviour-
// preserving wrappers around the (unexported) Stage 5.2 deployment-resource
// internals — the id formula, the CRUD timeout defaults, and the engine boundary
// — so the out-of-package godog acceptance suite under test/e2e can drive the REAL
// impl (DESIGN §5.2, §12, §17) without duplicating it. Production builds never see
// these symbols.

import (
	"context"
	"fmt"
	"time"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/engine"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
)

// DeploymentIDForE2E exposes the real, unexported deploymentID formula
// (sha1(sorted(lower(hosts))+"/"+name)[0:12] + ":" + name) so the acceptance suite
// can prove the id is byte-identical under host reordering (DESIGN §5.2).
func DeploymentIDForE2E(d *spec.Deployment) string { return deploymentID(d) }

// DefaultTimeoutsForE2E exposes the real create/update/delete timeout defaults the
// CRUD methods apply when a resource config sets no explicit timeouts (DESIGN §5.2).
func DefaultTimeoutsForE2E() (create, update, del time.Duration) {
	return defaultCreateTimeout, defaultUpdateTimeout, defaultDeleteTimeout
}

// e2eConnectErrEngine is a deployEngine whose deploy always fails with a real
// PREFLIGHT ERR_CONNECT engine.CodedError (the taxonomy code transport.ErrConnect
// produces), so the acceptance suite can drive the Create coded-diagnostic path
// deterministically without a live transport.
type e2eConnectErrEngine struct{}

func (e2eConnectErrEngine) Update(context.Context, *spec.Deployment, *spec.Deployment) (*engine.Status, error) {
	return nil, &engine.CodedError{Code: "ERR_CONNECT", Host: "lab-01", Step: "PREFLIGHT",
		Err: fmt.Errorf("dial tcp: connection refused")}
}

func (e2eConnectErrEngine) ReadStatus(context.Context, *spec.Deployment) (*engine.Status, error) {
	return nil, &engine.CodedError{Code: "ERR_CONNECT", Host: "lab-01", Step: "PREFLIGHT",
		Err: fmt.Errorf("dial tcp: connection refused")}
}

func (e2eConnectErrEngine) Destroy(context.Context, *spec.Deployment, string) error {
	return &engine.CodedError{Code: "ERR_CONNECT", Host: "lab-01", Step: "PREFLIGHT",
		Err: fmt.Errorf("dial tcp: connection refused")}
}

func (e2eConnectErrEngine) Warns() []string { return nil }

// NewDeploymentResourceWithConnectError returns a DeploymentResource whose engine
// deploy always fails with a PREFLIGHT ERR_CONNECT, so the acceptance suite can drive
// the real Create → apply → diagSummary path and assert the "[ERR_CONNECT] " coded
// diagnostic Summary (DESIGN §12) deterministically.
func NewDeploymentResourceWithConnectError() *DeploymentResource {
	return &DeploymentResource{newEngine: func() deployEngine { return e2eConnectErrEngine{} }}
}
