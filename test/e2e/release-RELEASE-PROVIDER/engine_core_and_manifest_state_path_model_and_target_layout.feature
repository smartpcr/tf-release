@story-release:RELEASE-PROVIDER @phase-engine-core-and-manifest-state @stage-path-model-and-target-layout @setup-inline
Feature: Path Model and Target Layout
  The layout package derives the on-target release tree (DESIGN §9.1) with the
  identical shape on both OSes, differing only in the separator (`\` on windows,
  `/` on linux). From a ReleaseCtx it renders an idempotent, deterministic
  directory-creation script per OS that matches the committed golden fixtures
  under internal/layout/testdata.

  Scenario: Path derivation both OSes
    Given install_root and app name for windows and linux
    When the paths are computed for each OS
    Then the separators and layout match DESIGN section 9.1 exactly

  Scenario: Layout script golden
    Given a ReleaseCtx for windows and linux
    When the dir-creation script is generated for each OS
    Then it matches the committed golden fixture for each OS
