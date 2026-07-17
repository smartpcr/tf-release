@story-release:RELEASE-PROVIDER @phase-project-scaffold-and-spec-engine @stage-spec-validation-and-variable-substitution @setup-inline
Feature: Spec Validation and Variable Substitution
  The spec engine rejects invalid Deployment specs with the stable
  ERR_SPEC_INVALID contract (naming the offending JSON path), substitutes
  ${var:NAME} tokens while preserving the $${var:NAME} escape, and refuses any
  files[].path that is absolute or contains ".." traversal — all in-process,
  with no external service.

  Scenario Outline: Validation matrix rejects invalid specs
    Given the invalid Deployment spec case "<id>"
    When the spec is validated
    Then validation fails with ERR_SPEC_INVALID naming "<fragment>"

    Examples:
      | id     | fragment                    |
      | VAL-01 | line                        |
      | VAL-02 | artifact.version            |
      | VAL-03 | pattern.type                |
      | VAL-04 | target.hosts                |
      | VAL-05 | artifact.checksum           |
      | VAL-06 | exactly one of              |
      | VAL-07 | unresolved variable HOST    |
      | VAL-08 | env var NEVER_SET_ENV_VAL08 |
      | VAL-09 | artifact.type               |

  Scenario: Variable substitution and escape
    Given a spec fragment containing "${var:X}" and an escaped "$${var:Y}"
    When it is substituted with variable X set to "lab-01"
    Then "${var:X}" is replaced with "lab-01"
    And the literal "${var:Y}" is preserved
    And substituting a spec with an unknown variable name yields ERR_SPEC_INVALID

  Scenario Outline: files path traversal rejected
    Given a valid base Deployment spec
    When files[0].path is set to "<path>"
    Then the spec validation "<outcome>"

    Examples:
      | path            | outcome  |
      | config/app.json | passes   |
      | /etc/x          | rejected |
      | C:\x            | rejected |
      | C:/x            | rejected |
      | ../escape       | rejected |
      | a/../../escape  | rejected |
