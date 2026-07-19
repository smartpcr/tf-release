@story-release:RELEASE-PROVIDER @phase-failover-cluster-support @stage-cluster-rolling-update-and-rollback-engine @setup-inline
Feature: Cluster rolling update and rollback engine
  As the release provider engine
  A cluster_generic_service rolling UPDATE must update passives in hosts order with
  exactly one role failover, and on a health failure must roll the whole cluster back
  to the previous version. Proven in-process against a fake Transport with a scripted
  Result queue (no network, no docker).

  Scenario: Rolling update order
    Given a 3-node cluster fake transport with owner "lab-01" running release "1.0.0"
    And the cluster hosts are "lab-01,lab-02,lab-03" with no preferred owner
    When a rolling UPDATE to "2.0.0" runs
    Then the update succeeds
    And the passives are updated in hosts order "lab-02,lab-03" before the failover
    And exactly one MOVE_GROUP occurs
    And the role owner ends on a node running the new release "2.0.0"
    And every node records a success manifest at "2.0.0"

  Scenario: Cluster health rollback
    Given a 2-node cluster fake transport with owner "lab-01" running release "1.0.0"
    And the cluster hosts are "lab-01,lab-02" with no preferred owner
    And the health check fails on firstNew "lab-02" while it runs the new release
    When a rolling UPDATE to "2.0.0" runs
    Then the update fails and rolls back to "1.0.0"
    And the role moves back to the original owner "lab-01"
    And both nodes junction to the previous version "1.0.0"
    And both manifests read "rolled_back" at "1.0.0"
