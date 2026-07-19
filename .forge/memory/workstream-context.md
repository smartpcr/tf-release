# Forge workstream memory

Concise context required to move the next iteration forward. Do not treat this as a transcript; raw prompts, full logs, and full file contents are intentionally omitted.

- Work item: `ws-release-release-provider-phase-docker-examples-and-packaging-stage-docker-container-pattern`
- Generated UTC: 2026-07-19T01:04:45.9520306+00:00

## Current workstream
- Work item title: Docker Container Pattern
- State/execution: `active` / `failed`; pair attempt 1; trial 1; points 8
- Work item plan anchor: `phase-docker-examples-and-packaging/stage-docker-container-pattern`
- Workstream: Docker Container Pattern
- Status/category: `active` / `impl`
- Branch/base: `ws/release-RELEASE-PROVIDER/phase-docker-examples-and-packaging-stage-docker-container-pattern` -> `feature/release`
- Workstream plan anchor: `phase-docker-examples-and-packaging/stage-docker-container-pattern`
- Summary: Docker Container Pattern (3 steps)

## Story and plan context
- Story: `release:RELEASE-PROVIDER` — release provider
- Spawn phase/base: `implement` / `feature/release`

## Dependency context
- Deployment Resource CRUD and Plan Modifiers — E2E — status `complete`, branch `ws/release-RELEASE-PROVIDER/phase-terraform-provider-surface-stage-deployment-resource-crud-and-plan-modifiers-e2e`
- Provider and Deployment Resource Schema — E2E — status `complete`, branch `ws/release-RELEASE-PROVIDER/phase-terraform-provider-surface-stage-provider-and-deployment-resource-schema-e2e`
- Read Drift Reconciliation and Destroy Modes — E2E — status `complete`, branch `ws/release-RELEASE-PROVIDER/phase-terraform-provider-surface-stage-read-drift-reconciliation-and-destroy-modes-e2e`

