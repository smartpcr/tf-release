@story-release:RELEASE-PROVIDER @phase-single-target-deployment-patterns @stage-windows-service-pattern @setup-inline
Feature: Windows Service Pattern S-step and WinSW generation
  The windows_service pattern generates deterministic S4 configure/S5 env
  scripts and, when wrapped by WinSW, a WinSW xml. Each must reproduce the
  committed golden fixtures under internal/pattern/testdata (DESIGN §9.2).

  Scenario: S4 fresh vs update golden
    Given a windows_service spec for "PaymentsSvc" with recovery and env injection
    When the S4 scripts are generated for fresh install and update
    Then the S4 configure script matches the golden "winsvc_s4_configure.golden"
    And the S5 env-injection script matches the golden "winsvc_s5_env.golden"
    And the S4 script contains both the fresh create and update config branches
    And the S4 script installs the recovery actions

  Scenario: WinSW xml golden
    Given a windows_service spec with wrapper "winsw"
    When the WinSW xml is generated
    Then the WinSW xml matches the golden "winsvc_winsw.xml.golden"
    And the WinSW xml contains the stopwait entry "<stopwait>45sec</stopwait>"
    And the WinSW xml contains the env entry "ASPNETCORE_ENVIRONMENT"
