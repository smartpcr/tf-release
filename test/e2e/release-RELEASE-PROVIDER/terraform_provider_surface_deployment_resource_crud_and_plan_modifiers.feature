@story-release:RELEASE-PROVIDER @phase-terraform-provider-surface @stage-deployment-resource-crud-and-plan-modifiers @setup-inline
Feature: Deployment resource CRUD and plan modifiers
  The labdeploy_deployment resource forces replacement on immutable spec paths,
  treats host reordering as a no-op, surfaces coded diagnostics, computes a
  deterministic host-order-independent id, applies timeout defaults, and refuses
  import (DESIGN §5.2, §12).

  Scenario Outline: RequiresReplace fires on immutable path <field>
    Given a prior deployment snapshot in state
    And a plan that changes only the immutable field "<field>"
    When the deployment plan is computed
    Then the change forces RequiresReplace

    Examples:
      | field                |
      | pattern.type         |
      | service_name         |
      | target.hosts         |
      | target.os            |
      | metadata.name        |
      | pattern.install_root |
      | role_name            |

  Scenario: Host reorder is not a replacement
    Given a prior deployment snapshot with hosts "lab-01,lab-02"
    And a plan with the same hosts reordered to "lab-02,lab-01" and no other change
    When the deployment plan is computed
    Then RequiresReplace does not fire

  Scenario: Create surfaces a coded ERR_CONNECT diagnostic Summary
    Given a deployment whose engine preflight fails with the ERR_CONNECT taxonomy code
    When Create surfaces the failure
    Then the diagnostic Summary begins with "[ERR_CONNECT] "

  Scenario: Deterministic host-order-independent SHA-1 id
    Given a spec with name "n" and hosts "b,a"
    When the deployment id is computed
    Then it equals the sha1(sorted(hosts)+"/"+name)[0:12] + ":" + name formula
    And it is byte-identical when the same hosts are supplied in a different order

  Scenario: Timeout defaults when none are configured
    Given a deployment resource config with no explicit timeouts
    When the resource timeout defaults are read
    Then create and update default to 30m and delete defaults to 15m

  Scenario: Import is unsupported
    Given the labdeploy_deployment resource
    When terraform import is invoked on it
    Then the error is "import is not supported; adopt via apply"
