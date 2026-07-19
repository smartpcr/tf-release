@story-release:RELEASE-PROVIDER @phase-lab-acceptance-gate @stage-windows-and-linux-single-target-acceptance @setup-inline
Feature: Windows and Linux single-target acceptance matrix (DESIGN §18)
  Single-target acceptance for the two lab targets W1 (Windows Server 2022, WinRM
  https, node+.NET+vstest) and L1 (Linux VM, ssh). Every scenario ALWAYS runs the
  REAL DESIGN §18 lifecycle by driving the real internal/engine over the REAL
  `local` transport (actual powershell.exe / sh execution) against the REAL gate
  filesystem — nothing is faked and nothing is manually seeded. `engine.Deploy`
  really fetches the artifact over HTTP, verifies its checksum, extracts it,
  repoints the real `current` junction/symlink, and writes the real manifest +
  marker. On top of those REAL deploys the suite proves:

    * CAP — a real console_app deploy reaches its version and `current` tracks it,
    * IDP — a byte-identical re-apply is idempotent,
    * DRF — a mutated on-host marker is reported `drift` and a re-apply converges,
    * LCK — a real `.lock` is held and a contending acquire is refused ERR_LOCKED,
    * RBK — upgrade 1.0.0 -> 1.1.0 then a rollback re-apply converges on 1.0.0, and
    * DST — destroy --purge removes the whole tree and leaves no `.lock`.

  This reproducible core runs on the plain `go test -tags e2e` gate with NO
  external service and NO skip. When TF_ACC=1 AND the W1/L1 connection env is
  present (LABDEPLOY_ACC_W1_* / LABDEPLOY_ACC_L1_* / LABDEPLOY_ACC_ARTIFACT_BASE_URL
  + LABDEPLOY_ACC_SHA_*), each scenario ADDITIONALLY drives the full toolchain
  matrix against the REAL host over REAL WinRM/SSH; a missing var under TF_ACC=1
  FAILS the scenario so a mis-configured lab run cannot masquerade as green. The
  suite never sets TF_ACC itself.

  Scenario: Windows single-target matrix deploys WSV NOD NET CAP and releases the lock
    Given a labdeploy Windows single-target driven over the real local transport
    When the WSV NOD NET CAP deploy lifecycle runs on the real filesystem, plus the real W1 WinRM matrix under TF_ACC
    Then the console_app deploy reaches its version and the current handle is a real reparse point tracking the release
    And the node, .NET and vstest toolchains verify on the target and the service control manager is reachable
    And a byte-identical re-apply is idempotent
    And on-host drift is detected and a converging re-apply restores agreement
    And a contended acquire is refused with ERR_LOCKED
    And an upgrade then rollback converges back on the prior release
    And the ".lock" file is absent after the run

  Scenario: Linux console and drift behaviour matches DESIGN section 18
    Given a labdeploy Linux single-target driven over the real local transport
    When the CAP-linux console plus DRF DST IDP LCK RBK scenarios run on the real filesystem, plus the real L1 SSH matrix under TF_ACC
    Then the current symlink tracks the deployed release per DESIGN section 18
    And the console extraction and checksum run through a real POSIX shell and the current symlink uses "ln -sfn" per DESIGN section 18
    And a byte-identical re-apply is idempotent
    And console drift is detected and a re-apply converges
    And a contended acquire is refused with ERR_LOCKED
    And an upgrade then rollback converges back on the prior release
    And destroy purge removes the tree and the ".lock" file is absent after the run
