<overview>
Implementing Stage 8.1 (Docker Container Pattern) of a Terraform provider (`terraform-provider-labdeploy`, a Go project) in a Forge worktree. Work is driven by iterative evaluator feedback. **Critical discovery this session: the iter-5 changes I believed I had made were never committed** — the git worktree reset to iter-4 state, so the iter-6 evaluator repeated the same 3 items verbatim. I am now **redoing all iter-5 fixes on disk** (inspect-error classification, rollback config fidelity, localhost registry detection).
</overview>

<history>
1. **Prior context (iters 1-4, summarized)**: Production code was already implemented. Across iters added test proofs, then hardened real bugs: per-OS shell quoting via `d.q()`, image-id durability, rollback version rebuild, config fingerprint idempotency, pre-run/post-run inspect handling. Iter-4 committed at score 88 (git commit `e511a5c`).

2. **This session opened as "iteration 7"** with evaluator feedback (labeled iteration 5, score 88) repeating 3 items with OLD line numbers:
   - Item 1: `docker_container.go:120-124` masks every nonzero `docker inspect` as absence.
   - Item 2: `engine.go:558-564` rollback rebuilds from rejected desired spec (only LD_VERSION), losing prior ports/env/volumes/restart/run_args.
   - Item 3: `docker_container.go:67-70` doesn't recognize `localhost` registry.

3. **I inspected the files and discovered the iter-5 work was gone.**
   - Viewed `docker_container.go` — showed OLD code (no localhost, old `CurrentImageID` masking all nonzero as "").
   - Ran `git log` + `git status`: HEAD at iter-4 (`e511a5c` + a merge `21e34fb`); only `.forge/` untracked. Confirmed iter-5 edits were never committed/lost.

4. **Redid all three iter-5 fixes on disk (in progress):**
   - Applied localhost registry detection to `Pull`.
   - Rewrote `CurrentImageID` with sentinel exit codes (0/20/21).
   - Added `DockerConfigSnapshot` type + `Snapshot` method.
   - Wired engine rollback to use prior-config snapshot + `docker_config` persistence + 2 helpers.
   - Updated `TestDockerCurrentImageID` (exit 20/21 cases), added `TestDockerPullLocalhostRegistry`.
   - `go build ./...` = 0; pattern tests pass.
   - **Was about to add the engine test `TestDockerRollbackRestoresPriorConfig`** when compaction hit.
</history>

<work_done>
Files updated this session (redoing iter-5 fixes):
- `internal/pattern/docker_container.go`:
  - [x] `Pull` registry extraction (~67-79): now `host == "localhost" || strings.ContainsAny(host, ".:")` recognizes localhost as registry.
  - [x] `CurrentImageID` (~115-160): rewrote to classify via sentinel exit codes — 0=found (return trimmed stdout), 20=absent (return "" nil), else=`stepErr("ERR_CONNECT", ...)`. sh: `out=$(docker inspect ... 2>&1); rc=$?; if [ $rc -eq 0 ]...; case "$out" in *"No such"*) exit 20;; esac; ...exit 21`. PS: `Out-String` + `$LASTEXITCODE` + `-match 'No such'`.
  - [x] Added `DockerConfigSnapshot` struct (ContainerName/Ports/Volumes/Restart/RunArgs/Env with json tags) + `Snapshot(rc)` method.
- `internal/engine/engine.go`:
  - [x] Rollback (~558-573): `rbRC` now uses `dockerRollbackCtx(s, p, prev, m.Extra["docker_config"])` when `prev != "" && m != nil`, falling back to `releaseCtxVersion(s, p, prev)`.
  - [x] rolled_back manifest Extra (~596): added `"docker_config": dockerConfigJSON(dc, rbRC)`.
  - [x] success manifest Extra (~634): added `"docker_config": dockerConfigJSON(dc, rc)`.
  - [x] Added `dockerConfigJSON(dc, rc)` and `dockerRollbackCtx(s, p, version, snapJSON)` helpers after `releaseCtxVersion` (~775+).
