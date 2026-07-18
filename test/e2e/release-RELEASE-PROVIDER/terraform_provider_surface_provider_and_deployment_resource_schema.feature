@story-release:RELEASE-PROVIDER @phase-terraform-provider-surface @stage-provider-and-deployment-resource-schema @setup-inline
Feature: Provider and Deployment Resource Schema
  The labdeploy_deployment resource schema enforces the VAL-06 exactly-one-of
  rule for spec/spec_file at the Terraform config layer, and round-trips a
  deployment state through the schema without dropping or mutating attributes
  (DESIGN §5.2, §14, VAL-06). These scenarios drive the REAL provider schema and
  config validators — no provider server, in-process (terraform-plugin-framework).

  Scenario: VAL-06 One-of validation rejects both spec and spec_file set
    Given a deployment resource config with both spec and spec_file set
    When the deployment resource config validators run
    Then validation fails with an "exactly one of" error

  Scenario: One-of validation rejects neither spec nor spec_file set
    Given a deployment resource config with neither spec nor spec_file set
    When the deployment resource config validators run
    Then validation fails with an "exactly one of" error

  Scenario: One-of validation rejects both present but empty
    Given a deployment resource config with both spec and spec_file empty
    When the deployment resource config validators run
    Then validation fails with an "exactly one of" error

  Scenario: One-of validation accepts only spec set
    Given a deployment resource config with only spec set
    When the deployment resource config validators run
    Then validation succeeds

  Scenario: One-of validation accepts only spec_file set
    Given a deployment resource config with only spec_file set
    When the deployment resource config validators run
    Then validation succeeds

  Scenario: One-of validation defers when spec is unknown (interpolated)
    Given a deployment resource config with spec unknown and spec_file null
    When the deployment resource config validators run
    Then validation succeeds

  Scenario: Schema round-trip preserves the deployment state
    Given a fully-populated deployment state
    When the state is written then read back through the schema
    Then the state round-trips without attribute loss
    And the schema exposes every DESIGN §5.2 attribute
