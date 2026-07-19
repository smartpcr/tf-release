@story-release:RELEASE-PROVIDER @phase-docker-examples-and-packaging @stage-docker-container-pattern @setup-inline
Feature: Docker container pattern D-step generation and preflight
  The docker_container pattern renders its D1..D7 lifecycle scripts (login, tag
  pull, digest pull, rm -f, run, rollback run, logout) deterministically so they
  can be pinned to committed goldens, and its Preflight gate fails loudly when
  the docker daemon is unavailable (DESIGN §8.1 / §9.6).

  Scenario: Docker run script golden
    Given a docker_container spec for image "registry.example.com/app" tagged "2.0.0" with registry auth
    When the D1..D7 scripts are generated
    Then the FETCH script matches the committed golden "docker_pull_linux.golden"
    And the START script matches the committed golden "docker_run_linux.golden"
    And the digest pull script matches the committed golden "docker_pull_digest.golden"
    And the rollback run script matches the committed golden "docker_run_rollback.golden"

  Scenario: Docker preflight fail
    Given a docker_container spec for image "registry.example.com/app" tagged "2.0.0" with registry auth
    And "docker version" fails via a fake transport with a scripted Result queue
    When Preflight runs
    Then it yields a StepError coded "ERR_PREFLIGHT" at step "PREFLIGHT"
