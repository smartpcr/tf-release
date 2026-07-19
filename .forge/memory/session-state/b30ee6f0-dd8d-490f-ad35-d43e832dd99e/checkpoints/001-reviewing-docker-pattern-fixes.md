<overview>
The user is repeatedly requesting read-only scoring of the Docker Container Pattern workstream against an authoritative changed-file list and normative `.forge-attachments/DESIGN.md`. The approach has been to read the attachment first, inspect every changed file, compare implementation and tests to Stage 8.1 requirements and prior feedback, run the Go suite, then return only strict fenced JSON with score, dimensions, verdict, and structured notes.
</overview>

<history>
1. Initial review of 8 changed test/plan files
   - Read the entire design attachment and all changed files.
   - Inspected the existing Docker pattern and engine integration.
   - Ran `go test ./internal/pattern ./internal/engine` and `go test ./...`; all passed.
   - Scored 73/100, identifying missing engine-level rollback/manifest coverage, unsafe shell rendering, and incorrect rollback `LD_VERSION`.

2. Review after branch expanded to 11 files
   - Verified new changes in `engine.go`, `docker_container.go`, and engine tests.
   - Confirmed manifest image-ID durability, health-triggered rollback, safe environment quoting, and previous-version `LD_VERSION` were addressed.
   - Ran the full suite successfully.
   - Scored 82/100, flagging ignored pre-run inspect errors, inconsistent post-run inspect failure state, same-version config no-op behavior, and incomplete shell quoting.

3. Next iteration
   - Verified fixes for all four prior issues:
     - Pre-run inspect transport errors abort before removal.
     - Post-run inspect failures persist failed state.
     - Config fingerprints prevent same-version configuration changes from being skipped.
     - Shell quoting covers registry, image, restart policy, and lifecycle container names.
   - Ran `go test ./...`; all passed.
   - Scored 88/100.
   - Remaining findings were:
     1. Docker CLI inspect failures were still treated as container absence.
     2. Rollback restored the old image using the new desired Docker configuration.
     3. `localhost/repository` was not recognized as a registry.

4. Current iteration in progress
   - Re-read the complete normative design attachment.
   - Opened all 11 authoritative changed files read-only.
   - Current visible code still appeared to contain the three prior issues:
     - `CurrentImageID` masks all nonzero inspect exits.
     - Rollback context is built from the desired spec.
     - Registry detection only checks `.` or `:`, not `localhost`.
   - The latest commit/diff and full test suite have not yet been checked for this iteration.
</history>

<work_done>
No repository files were modified, committed, or pushed.

Completed:
- [x] Read the normative design attachment before each scoring pass.
- [x] Opened every authoritative changed file.
- [x] Compared plan checkboxes and tests to actual implementation.
- [x] Reviewed D1-D7, preflight, rollback, manifest, quoting, and idempotency behavior.
- [x] Ran the full Go test suite successfully in prior iterations.
- [ ] Finish the current iteration’s latest-commit inspection and final score.

Prior verified improvements:
- Manifest records `extra.image_id`.
- Engine-level health rollback is covered.
- Rollback uses previous `LD_VERSION`.
- Docker run values are shell-quoted.
- Config fingerprints handle same-version mutable changes.
- Pre/post-run transport inspection errors have explicit engine behavior.
</work_done>

<technical_details>
- Normative Docker flow is DESIGN §9.6:
  - D1 login
  - D2 tag/digest pull
  - D3 inspect current image ID
  - D4 `rm -f`
  - D5 run with restart/ports/env/volumes
  - D6 health failure rollback to old image ID and health recheck
  - D7 logout
- Successful manifests must record the running image ID in `manifest.extra`.
- Rollback consistency follows DESIGN §10.2 and Decision D11: failed updates must restore the prior machine state because Terraform retains prior state.
- Current config idempotency uses `DockerContainer.ConfigFingerprint`, stored as `manifest.extra["config_hash"]`.
- Shell arguments use target-specific single quoting through `q`, which delegates to `psq` or `shq`.
- `run_args` remain intentionally raw/operator-authored.
- Important unresolved behavior from the last completed review:
  - `CurrentImageID` scripts turn any nonzero Docker CLI exit into empty output, masking daemon/permission failures as absence.
  - Rollback uses `releaseCtxVersion(s, p, prev)`, where `s` is the failed desired spec, so ports, volumes, environment, restart policy, and run arguments may remain the rejected values.
  - Registry parsing fails to classify `localhost/repo` as a custom registry.
- No generator `grep -rnF` claim block has been supplied in the visible user prompts, so no literal stale-reference re-grep was applicable.
- Environment: Windows repository at `C:\forge\release\.worktree\stage-8.1-docker-container-pattern`.
</technical_details>

<important_files>
- `.forge-attachments/DESIGN.md`
  - Normative specification.
  - Key sections: §6.3 Docker artifact, §6.4 Docker pattern, §9.6 D1-D7, §10.2 rollback, §11 secrets, §12 errors.

- `docs/stories/release-RELEASE-PROVIDER/implementation-plan.md`
  - Stage 8.1 implementation and test checkboxes.
  - Relevant lines: 411-423.

- `internal/pattern/docker_container.go`
  - Core Docker command generation, preflight, pull/login/logout, inspect, lifecycle, quoting, and config fingerprint.
  - Key areas:
    - Pull/registry detection: approximately lines 60-113.
    - `CurrentImageID`: approximately lines 115-131.
    - Run argument quoting: approximately lines 133-197.
    - Status/stop/uninstall: approximately lines 205-250.
    - `ConfigFingerprint`: approximately lines 252-290.

- `internal/engine/engine.go`
  - Docker orchestration, idempotency, rollback, and manifest writes.
  - Key area: `deployDocker`, approximately lines 484-677.
  - Rollback context around lines 552-565 remains especially important.

- `internal/engine/docker_steplog_test.go`
  - Fake Docker target and engine-level tests for image recording, rollback, inspect errors, and same-version config changes.

- `internal/pattern/docker_container_test.go`
  - Golden tests, preflight test, quoting tests, D3 test, and fingerprint tests.

- `internal/pattern/testdata/docker_*.golden`
  - Expected Linux/Windows pull and run scripts, including digest and rollback forms.
</important_files>

<next_steps>
1. Inspect `git log`, `git show --stat`, and `git diff HEAD^ HEAD` for the current iteration to determine whether a newer commit addresses the three prior findings.
2. Re-check exact current line references in `docker_container.go`, `engine.go`, and tests.
3. Run `go test ./...`.
4. Decide whether the three prior findings remain:
   - inspect exit classification,
   - restoration of prior Docker configuration,
   - localhost registry detection.
5. Return only a fenced strict JSON review with score equal to dimension totals, structured improvements, unchecked remaining issues, and a pass only if score is at least 90.
</next_steps>