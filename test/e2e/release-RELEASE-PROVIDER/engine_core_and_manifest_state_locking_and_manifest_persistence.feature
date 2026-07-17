@story-release:RELEASE-PROVIDER @phase-engine-core-and-manifest-state @stage-locking-and-manifest-persistence @setup-inline
Feature: Locking and manifest persistence
  As the release engine
  I need the target lock primitive to fail fast on a fresh peer-held lock
  (ERR_LOCKED naming the owner) yet atomically take over a proven-stale lock
  with a WARN diagnostic, and I need a manifest to survive a write/re-read
  round-trip through the transport with every field — including last_operation —
  unchanged (DESIGN §10.4, §13). Both scenarios drive the REAL engine code
  through a fake Transport whose in-memory filesystem answers the scripted lock
  and manifest primitives.

  Scenario: Lock fresh vs stale
    Given a fake transport holding a fresh .lock owned by "peer-alpha" and another holding an aged .lock owned by "peer-bravo"
    When AcquireLock runs against each
    Then the fresh case yields ERR_LOCKED naming the owner and the aged case overwrites with a WARN diagnostic naming the stale owner

  Scenario: Manifest round-trip
    Given a fully populated manifest struct including last_operation
    When it is written then re-read through a fake transport
    Then all fields including last_operation survive unchanged