- `internal/pattern/docker_container_test.go`:
  - [x] `TestDockerCurrentImageID`: absent now `{ExitCode: 20}`; added exit-21 → ERR_CONNECT assertion (uses `errors.As`, `*StepError`).
  - [x] Added `TestDockerPullLocalhostRegistry` (localhost on Linux+Windows, library/nginx Hub-namespace negative case).

Verification so far:
- [x] `go build ./...` = 0.
- [x] `go test ./internal/pattern/ -run TestDocker` = PASS.
- [ ] Engine test `TestDockerRollbackRestoresPriorConfig` NOT yet added.
- [ ] Full engine suite NOT yet re-run.
- [ ] `.forge/iter-notes.md` NOT yet overwritten for this iter.
</work_done>

<technical_details>
- **ROOT CAUSE of the stall**: iter-5 edits were never persisted to git. Forge commits after each iter; my iter-5 work was left uncommitted and the worktree reset to iter-4. **Before replying DONE this iter, the edits must be complete and left uncommitted in the worktree — do NOT run git mutations; Forge stages/commits.** The convergence detector will HARD-STOP (`stalled-no-convergence`) if the same items recur, so these fixes MUST actually land.
- **Go project**, cwd `C:\forge\release\.worktree\stage-8.1-docker-container-pattern`, Windows env, backslash paths. Build: `go build ./...` (exit 0 gate). Tests: `go test ./internal/pattern/ ./internal/engine/`. **Engine tests are SLOW (~130s)**; pattern ~3s. This is Go, NOT dotnet (prompt boilerplate says dotnet — ignore).
- **Sentinel exit codes (20/21)** chosen over Go-side stderr parsing: deterministic + shell-agnostic. `dockerNode` fake in engine tests intercepts `docker inspect ... {{.Image}}` and returns `ok(n.image)` (exit 0), so engine tests still work: `n.image=""` → exit 0 empty → CurrentImageID returns "". The new script only changes behavior for genuine nonzero exits.
- **Config snapshot approach (item 2)**: persist `DockerConfigSnapshot` as JSON in `manifest.extra["docker_config"]` on success + rolled_back writes. On rollback, `dockerRollbackCtx` unmarshals it and rebuilds a ReleaseCtx with a COPIED spec (`sc := *s; pc := s.Pattern; ...; sc.Pattern = pc`) so the prior ports/volumes/restart/run_args/env are restored, not the rejected desired ones. Env stored includes builtin LD_* (already visible via docker inspect on-host, no new secret exposure).
- **`releaseCtxVersion(s, p, version)`** (engine.go ~755): builds ReleaseCtx at a specific version (threads LD_VERSION). `encoding/json` already imported in engine.go.
- **Pattern helpers** (already exist): `stepErr(code, host, step, err)`, `truncOut(r)`, `d.q(t, s)` (per-OS single-quote), `imageRef(a)` (digest→`img@digest`, else `img:tag`).
- **Registry-vs-repository rule**: first path component is a registry host iff it contains `.`/`:` OR equals `localhost`; else it's a Hub namespace (e.g. `library/nginx`). `regArg` is `""` when registry empty (so `docker login -u ...` with no host), else `" " + d.q(t, registry)`.
- **Engine test fake `dockerNode`** (docker_steplog_test.go): has fields `image`, `runImage`, `rollbackImage`, `pullErr`, `runErr`, `runScripts`, and iter-4-added `inspectErr`/`inspectErrAfterRun`/`didRun`. `healthGate` gated on `n.image` (rollback run detected by ref==rollbackImage flips n.image to old id so recheck passes). Helpers: `dockerSpec(t, version)`, `dockerSpecPorts(t, version, ports...)`.
- **Manifest JSON keys**: `current_version`, `previous_version`, `extra` (map), `last_operation.result` (success|failed|rolled_back). Manifest path in tests: `C:\deploy\sample-svc\manifest.json` (const `dockerManifestPath`).
</technical_details>

