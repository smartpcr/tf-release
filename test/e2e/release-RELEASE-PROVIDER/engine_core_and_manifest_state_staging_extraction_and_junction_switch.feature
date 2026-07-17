@story-release:RELEASE-PROVIDER @phase-engine-core-and-manifest-state @stage-staging-extraction-and-junction-switch @setup-inline
Feature: Staging extraction and junction switch
  As the release engine
  I need the staging→release extraction scripts, the current-junction switch
  scripts (with the exit-42 ERR_SWITCH guard), and the retention prune to behave
  exactly as pinned by DESIGN §9.1 and §10.1 step 13, on both Windows and Linux.

  Scenario: Switch and extract scripts golden
    Given a ReleaseCtx for windows and linux
    When the extract and junction-switch scripts are generated for each OS
    Then they match the committed golden fixtures including the exit-42 guard

  Scenario: Prune keeps previous
    Given a releases set with keep_releases 2 and a previous_version
    When prune runs against a fake transport
    Then the oldest releases are removed and the previous_version is retained
