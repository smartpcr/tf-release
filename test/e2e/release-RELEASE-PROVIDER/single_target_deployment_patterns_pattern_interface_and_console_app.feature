@story-release:RELEASE-PROVIDER @phase-single-target-deployment-patterns @stage-pattern-interface-and-console-app @setup-inline
Feature: console_app pattern verbs over the Pattern interface
  The console_app pattern is files-only: Configure runs post_install then
  verify_command in the current release dir, Start/Stop are no-ops, and Status
  reports drift when the on-host release marker does not match the manifest
  version (DESIGN §9.3).

  Scenario: Console verify flow
    Given a console_app ReleaseCtx for app "sample-svc" version "1.0.0" on "windows"
    And the verify_command is "sample-svc.exe --version"
    When the Configure and Start scripts are generated over a fake transport
    Then Start ran no scripts on the transport
    And the generated verify_command script matches the committed golden "console_verify_windows.golden"

  Scenario Outline: Console status drift
    Given a console_app ReleaseCtx for app "sample-svc" version "1.1.0" on "windows"
    And a fake transport whose on-host release marker reports version "<marker_version>"
    When Status is computed via the fake transport
    Then the reported service status is "<status>"

    Examples:
      | marker_version | status |
      | 1.0.0          | drift  |
      | 1.1.0          | n/a    |
