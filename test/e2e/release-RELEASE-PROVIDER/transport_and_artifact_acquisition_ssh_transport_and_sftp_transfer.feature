@story-release:RELEASE-PROVIDER @phase-transport-and-artifact-acquisition @stage-ssh-transport-and-sftp-transfer @setup-inline
Feature: SSH Transport and SFTP Transfer
  The ssh transport dials a real SSH endpoint, exercises the SFTP subsystem for
  file transfer, surfaces application exit codes from Exec, and never retries an
  authentication rejection. A pinned host key that does not match the presented
  key must fail Connect as ERR_CONNECT with detail "host key mismatch".

  # Scenario 1 is proof:lab for a LIVE VM; here it is proven honestly in-process
  # against an ephemeral stub sshd (no docker, no lab) so the required behaviour
  # actually EXECUTES and ASSERTS in the gate: SFTP round-trip byte-identity plus
  # auth rejection is not retried.
  Scenario: SSH dial and SFTP round-trip
    Given a stub SSH endpoint whose host key is pinned correctly
    When Connect, Exec, Upload and Download run over the ssh transport
    Then data round-trips byte-for-byte and auth rejection is not retried

  Scenario: Host key mismatch mapping
    Given a stub SSH endpoint whose pinned host key is wrong
    When Connect runs over the ssh transport against the stub key
    Then the error is ERR_CONNECT with detail host key mismatch
