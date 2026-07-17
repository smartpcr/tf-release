@story-release:RELEASE-PROVIDER @phase-engine-core-and-manifest-state @stage-health-checks-and-log-collection @setup-inline
Feature: Health checks and log collection
  As the release engine
  I need the per-type health-probe scripts (win/linux) to match the committed
  goldens and encode expect_status / expect_body_regex, and the TRX/JUnit result
  parsers to report the exact total/passed/failed/skipped counters pinned by the
  committed fixtures (DESIGN §6.5, §7, §8.5).

  Scenario: Health script generation
    Given the health_check types http-default, http-expect, tcp, exec and none
    When the probe scripts are generated for windows and linux
    Then they match the committed golden fixtures and encode expect_status and expect_body_regex logic

  Scenario: TRX and JUnit counters
    Given the committed TRX and JUnit result fixtures
    When they are parsed
    Then the total, passed, failed and skipped counters match the expected values