<important_files>
- `internal/pattern/docker_container.go`
  - Core D1..D7 pattern. Edits DONE this iter: localhost registry (~67-79), sentinel-exit `CurrentImageID` (~115-160), `DockerConfigSnapshot`+`Snapshot` (new, after CurrentImageID). `q()` helper ~163+, `runArgs` ~133.
- `internal/engine/engine.go`
  - `deployDocker` at ~488-651. Edits DONE: rollback rbRC via `dockerRollbackCtx` (~558-573), `docker_config` in rolled_back (~596) + success (~634) manifests, new helpers `dockerConfigJSON`/`dockerRollbackCtx` (~775+ after `releaseCtxVersion`). Post-run inspect fail-closed (~620-628), pre-run inspect propagate (~521-527) already present from iter-4.
- `internal/pattern/docker_container_test.go`
  - Edits DONE: `TestDockerCurrentImageID` exit20/exit21 (~216-235), `TestDockerPullLocalhostRegistry` (new, after digest test ~130). `errors` imported.
- `internal/engine/docker_steplog_test.go`
  - Has iter-4 tests through `TestDockerSameVersionConfigChangeRedeploys` (ends ~342). **STILL NEEDS**: append `TestDockerRollbackRestoresPriorConfig`. `dockerSpec`/`dockerSpecPorts` helpers present.
- `.forge/iter-notes.md`
  - Must overwrite with THIS iter's reflection before finishing (untracked `.forge/`, excluded from git).
</important_files>

<next_steps>
Immediate next steps (finish redoing iter-5):
1. **Append `TestDockerRollbackRestoresPriorConfig`** to `internal/engine/docker_steplog_test.go` (after line ~342). Test: phase-1 deploy 1.0.0 with `Pattern.Ports=["8080:8080"]` + `Environment={FOO:old}`; phase-2 deploy 2.0.0 with `Ports=["9090:9090"]` + `{FOO:new}`, set `n.runImage="sha256:v2img"`, `n.rollbackImage="sha256:v1img"`, `n.runScripts=nil`, `healthGate=func() bool { return n.image=="sha256:v2img" }`. Assert ERR_HEALTH_CHECK; find rollbackRun (contains `sha256:v1img`) and newRun (contains `LD_VERSION=2.0.0`, NOT the id); rollbackRun must have `-p '8080:8080'` + `-e 'FOO=old'` + `LD_VERSION=1.0.0`, NOT 9090/FOO=new; manifest at 1.0.0 rolled_back with `8080:8080` not `9090:9090`. **GOTCHA from prior attempt**: detect newRun by `LD_VERSION=2.0.0` (the run script references the image REF not the resulting id), not by `sha256:v2img`.
2. `go build ./...` (exit 0), `go vet ./internal/pattern/ ./internal/engine/`.
3. Run docker engine tests: `go test ./internal/engine/ -run "TestDockerRollbackRestoresPriorConfig|TestDockerPreRunInspectErrorAbortsBeforeRemoval|TestDockerPostRunInspectFailurePersistsNewVersionFailedState|TestDockerSameVersionConfigChangeRedeploys|TestDockerHealthFailureRollsBackPrevVersion|TestDockerDeploySuccessRecordsImageID|TestDockerDeployEmptyImageIDFailsClosed|TestDockerRollbackFailedNoManifest" -v`.
4. Run full suites: `go test ./internal/pattern/ ./internal/engine/` (green), optionally provider/spec/layout/transport.
5. Overwrite `.forge/iter-notes.md` with iter reflection + `### Prior feedback resolution` block marking all 3 items FIXED (this time verify they're actually on disk).
6. Write final Iteration Summary with `### Prior feedback resolution` (items 1/2/3 ADDRESSED with file refs + test names). Do NOT emit `DONE`/`[Iteration N/20]` markers. Do NOT run git.
</next_steps>