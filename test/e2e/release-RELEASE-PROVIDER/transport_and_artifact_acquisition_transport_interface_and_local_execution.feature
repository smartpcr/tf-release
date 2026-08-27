@story-release:RELEASE-PROVIDER @phase-transport-and-artifact-acquisition @stage-transport-interface-and-local-execution @setup-inline
Feature: Transport Interface and Local Execution
  The local transport runs Exec + Upload + Download against the gate host's own
  shell (transport local, gated on runtime.GOOS). A file survives an
  Upload/Download round-trip, Exec surfaces application exit codes in Result
  while returning a transport error only on transport failure, and Connect
  rejects a target whose declared os does not match the runner before any
  command executes.

  Scenario: Local round-trip
    Given a local transport for the gate OS
    When Connect succeeds and a file is uploaded then downloaded
    Then the file survives the round-trip byte-for-byte
    And Exec returns the application exit code in Result with no transport error

  Scenario: OS mismatch rejected
    Given a local transport whose target os does not match the runner
    When Connect runs
    Then Connect errors before executing any command
