@story-release:RELEASE-PROVIDER @phase-project-scaffold-and-spec-engine @stage-canonical-json-and-spec-hashing @setup-inline
Feature: Canonical JSON and Spec Hashing
  The spec engine derives spec_hash from canonical JSON (sorted keys) so the hash
  is invariant to author key order (T2 preservation), and a version_override both
  rewrites artifact.version and changes the resulting spec_hash.

  Scenario: Hash stable across key order
    Given a committed canonical-JSON fixture rendered with keys in different orders
    When each ordering is hashed
    Then the spec hash is byte-identical across orderings

  Scenario: Override changes hash
    Given a base Deployment spec
    When a version_override is applied
    Then artifact.version reflects the override
    And the spec hash differs from the base spec hash
