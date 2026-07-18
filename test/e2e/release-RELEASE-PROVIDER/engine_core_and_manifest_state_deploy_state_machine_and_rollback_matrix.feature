@story-release:RELEASE-PROVIDER @phase-engine-core-and-manifest-state @stage-deploy-state-machine-and-rollback-matrix @setup-inline
Feature: Deploy state machine and rollback matrix
  As the release engine
  I need the Deploy state machine to execute its fixed step order, short-circuit
  idempotent no-ops, and roll back to exactly the state pinned by DESIGN §10.2 at
  each mutating step — proven in-process against a fake scripted transport with a
  tflog test sink asserting the structured step fields.

  Scenario Outline: Rollback matrix row
    Given a fake transport scripting a failure at the "<step>" step of a "<mode>" deploy
    When the deploy runs
    Then the resulting on-target state matches the "<outcome>" rollback-matrix row

    Examples: fresh install
      | mode  | step     | outcome        |
      | fresh | stage    | fresh-wiped    |
      | fresh | fetch    | fresh-wiped    |
      | fresh | checksum | fresh-wiped    |
      | fresh | extract  | fresh-wiped    |
      | fresh | render   | fresh-wiped    |
      | fresh | switch   | fresh-cleaned  |
      | fresh | configure| fresh-cleaned  |
      | fresh | start    | fresh-cleaned  |
      | fresh | health   | fresh-cleaned  |

    Examples: update in place
      | mode   | step     | outcome         |
      | update | stage    | update-wiped    |
      | update | fetch    | update-wiped    |
      | update | checksum | update-wiped    |
      | update | extract  | update-wiped    |
      | update | render   | update-wiped    |
      | update | switch   | update-restored |
      | update | configure| update-restored |
      | update | start    | update-restored |
      | update | health   | update-restored |

  Scenario: Idempotent short-circuit
    Given a target already at the spec version with a matching checksum and a healthy service
    When the deploy runs
    Then it returns a NO-OP with no FETCH or SWITCH step logged

  Scenario: Single-host step order logged
    Given a successful single-host deploy captured through a tflog test sink
    When the deploy completes
    Then the logged step values appear in the fixed VALIDATE..UNLOCK order
    And every logged step carries app, host, step, version and a numeric duration_ms

  Scenario: Conditional and per-host steps logged
    Given a multi-host update where one host has the release cached and a rollback repeats SWITCH captured through a tflog test sink
    When the deploy runs
    Then the cached host logs neither FETCH nor CHECKSUM while the other host logs both
    And each host logs SWITCH at least twice and every record carries the full app/host/step/version/duration_ms field set
