@story-release:RELEASE-PROVIDER @phase-project-scaffold-and-spec-engine @stage-repository-bootstrap-and-toolchain @setup-inline
Feature: Repository Bootstrap and Toolchain
  The terraform-provider-labdeploy repository must build to a statically linked
  binary, pass lint cleanly, and serve a Terraform plugin protocol v6 provider at
  the advertised registry address.

  Scenario: Module builds
    Given the existing repo
    When "make build" runs on the gate host
    Then binary "terraform-provider-labdeploy_v0.1.0" is produced with CGO_ENABLED=0

  Scenario: Lint clean
    Given the repo
    When "golangci-lint run" runs on the gate host
    Then it exits 0 with no findings

  Scenario: Provider advertises protocol v6
    Given main.go
    When the provider is served under the terraform-plugin-testing harness
    Then it advertises plugin protocol v6 and address "registry.local/smartpcr/labdeploy"
