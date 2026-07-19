@story-release:RELEASE-PROVIDER @phase-failover-cluster-support @stage-cluster-generic-service-pattern-scripts @setup-inline
Feature: Cluster generic service pattern scripts
  The cluster_generic_service pattern generates WSFC coordinator scripts for a
  Generic Service role and refuses to bind a role that is already owned by a
  different service. (DESIGN §9.5)

  Scenario: Cluster create scripts golden
    Given a cluster_generic_service spec with service "PaymentsSvc" role "PaymentsRole" static address "10.0.0.42"
    And the preferred owner ordering "lab-02,lab-01,lab-03"
    When the create and move scripts are generated
    Then the create-role script matches the committed golden "cluster_create_role.golden"
    And the create-role script passes "-StaticAddress '10.0.0.42'"
    And the preferred-owners script matches the committed golden "cluster_preferred_owners.golden"
    And the preferred-owners script preserves the owner ordering "'lab-02','lab-01','lab-03'"
    And the move-group script matches the committed golden "cluster_move_group.golden"

  Scenario: Role binding conflict
    Given a cluster role "PaymentsRole" already bound to service "other-svc" owned by "lab-01"
    When preflight runs for spec service "PaymentsSvc" via a fake transport
    Then it fails with code "ERR_SERVICE_INSTALL"
    And the error names both service "other-svc" and service "PaymentsSvc"
    And nothing is modified — only the read-only role-binding probe ran
