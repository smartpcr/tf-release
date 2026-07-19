package pattern

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/spec"
	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// Scenario: Cluster create scripts golden (DESIGN §9.5 C2/C3/C4, T6).
// Given a cluster_generic_service spec, when the coordinator create + move
// scripts are generated they must match the committed goldens, INCLUDING the
// `-StaticAddress` argument (CAP) on Add-ClusterGenericServiceRole and the
// preferred_owner ordering passed to Set-ClusterOwnerNode. Each coordinator
// verb (CreateRole/SetPreferredOwners/StartGroup/MoveGroup/StopGroup) is
// captured as its own golden so a regression in any one script is localized.
func TestClusterGenericCreateScriptsGolden(t *testing.T) {
	var c ClusterGeneric
	f := &scriptTransport{osKind: spec.OSWindows}
	ctx := context.Background()

	// C2: role creation WITH a static address (creates a CAP).
	if err := c.CreateRole(ctx, f, "PaymentsSvc", "PaymentsRole", "10.0.0.42"); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	// C3: preferred owner first, then the remaining nodes in spec order.
	if err := c.SetPreferredOwners(ctx, f, "PaymentsRole", []string{"lab-02", "lab-01", "lab-03"}); err != nil {
		t.Fatalf("SetPreferredOwners: %v", err)
	}
	// C4: bring the role Online.
	if err := c.StartGroup(ctx, f, "PaymentsRole", 120); err != nil {
		t.Fatalf("StartGroup: %v", err)
	}
	// U4: move the role to the first updated node with a drain wait.
	if err := c.MoveGroup(ctx, f, "PaymentsRole", "lab-02", 300); err != nil {
		t.Fatalf("MoveGroup: %v", err)
	}
	// create-failure / destroy path: take the role Offline.
	if err := c.StopGroup(ctx, f, "PaymentsRole"); err != nil {
		t.Fatalf("StopGroup: %v", err)
	}

	if len(f.scripts) != 5 {
		t.Fatalf("expected 5 coordinator scripts, got %d", len(f.scripts))
	}
	checkGolden(t, "cluster_create_role.golden", f.scripts[0])
	checkGolden(t, "cluster_preferred_owners.golden", f.scripts[1])
	checkGolden(t, "cluster_start_group.golden", f.scripts[2])
	checkGolden(t, "cluster_move_group.golden", f.scripts[3])
	checkGolden(t, "cluster_stop_group.golden", f.scripts[4])

	// The create script must carry the StaticAddress (CAP) argument.
	if !strings.Contains(f.scripts[0], "-StaticAddress '10.0.0.42'") {
		t.Errorf("CreateRole script must pass -StaticAddress:\n%s", f.scripts[0])
	}
	if !strings.Contains(f.scripts[0], "Add-ClusterGenericServiceRole -ServiceName 'PaymentsSvc' -Name 'PaymentsRole'") {
		t.Errorf("CreateRole script must name service + role:\n%s", f.scripts[0])
	}
	// A conflicting create must be exit-code gated to ERR_SERVICE_INSTALL (46).
	if !strings.Contains(f.scripts[0], "exit 46") {
		t.Errorf("CreateRole failure must map to ERR_SERVICE_INSTALL (exit 46):\n%s", f.scripts[0])
	}
	// The preferred_owner ordering must be preserved verbatim.
	if !strings.Contains(f.scripts[1], "Set-ClusterOwnerNode -Group 'PaymentsRole' -Owners 'lab-02','lab-01','lab-03'") {
		t.Errorf("SetPreferredOwners must pass the ordered owner list:\n%s", f.scripts[1])
	}
	// The move must gate on owner + Online and map failures to ERR_CLUSTER_MOVE (47).
	if !strings.Contains(f.scripts[3], "Move-ClusterGroup -Name 'PaymentsRole' -Node 'lab-02' -Wait 300") {
		t.Errorf("MoveGroup must target the node with a drain wait:\n%s", f.scripts[3])
	}
	if !strings.Contains(f.scripts[3], "exit 47") {
		t.Errorf("MoveGroup failure must map to ERR_CLUSTER_MOVE (exit 47):\n%s", f.scripts[3])
	}
}

