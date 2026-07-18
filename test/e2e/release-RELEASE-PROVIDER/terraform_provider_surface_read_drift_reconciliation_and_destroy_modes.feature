@story-release:RELEASE-PROVIDER @phase-terraform-provider-surface @stage-read-drift-reconciliation-and-destroy-modes @setup-inline
Feature: Read drift reconciliation and destroy modes
  The labdeploy_deployment Read refresh reconciles the on-target manifest against
  Terraform state: an absent manifest removes the resource, a failed last_operation
  marks deployed_version so the next plan converges, and an unreachable target is a
  loud Read error that preserves prior state. An apply naming a lab-insecure transport
  setting emits exactly one WARN diagnostic (DESIGN §10.4, §11). Destroy honours
  destroy_mode: purge stops, uninstalls, and removes the whole <root>/<app> tree;
  unregister stops and uninstalls but deletes only the manifest so a later Read sees an
  absent deployment while the kept releases remain; abandon opens no connection at all
  and simply drops Terraform state (DESIGN §10.5).

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

  Scenario: Purge destroy removes the whole app tree
    Given a verified deployment in state with destroy_mode "purge" whose target has a manifest and a kept release
    When the deployment Delete runs against the fake target
    Then the entire <root>/<app> tree is removed from the target, manifest and releases alike
    And a connection to the target was opened to perform the removal
    And the resource is dropped from Terraform state

  Scenario: Unregister destroy deletes the manifest but keeps the releases
    Given a verified deployment in state with destroy_mode "unregister" whose target has a manifest and a kept release
    When the deployment Delete runs against the fake target
    Then the target manifest is deleted so a later Read sees an absent deployment
    And the releases tree on the target is left intact
    And the resource is dropped from Terraform state

  Scenario: Abandon destroy opens no connection and drops state
    Given a verified deployment in state with destroy_mode "abandon" whose target has a manifest and a kept release
    When the deployment Delete runs against the fake target
    Then no connection is ever opened to the target
    And the target manifest and releases are left untouched
    And the resource is dropped from Terraform state
