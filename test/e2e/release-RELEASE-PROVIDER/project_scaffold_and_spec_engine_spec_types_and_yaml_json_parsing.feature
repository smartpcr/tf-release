@story-release:RELEASE-PROVIDER @phase-project-scaffold-and-spec-engine @stage-spec-types-and-yaml-json-parsing @setup-inline
Feature: Spec Types and YAML JSON Parsing
  The spec engine parses Deployment specs authored in either YAML or JSON into
  identical in-memory structs, and decodes the pattern discriminated union so the
  concrete member named by pattern.type is populated while the others stay nil.

  Scenario: YAML equals JSON
    Given the same Deployment spec authored in YAML and in JSON
    When both are parsed
    Then they produce identical in-memory structs
    And they produce the same canonical spec hash

  Scenario: Pattern union decode
    Given a Deployment spec with "pattern.type: windows_service"
    When it is parsed
    Then the windows_service concrete struct is populated
    And every other pattern union member is nil
