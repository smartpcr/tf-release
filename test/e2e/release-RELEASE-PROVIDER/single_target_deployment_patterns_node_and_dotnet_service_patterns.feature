@story-release:RELEASE-PROVIDER @phase-single-target-deployment-patterns @stage-node-and-dotnet-service-patterns @setup-inline
Feature: Node and Dotnet service patterns generate correct S4/S5 scripts
  The dotnet_api and node_web_app patterns must render deterministic remote
  scripts. The dotnet_dll launcher must emit the normative quoted binPath, and
  node install_deps must fail with ERR_SERVICE_INSTALL naming the missing
  lockfile.

  Scenario: Dotnet binPath golden
    Given a dotnet_api release with launcher "dotnet_dll" and dll "WebApi.dll"
    And the dotnet executable is "C:\Program Files\dotnet\dotnet.exe" with args "--environment Production"
    When the S4 configure script is generated
    Then the generated S4 script matches the committed golden "dotnet_dll_s4_configure.golden"
    And the S4 script contains the normative quoted dotnet binPath

  Scenario: Node install_deps preflight
    Given a node_web_app release with install_deps enabled
    And the release directory has no "package-lock.json"
    When node Preflight and the install_deps step run via the fake transport
    Then it fails with code "ERR_SERVICE_INSTALL"
    And the failure names the lockfile "package-lock.json"
