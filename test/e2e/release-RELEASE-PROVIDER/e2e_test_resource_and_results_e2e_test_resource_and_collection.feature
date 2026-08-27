@story-release:RELEASE-PROVIDER @phase-e2e-test-resource-and-results @stage-e2e-test-resource-and-collection @setup-inline
Feature: E2E Test Resource and Collection
  The labdeploy_e2e_test resource runs a test package on a target during apply,
  collecting the results tree, logs and summary.json before surfacing a
  test-failure error, killing the process tree on timeout, and wiring an
  ordering-only dependency edge via deployment_id (DESIGN §5.3, §7.4).

  Scenario: Collection before failure
    Given an e2e_test resource with fail_on_test_failure true and a fake transport serving the on-target results zip
    When Create runs against the fake transport for a failing run
    Then the persisted results_dir tree is byte-identical to the committed golden snapshot
    And ERR_TEST_FAILED is returned only after the tree is fully written

  Scenario: Timeout kills tree
    Given a TestRun whose runner timeout is exceeded on a fake transport
    When the engine runs the test
    Then it returns ERR_TIMEOUT and issues the process-tree kill script

  Scenario: deployment_id wires a reference without replacement
    Given the labdeploy_e2e_test resource schema
    When the deployment_id attribute is inspected
    Then it is a settable string attribute with no RequiresReplace plan modifier

  Scenario: deployment_id ordering under apply
    Given a config where labdeploy_e2e_test deployment_id references the deployment id
    When terraform apply runs in the plugin-testing harness
    Then Terraform Core orders the e2e_test create strictly after the deployment create