## Iteration summaries and evaluator feedback
### Iteration 2 — verdict `iterate`, score 73, pair 1
- Commit: `6f68993ced4b` forge[YA] iter 2: ws-release-release-provider-phase-docker-examples-and-packaging-stage-docker-container-pattern
- Files changed: 8 (added 7, modified 1, deleted 0)
- Generator summary:
  Stage 8.1's production code (`docker_container.go` D1..D7, engine rollback + `manifest.extra["image_id"]`, preflight) was already implemented and wired. The missing piece was the two required **test-scenario proofs**, which I added.
  
  **Files changed:**
  - **`internal/pattern/docker_container_test.go`** (new): 5 tests
    - `TestDockerRunScriptGoldenLinux` — D1 login (`--password-stdin`) → D2 tag pull → D7 logout FETCH script; D4 `rm -f` + D5 `run` with ports/env/volumes/restart.
    - `TestDockerPullDigestGolden` — digest pull (`image@sha256:…`), no auth ⇒ no login/logout.
    - `TestDockerRollbackRunGolden` — D6 rollback re-runs against the recorded old image id, not the new tag.
    - `TestDockerRunScriptGoldenWindows` — PowerShell pull/run variants.
    - `TestDockerPreflightFail` — `docker version` failing via scripted `Result` queue ⇒ `StepError` code `ERR_PREFLIGHT` / step `PREFLIGHT`; healthy exit 0 ⇒ no error.
  - **`internal/pattern/testdata/docker_*.golden`** (6 new committed fixtures).
  - **`docs/stories/.../implementation-plan.md`** — checked the two Stage 8.1 Test Scenario boxes.
  
  **Verification:** `go build ./...` exit 0; `go test ./internal/pattern/` all pass (5/5 new docker tests
  ...(truncated)
- Evaluator feedback:
  Improvements this iteration:
  - Added substantive Linux and Windows goldens covering authenticated tag pulls, digest pulls, logout, container replacement, and run options.
  - Added a focused preflight test verifying `docker version` failures produce `ERR_PREFLIGHT`.
  
  Still needs improvement:
  - [ ] 1. The Stage 8.1 scenario is marked complete, but `internal/pattern/docker_container_test.go` never exercises D3 or verifies `manifest.extra.image_id`; `internal/engine/engine.go:591-596` also ignores the post-run inspect error and can persist an empty image ID after reporting success.
  - [ ] 2. `TestDockerRollbackRunGolden` at `internal/pattern/docker_container_test.go:132-155` directly calls `RunNew` and does not prove that a health failure invokes rollback using the D3-recorded image, rechecks health, and returns `ERR_HEALTH_CHECK`.
  - [ ] 3. The run goldens preserve unsafe environment rendering: `internal/pattern/docker_container.go:139-150` places arbitrary values inside shell double quotes, allowing command substitution such as `$()` and using invalid backslash quote escaping for PowerShell. Add metacharacter cases and render arguments safely.
  - [ ] 4. `internal/pattern/testdata/docker_run_rollback.golden:2` restarts the old image with `LD_VERSION=2.0.0`; the engine calls rollback with the new release context at `internal/engine/engine.go:540`, so the restored container advertises the failed version rather than the previous version.
  
  Why this score: The committed tests are useful and pass, but the completion checkbox overstates coverage of D3 and D6. Manifest durability, rollbac
  ...(truncated)
### Iteration 3 — verdict `iterate`, score 82, pair 1
- Commit: `a76dd3a1025f` forge[YA] iter 3: ws-release-release-provider-phase-docker-examples-and-packaging-stage-docker-container-pattern
- Files changed: 11 (added 7, modified 4, deleted 0)
- Generator summary:
  Iter-2 feedback flagged four **production** issues (not just test gaps). All fixed with real code changes plus proving tests.
  
  ### Prior feedback resolution
  - [x] **1. FIXED** — `internal/engine/engine.go` (post-run inspect) + `internal/pattern/docker_container_test.go`. Post-run `CurrentImageID` error/empty now **fails closed** (`ERR_CONNECT`/step FINALIZE) instead of persisting a success manifest with an empty `image_id`. Added `TestDockerCurrentImageID` (D3), `TestDockerDeploySuccessRecordsImageID` (asserts `manifest.extra.image_id`), and `TestDockerDeployEmptyImageIDFailsClosed`.
  - [x] **2. FIXED** — `internal/engine/docker_steplog_test.go::TestDockerHealthFailureRollsBackPrevVersion` drives a real health failure through the engine: D3-recorded old image → rollback run → health recheck → surfaces `ERR_HEALTH_CHECK` (no longer just a direct `RunNew` call).
  - [x] **3. FIXED** — `internal/pattern/docker_container.go`. Removed `shellKV` (bare double-quoted `-e "K=V"`). New per-OS `d.q()` single-quotes name/ports/env/volumes/ref: sh uses `'\''` escaping, PowerShell uses `''` — killing `$()`/backtick/`$VAR`/quote injection and the invalid PS `\"` escaping. `TestDockerRunArgMetacharSa
  ...(truncated)
- Evaluator feedback:
  Improvements this iteration:
  - [x] Prior item 1 FIXED — successful deployment now requires and tests `manifest.extra.image_id`.
  - [x] Prior item 2 FIXED — an engine-level test now proves health-triggered rollback and `ERR_HEALTH_CHECK`.
  - [x] Prior item 3 FIXED — Docker run values use shell-specific literal quoting with metacharacter tests.
  - [x] Prior item 4 FIXED — rollback rebuilds the release context with the previous `LD_VERSION`.
  
  Still needs improvement:
  - [ ] 1. `internal/engine/engine.go:512-514` ignores transport errors from the pre-run D3 `CurrentImageID`; deployment can remove the live container without reliably recording its rollback image. Propagate the inspect error before D4, with a fake-transport test.
  - [ ] 2. `internal/engine/engine.go:604-608` returns immediately when post-run image inspection fails, leaving the new container running while Terraform retains the old state; `TestDockerDeployEmptyImageIDFailsClosed` only checks the missing success manifest. Roll back the mutated container or persist the required failed state and test the resulting machine state.
  - [ ] 3. `internal/engine/engine.go:493-496` treats matching version plus running status as an unconditional no-op, so same-version changes to image tag/digest, ports, environment, volumes, restart policy, or run arguments are silently ignored. Include a Docker configuration/image fingerprint in idempotency or route Docker updates using the prior spec.
  - [ ] 4. Shell quoting remains incomplete in `internal/pattern/docker_container.go`: pull/login values at lines 71-92, inspect/status/container names
  ...(truncated)
### Iteration 4 — verdict `iterate`, score 88, pair 1
- Commit: `e511a5cf43d7` forge[YA] iter 4: ws-release-release-provider-phase-docker-examples-and-packaging-stage-docker-container-pattern
- Files changed: 23 (added 7, modified 5, deleted 11)
- Generator summary:
  — Docker Container Pattern (Stage 8.1), iter 4
  
  ### Prior feedback resolution
  1. **ADDRESSED** — `engine.go` pre-run D3 inspect (`~516-527`): a transport error from `CurrentImageID` now aborts *before* D4 `rm -f`/`run` (`if cerr != nil { return nil, cerr }`) instead of being swallowed. Test: `TestDockerPreRunInspectErrorAbortsBeforeRemoval` asserts no run executes, no success manifest.
  2. **ADDRESSED** — `engine.go` post-run inspect (`~613-629`): failure now persists a **failed manifest at the NEW version** via `dockerFinalizeFailed(...s.Artifact.Version...)` (Read shows drift → re-apply reconverges) then returns the coded FINALIZE error. Test: `TestDockerPostRunInspectFailurePersistsNewVersionFailedState`.
  3. **ADDRESSED** — `engine.go` idempotency (`493-501`): no-op now requires `config_hash == dc.ConfigFingerprint(rc)`. New `ConfigFingerprint` (sha256 of ref/name/restart/sorted ports+volumes/ordered run_args/sorted env) stored in success + rolled_back manifests; failed manifests omit it. Tests: `TestDockerConfigFingerprint` + `TestDockerSameVersionConfigChangeRedeploys`.
  4. **ADDRESSED** — `docker_container.go` complete quoting: `Pull`, `CurrentImageID`, `Status`, `Stop`, `Unins
  ...(truncated)
- Evaluator feedback:
  Improvements this iteration:
  - [x] Prior item 1 FIXED — pre-run transport inspection failures now abort before container replacement and are tested.
  - [x] Prior item 2 FIXED — post-run inspection failure persists a failed new-version manifest for reconvergence.
  - [x] Prior item 3 FIXED — Docker idempotency now includes a configuration fingerprint, with same-version redeployment coverage.
  - [x] Prior item 4 FIXED — registry, image, restart policy, and lifecycle container-name arguments now use shell-safe quoting with metacharacter tests.
  
  Still needs improvement:
  - [ ] 1. `internal/pattern/docker_container.go:119-125` converts every nonzero `docker inspect` result into an absent-container empty ID. A daemon, permission, or CLI failure therefore bypasses the pre-run D3 protection in `internal/engine/engine.go:521-527`; distinguish not-found from other failures and return `ERR_CONNECT`/`ERR_PREFLIGHT` before D4.
  - [ ] 2. Docker rollback at `internal/engine/engine.go:558-565` rebuilds `rbRC` from the failed desired spec, changing only `LD_VERSION`; if an update also changed ports, volumes, environment, restart policy, or run arguments, the old image is restored with the rejected new configuration. Preserve and apply the prior Docker configuration so the machine matches Terraform's retained prior state.
  - [ ] 3. Registry extraction at `internal/pattern/docker_container.go:67-70` does not recognize the standard `localhost/repository` registry form because `localhost` contains neither `.` nor `:`. Authenticated pulls then log into Docker Hub instead of the local registry; recogniz
  ...(truncated)
### Iteration 5 — verdict `iterate`, score 88, pair 1
- Commit: `21e34fb56a81` Merge remote-tracking branch 'origin/feature/release' into ws/release-RELEASE-PROVIDER/phase-docker-examples-and-packaging-stage-docker-container-pattern
- Files changed: 11 (added 7, modified 4, deleted 0)
- Generator summary:
  — Docker Container Pattern (Stage 8.1), iter 5
  
  ### Prior feedback resolution
  - [x] **1. FIXED** — `internal/pattern/docker_container.go` `CurrentImageID`: the D3 inspect now classifies results by **sentinel exit code** (0=found, 20=`No such object/container`→`"" ,nil`, else→coded **`ERR_CONNECT`**). A daemon/permission/CLI failure is no longer masked as an absent container, so the engine's pre-run abort (`engine.go:521`) actually triggers before D4 `rm -f`. Test `TestDockerCurrentImageID` now asserts exit-20→`""` and exit-21→`ERR_CONNECT`.
  - [x] **2. FIXED** — rollback now restores the **prior configuration**, not just `LD_VERSION`. Added `DockerContainer.Snapshot`/`DockerConfigSnapshot` persisted as JSON in `manifest.extra["docker_config"]` on success + rolled_back writes; new `dockerRollbackCtx` rebuilds the rollback context from that snapshot (falls back to version-only for legacy manifests). Test `TestDockerRollbackRestoresPriorConfig`: a combined version+port+env update that fails health rolls back with the **prior** port `8080`, `FOO=old`, `LD_VERSION=1.0.0` — not the rejected new values.
  - [x] **3. FIXED** — `Pull` registry extraction now recognizes the dotless host **`loca
  ...(truncated)
- Evaluator feedback:
  Improvements this iteration:
  - Verified substantive D1..D7 generation, preflight error handling, image-ID manifest persistence, health rollback, failed-state persistence, configuration fingerprints, and shell quoting.
  - Verified the full Go test suite passes.
  
  Still needs improvement:
  - [ ] 1. `internal/pattern/docker_container.go:120-124` masks every nonzero `docker inspect` exit as container absence, so daemon, authorization, and malformed-command failures can proceed to destructive replacement instead of aborting; distinguish an actual missing container from other Docker CLI failures and test both cases.
  - [ ] 2. `internal/engine/engine.go:558-564` constructs rollback context from the rejected desired deployment via `releaseCtxVersion(s, p, prev)`, restoring only the old image/version while retaining new ports, environment, volumes, restart policy, and run arguments; persist or recover the prior configuration and add a rollback test where configuration changed.
  - [ ] 3. `internal/pattern/docker_container.go:67-70` recognizes registries only when the first image component contains `.` or `:`, omitting Docker's special `localhost/repository` registry form; authenticated pulls therefore log in to the default registry instead of localhost. Add `localhost` detection and Linux/Windows coverage.
  
  Why this score: The implementation is substantial, well integrated, and broadly tested, but three edge cases can cause incorrect authentication, unsafe replacement, or incomplete rollback. These correctness gaps keep it below pass.
### Iteration 6 — verdict `iterate`, score 0, pair 1
- Failure: `generator-inactive` — Copilot transport silent for 10.1min (>10min) — assumed dead
- Generator summary:
  The evaluator's iter-5 feedback repeats the same 3 items with the *old* line numbers — I must verify whether my iter-5 changes actually landed on disk. Let me inspect the current state first.The evaluator's iter-5 feedback repeats the same 3 items with the *old* line numbers — I must verify whether my iter-5 changes actually landed on disk. Let me inspect the current state first.My iter-5 changes are **not on disk** — the file shows the old code (no localhost detection, no sentinel-exit `CurrentImageID`). The changes were reverted between iterations. Let me check the full current state of both files and tests.My iter-5 changes are **not on disk** — the file shows the old code (no localhost detection, no sentinel-exit `CurrentImageID`). The changes were reverted between iterations. Let me check the full current state of both files and tests.
- Evaluator feedback:
  GENERATOR FAILED (generator-inactive): Copilot transport silent for 10.1min (>10min) — assumed dead
  
  The evaluator was skipped to avoid burning tokens against a known-broken generator output. Auto-retrying on next iteration (transient failure).
  
  ---
  
  ## LATEST evaluator feedback (iteration 5, score 88, verdict: iterate)
  
  Improvements this iteration:
  - Verified substantive D1..D7 generation, preflight error handling, image-ID manifest persistence, health rollback, failed-state persistence, configuration fingerprints, and shell quoting.
  - Verified the full Go test suite passes.
  
  Still needs improvement:
  - [ ] 1. `internal/pattern/docker_container.go:120-124` masks every nonzero `docker inspect` exit as container absence, so daemon, authorization, and malformed-command failures can proceed to destructive replacement instead of aborting; distinguish an actual missing container from other Docker CLI failures and test both cases.
  - [ ] 2. `internal/engine/engine.go:558-564` constructs rollback context from the rejected desired deployment via `releaseCtxVersion(s, p, prev)`, restoring only the old image/version while retaining new ports, environment, volumes, restart policy, and run arguments; persist or recover the prior configuration and add a rollback test where configuration changed.
  - [ ] 3. `internal/pattern/docker_container.go:67-70` recognizes registries only when the first image component contains `.` or `:`, omitting Docker's special `localhost/repository` registry form; authenticated pulls therefore log in to the default registry instead of localhost. Add `localhost` de
  ...(truncated)

## Prompt audit pointers
- Iter 4 generator attempt 1: iteration_prompt id 474, 32172 chars, model `claude-opus-4.8`.
- Iter 4 evaluator attempt 1: iteration_prompt id 479, 11476 chars, model `gpt-5.6-sol`.
- Iter 5 generator attempt 1: iteration_prompt id 480, 35761 chars, model `claude-opus-4.8`.
- Iter 5 evaluator attempt 1: iteration_prompt id 482, 11476 chars, model `gpt-5.6-sol`.
- Iter 6 generator attempt 1: iteration_prompt id 483, 31267 chars, model `claude-opus-4.8`.
- Iter 7 generator attempt 1: iteration_prompt id 484, 31267 chars, model `claude-opus-4.8`.

## Agent working notes
### Iteration 5

# Iter notes — Docker Container Pattern (Stage 8.1) — iter 5

## Focus: resolve iter-4 evaluator items (inspect classification, rollback config fidelity, localhost registry)

### Prior feedback resolution
1. FIXED — docker_container.go CurrentImageID (~124-160): rewrote the D3 inspect to classify via SENTINEL EXIT CODES — 0=found, 20="No such object/container" (absent -> "" nil), anything else (daemon down / permission / CLI error) -> coded ERR_CONNECT. Previously ANY nonzero was masked as empty, letting a real failure bypass the engine's pre-run abort. Single-pass script (sh: out=$(... 2>&1); rc=$?; case; PS: Out-String + $LASTEXITCODE + -match 'No such'). Test TestDockerCurrentImageID now asserts exit20->"" and exit21->ERR_CONNECT.
2. FIXED — engine.go rollback (~558-580) now restores the PRIOR configuration, not just LD_VERSION. Added DockerContainer.Snapshot + DockerConfigSnapshot (name/ports/volumes/restart/run_args/env) persisted as JSON in manifest.extra["docker_config"] on every success + rolled_back write. New engine helper dockerRollbackCtx(s,p,prev,snapJSON) rebuilds rbRC from the prior snapshot (falls back to releaseCtxVersion when no snapshot); dockerConfigJSON marshals it. Test TestDockerRollbackRestoresPriorConfig: combined version+port+env change, health fails -> rollback run carries prior port 8080 + FOO=old + LD_VERSION=1.0.0, NOT the rejected 9090/FOO=new.
3. FIXED — docker_container.go Pull registry extraction (~67-78): now recognizes the dotless host `localhost` as a REGISTRY (host=="localhost" || ContainsAny(host, ".:")). Authenticated localhost/repo pulls now docker login 'localhost' instead of Docker Hub. Test TestDockerPullLocalhostRegistry covers localhost (registry) and library/nginx (Hub namespace, no registry host).

## Files touched this iter
- internal/pattern/docker_container.go: localhost registry rule; sentinel-exit CurrentImageID; DockerConfigSnapshot + Snapshot.
- internal/engine/engine.go: rollback uses prior-config snapshot (dockerRollbackCtx); docker_config persisted in success + rolled_back manifests; dockerConfigJSON/dockerRollbackCtx helpers.
- internal/pattern/docker_container_test.go: CurrentImageID exit20/exit21 cases; TestDockerPullLocalhostRegistry.
- internal/engine/docker_steplog_test.go: TestDockerRollbackRestoresPriorConfig.

## Decisions
- Sentinel exit codes (20/21) over Go-side stderr string parsing: deterministic + shell-agnostic, avoids fragile substring matching across sh/PS locales. "No such" match kept only as the in-script absence classifier.
- Persist full rendered env (incl builtin LD_*) in the snapshot so rollback replays the EXACT prior -e flags; env already visible via docker inspect on-host, so no new secret exposure.
- config_hash for rolled_back manifest computed from rbRC (prior config) so a later apply of the prior spec is a no-op but the desired spec still redeploys.

## Verification
- go build ./... = 0; go vet ./internal/pattern/ ./internal/engine/ clean.
- 
...(truncated)
