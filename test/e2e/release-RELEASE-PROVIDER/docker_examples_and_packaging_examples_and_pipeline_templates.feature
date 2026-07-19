@story-release:RELEASE-PROVIDER @phase-docker-examples-and-packaging @stage-examples-and-pipeline-templates @setup-inline
Feature: Examples and Pipeline Templates — E2E
  The committed examples/specs/*.yaml fixtures and examples/pipelines/*.yml
  templates are the public, copy-paste surface of the provider. This suite reads
  them straight from disk and proves, in-process (no external service), that:
    - every committed deployment/test spec parses and validates through the real
      spec engine (merge-then-validate, mirroring the example provider
      default_target), and
    - both pipeline templates are well-formed YAML and carry the
      always()/condition: always() guards on their actual publish/upload steps so
      results and logs upload even when a rollout fails.

  Scenario: Committed example specs parse and validate
    Given the committed example specs under "examples/specs"
    When each committed spec is run through the spec validator
    Then every committed spec parses and validates without error
    And both a Deployment and a TestRun fixture are covered

  Scenario: Committed pipeline templates are well-formed with always() publish guards
    Given the committed pipeline templates under "examples/pipelines"
    When each committed pipeline file is parsed with a YAML parser
    Then both pipeline templates are well-formed YAML
    And the GitHub workflow uploads results guarded by "always()"
    And the Azure pipeline publishes results guarded by "always()"