// TestClusterGenericCreateRoleNoStaticAddress guards that an empty
// static_address omits the -StaticAddress argument entirely (no dangling flag).
func TestClusterGenericCreateRoleNoStaticAddress(t *testing.T) {
	var c ClusterGeneric
	f := &scriptTransport{osKind: spec.OSWindows}
	if err := c.CreateRole(context.Background(), f, "PaymentsSvc", "PaymentsRole", ""); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if strings.Contains(f.scripts[0], "-StaticAddress") {
		t.Errorf("empty static_address must omit -StaticAddress:\n%s", f.scripts[0])
	}
}

// Scenario: Role binding conflict (DESIGN §9.5 preflight, CLU-06).
// Given the WSFC role already exists bound to `other-svc`, when PreflightRole
// runs via a fake transport whose scripted probe reports that binding, then it
// yields an ERR_SERVICE_INSTALL error NAMING BOTH the bound service and the
// spec's wanted service, and NOTHING is modified — the only script that ran is
// the read-only role-binding probe.
func TestClusterGenericRoleBindingConflict(t *testing.T) {
	var c ClusterGeneric
	f := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{
		{ExitCode: 0, Stdout: "PRESENT|other-svc|lab-01|Online"},
	}}

	exists, owner, err := c.PreflightRole(context.Background(), f, "PaymentsRole", "PaymentsSvc")
	if err == nil {
		t.Fatal("PreflightRole must fail when the role is bound to a different service")
	}
	var se *StepError
	if !errors.As(err, &se) {
		t.Fatalf("expected *StepError, got %T: %v", err, err)
	}
	if se.Code != "ERR_SERVICE_INSTALL" {
		t.Errorf("role/service binding conflict must map to ERR_SERVICE_INSTALL, got %q", se.Code)
	}
	// The error must name BOTH services so an operator can see the collision.
	if !strings.Contains(err.Error(), "other-svc") || !strings.Contains(err.Error(), "PaymentsSvc") {
		t.Errorf("conflict error must name both the bound service and the spec service, got: %v", err)
	}
	if exists || owner != "" {
		t.Errorf("a conflict must not report the role as usable, got exists=%v owner=%q", exists, owner)
	}
	// Nothing modified: only the read-only RoleBinding probe ran.
	if len(f.scripts) != 1 {
		t.Fatalf("conflict preflight must run only the read-only probe, got %d scripts: %v", len(f.scripts), f.scripts)
	}
	if !strings.Contains(f.scripts[0], "Get-ClusterResource") {
		t.Errorf("the single script must be the read-only role-binding probe:\n%s", f.scripts[0])
	}
	for _, banned := range []string{"Add-ClusterGenericServiceRole", "Set-ClusterOwnerNode", "Move-ClusterGroup", "Start-ClusterGroup", "Remove-ClusterGroup"} {
		if strings.Contains(f.scripts[0], banned) {
			t.Errorf("conflict preflight must not emit a mutating cmdlet (%s):\n%s", banned, f.scripts[0])
		}
	}
}

// TestClusterGenericPreflightRoleNoConflict covers the two non-conflict paths:
// the role bound to the SAME service (exists, owner returned) and the role
// ABSENT (fresh create — exists=false), neither of which errors.
func TestClusterGenericPreflightRoleNoConflict(t *testing.T) {
	var c ClusterGeneric

	same := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{
		{ExitCode: 0, Stdout: "PRESENT|PaymentsSvc|lab-02|Online"},
	}}
	exists, owner, err := c.PreflightRole(context.Background(), same, "PaymentsRole", "PaymentsSvc")
	if err != nil {
		t.Fatalf("matching service binding must not error: %v", err)
	}
	if !exists || owner != "lab-02" {
		t.Errorf("expected exists=true owner=lab-02, got exists=%v owner=%q", exists, owner)
	}

	absent := &scriptTransport{osKind: spec.OSWindows, queue: []transport.Result{
		{ExitCode: 0, Stdout: "ABSENT"},
	}}
	exists, owner, err = c.PreflightRole(context.Background(), absent, "PaymentsRole", "PaymentsSvc")
	if err != nil {
		t.Fatalf("absent role must not error (fresh create): %v", err)
	}
	if exists || owner != "" {
		t.Errorf("absent role must report exists=false owner=\"\", got exists=%v owner=%q", exists, owner)
	}
}
