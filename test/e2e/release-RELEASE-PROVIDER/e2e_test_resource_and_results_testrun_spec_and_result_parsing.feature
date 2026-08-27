@story-release:RELEASE-PROVIDER @phase-e2e-test-resource-and-results @stage-testrun-spec-and-result-parsing @setup-inline
Feature: TestRun spec and result parsing
  As the release engine
  I need each runner.type to expand into the exact command line pinned by the
  committed goldens (including the results-dir logger flags), and the
  pass_criteria evaluation to reflect exit_codes and min_pass_rate while a
  results.format of none yields -1 sentinel counters (DESIGN §7.2, §7.3, §5.3).

  Scenario: Runner expansion golden
    Given a TestRun spec for each runner type
    When the runner command is expanded
    Then it matches the committed golden including results-dir flags

  Scenario: Pass criteria evaluation
    Given parsed counters plus pass criteria for each case
    When the pass criteria are evaluated
    Then passed reflects exit_codes and min_pass_rate and format none yields -1 counters
