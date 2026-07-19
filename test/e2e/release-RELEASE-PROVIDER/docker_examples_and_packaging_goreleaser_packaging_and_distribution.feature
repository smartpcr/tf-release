@story-release:RELEASE-PROVIDER @phase-docker-examples-and-packaging @stage-goreleaser-packaging-and-distribution @setup-inline
Feature: GoReleaser packaging and distribution
  The committed .goreleaser.yml plus the tools/zipbin build hook must package the
  provider into the filesystem-mirror zip matrix, and every emitted binary must be
  a statically linked, cgo-free build.

  Scenario: Build matrix
    Given the committed goreleaser config
    When the goreleaser snapshot build runs on the gate host
    Then a zip is produced for "linux_amd64"
    And a zip is produced for "windows_amd64"
    And each zip is named for its target and contains the provider binary

  Scenario: No cgo
    Given the built provider binaries
    When each binary is inspected on the gate host
    Then every binary is statically built with CGO_ENABLED=0
