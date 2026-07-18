@story-release:RELEASE-PROVIDER @phase-terraform-provider-surface @stage-read-drift-reconciliation-and-destroy-modes @setup-inline
Feature: Read drift reconciliation and destroy modes
  The labdeploy_deployment Read refresh reconciles the on-target manifest against
  Terraform state: an absent manifest removes the resource, a failed last_operation
  marks deployed_version so the next plan converges, and an unreachable target is a
  loud Read error that preserves prior state. An apply naming a lab-insecure transport
  setting emits exactly one WARN diagnostic (DESIGN §10.4, §11).

  Scenario: Read absent manifest removes resource
    Given a verified deployment in state whose target manifest is missing
    When the deployment Read refresh runs via a fake transport
    Then the resource is removed from state so the next plan is a create
    And the Read refresh raises no error

  Scenario: Failed op marker forces plan change
    Given a verified deployment in state whose manifest last_operation result is failed
    When the deployment Read refresh runs via a fake transport
    Then deployed_version carries the "!failed" suffix
    And the subsequent plan is non-empty and converging

  Scenario: Unreachable target is a loud Read error
    Given a verified deployment in state whose target fails to dial
    When the deployment Read refresh runs via a fake transport
    Then the Read refresh returns an ERROR diagnostic and not a warning
    And the prior state is left intact and the resource is not removed

  Scenario Outline: Exactly one insecure-transport warning per apply
    Given a deployment whose transport enables the insecure setting "<setting>"
    When an apply runs
    Then exactly one WARN diagnostic naming "<names>" is emitted
    And the apply raises no error

    Examples:
      | setting              | names                |
      | winrm_insecure_skip  | insecure_skip_verify |
      | ssh_unpinned_hostkey | host_key             |
