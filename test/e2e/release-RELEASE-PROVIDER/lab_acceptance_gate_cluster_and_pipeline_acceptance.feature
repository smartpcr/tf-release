@story-release:RELEASE-PROVIDER @phase-lab-acceptance-gate @stage-cluster-and-pipeline-acceptance @setup-inline
Feature: Cluster and Pipeline Acceptance (lab acceptance gate)
  As the release provider
  The C2/C3 WSFC rolling engine (CLU-01..08) must fresh-create a role, roll passives
  in hosts order with EXACTLY ONE failover (a single service gap), keep the correct
  owner, honor a preferred owner, roll the whole cluster back on a health failure,
  refuse a conflicting role or an unreachable node without mutating anything, and
  purge cleanly on destroy. The shipped GitHub Actions + Azure DevOps pipelines
  (PIP-01..03) must always publish their results artifacts and, on a deploy failure,
  recover the target to the last known good version v=1.1.0.

  The `[proof: lab]` host legs (a real two/three-node WSFC lab; live GH Actions + ADO
  runners) run under TF_ACC=1 against the operator's lab, wired by the Stage 9.2 lab
  pipeline. On the plain gate the SAME acceptance PROPERTIES are proven reproducibly
  and with NO skip: the REAL rolling engine (internal/engine) is driven over an
  in-process scripted WSFC transport across the full CLU-01..08 matrix, and the REAL
  shipped pipelines plus the REAL registration code (tests/e2e/pipeline) are asserted.

  # ---- CLU-01: fresh create ----
  Scenario: Fresh cluster create brings the role online on an owner running the new release
    Given a fresh WSFC cluster with hosts "lab-01,lab-02"
    When release "1.0.0" is applied to the cluster
    Then the cluster apply succeeds
    And the cluster role is online
    And the role owner runs release "1.0.0"
    And every node records a success manifest at "1.0.0"
    And the reported service status is "online"

  # ---- CLU-02: single-failover gap ----
  Scenario: Rolling update fails over exactly once with a single service gap
    Given a healthy WSFC cluster with hosts "lab-01,lab-02" owner "lab-01" running release "1.0.0"
    When release "1.1.0" is applied to the cluster
    Then the cluster apply succeeds
    And exactly one cluster failover occurs
    And the new owner is health-checked before the old owner is drained
    And the role owner ends on the previously passive node running release "1.1.0"

  # ---- CLU-03: C3 rolling order ----
  Scenario: Three-node rolling update updates passives in hosts order with one failover
    Given a healthy WSFC cluster with hosts "lab-01,lab-02,lab-03" owner "lab-01" running release "1.0.0"
    When release "1.1.0" is applied to the cluster
    Then the cluster apply succeeds
    And the passives are updated in hosts order "lab-02,lab-03" before the failover
    And exactly one cluster failover occurs
    And the former owner "lab-01" is updated last

  # ---- CLU-04: health rollback ----
  Scenario: A health failure rolls the whole cluster back to the previous version
    Given a healthy WSFC cluster with hosts "lab-01,lab-02" owner "lab-01" running release "1.0.0"
    And the health check fails on the newly updated passive node
    When release "1.1.0" is applied to the cluster
    Then the cluster apply fails and rolls the cluster back to "1.0.0"
    And the role returns to the original owner "lab-01"
    And every node is rolled back to release "1.0.0"

  # ---- CLU-05: connection failure ----
  Scenario: An unreachable node fails the apply without any partial switch
    Given a healthy WSFC cluster with hosts "lab-01,lab-02" owner "lab-01" running release "1.0.0"
    And WinRM to "lab-02" is blocked
    When release "1.1.0" is applied to the cluster
    Then the cluster apply fails to connect to "lab-02"
    And the reachable node "lab-01" is left unchanged at release "1.0.0"

  # ---- CLU-06: role conflict ----
  Scenario: A role already bound to a different service is refused without mutation
    Given a healthy WSFC cluster with hosts "lab-01,lab-02" owner "lab-01" running release "1.0.0"
    And the cluster role is bound to a different service "OtherSvc"
    When release "1.1.0" is applied to the cluster
    Then the cluster apply is refused with a service-install conflict naming both services
    And nothing is modified on any node

  # ---- CLU-07: destroy purge ----
  Scenario: Destroy purges the role, services and payload on every node
    Given a healthy WSFC cluster with hosts "lab-01,lab-02" owner "lab-01" running release "1.0.0"
    When the cluster deployment is destroyed with purge
    Then the cluster role is removed
    And the service and payload are gone on every node

  # ---- CLU-08: preferred owner ----
  Scenario: A rolling update lands the role on the preferred owner
    Given a healthy WSFC cluster with hosts "lab-01,lab-02,lab-03" owner "lab-01" running release "1.0.0"
    And the preferred owner is "lab-03"
    When release "1.1.0" is applied to the cluster
    Then the cluster apply succeeds
    And the role owner ends on the preferred node "lab-03"

  # ---- CLU lab leg (TF_ACC=1, C2/C3): the FULL CLU-01..08 matrix on the real cluster ----
  Scenario: Cluster rolling acceptance against the C2/C3 lab under TF_ACC=1
    Given the C2/C3 WSFC lab connection when provided
    When the full CLU-01..08 matrix runs against the C2/C3 lab under TF_ACC=1
    Then every CLU-01..08 acceptance check passes on the real cluster

  # ---- PIP-01..03: pipeline smoke ----
  Scenario: Shipped pipelines always publish artifacts and recover the target on failure
    Given the shipped reference pipelines
    Then the GitHub workflow always uploads a results artifact
    And the Azure pipeline always publishes VSTest results and a results artifact
    And the GitHub deploy failure triggers a rollback job that reapplies the last known good version
    And the Azure deploy failure triggers a rollback stage that reapplies the last known good version
    And registering the GitHub workflow preserves the failure-guarded rollback job
    And the live pipelines publish results and recover the target to "1.1.0" when provided
