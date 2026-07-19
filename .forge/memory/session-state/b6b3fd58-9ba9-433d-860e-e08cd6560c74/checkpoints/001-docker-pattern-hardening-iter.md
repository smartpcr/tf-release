<overview>
Implementing Stage 8.1 (Docker Container Pattern) of a Terraform provider (`terraform-provider-labdeploy`, Go project) in a Forge worktree. The production code (`docker_container.go` D1..D7, engine rollback/manifest wiring) was already present; work has been driven by iterative evaluator feedback across iters 2-4, each round hardening real production bugs (arg quoting, image-id durability, rollback version, idempotency) and adding proving tests. This is iteration 4, addressing 4 new evaluator items about transaction safety, idempotency, and complete shell quoting.
</overview>

<history>
1. **Iter 1-2**: Discovered production code already implemented. Added the two required Stage 8.1 test-scenario proofs: `internal/pattern/docker_container_test.go` (golden tests for D1..D7 + preflight-fail) and 6 golden fixtures under `internal/pattern/testdata/`. Checked the 2 Stage 8.1 boxes in `implementation-plan.md`. Score 73.

2. **Iter 3**: Evaluator gave 4 items (all fixed): (1) engine ignored post-run inspect error → added fail-closed on empty image_id; (2) rollback test only called RunNew directly → added engine-level health-failure rollback test; (3) unsafe `-e "K=V"` double-quote rendering → replaced `shellKV` with per-OS `d.q()` (sh `shq`, PS `psq`) + metacharacter tests; (4) rollback advertised wrong version → engine rebuilds `rbRC = releaseCtxVersion(s, p, prev)`. Regenerated goldens, added engine tests via enhanced `dockerNode` fake. Score 82.

3. **Iter 4 (current)**: Evaluator gave 4 new items:
   - Item 1: `engine.go:512-514` ignores transport error from pre-run D3 `CurrentImageID` → can `rm -f` live container without recording rollback image. Propagate error before D4.
   - Item 2: `engine.go:604-608` returns immediately on post-run inspect failure, leaving new container running while TF retains old state. Roll back OR persist failed state + test machine state.
   - Item 3: `engine.go:493-496` version+running = unconditional no-op → same-version config changes (tag/digest/ports/env/volumes/restart/run_args) silently ignored. Add config fingerprint to idempotency.
   - Item 4: Incomplete quoting in `docker_container.go` (pull/login lines 71-92, inspect/status/names lines 109-113 & 194-237, restart line 129). Apply `q` consistently + metachar tests.
   - I read both files fully, then began editing `docker_container.go`: added `crypto/sha256`+`encoding/hex` imports, rewrote `Pull` with quoted registry/username/ref (conditional `regArg` when registry non-empty), quoted container name in `CurrentImageID`, quoted restart policy in `runArgs`.
</history>

<work_done>
Files updated this iter (iter 4), IN PROGRESS:
- `internal/pattern/docker_container.go`:
  - [x] Added `crypto/sha256` + `encoding/hex` imports.
  - [x] Rewrote `Pull`: registry quoted via `regArg = " " + d.q(t, registry)` only when non-empty; username via `d.q(t, ...)`; ref via `qref := d.q(t, ref)`. Windows and linux `docker pull %s` now use `qref`.
  - [x] `CurrentImageID`: container name now `qn := d.q(t, rc.Spec.Pattern.ContainerName)` in both OS branches (linux changed from `'%s'` to `%s` with qn).
  - [x] `runArgs`: restart policy now `d.q(t, restart)` (line ~129).
  - [ ] STILL TODO: `Status`, `Stop`, `Uninstall` container names still use raw `name`/`'%s'` (lines ~194-237) — NOT yet quoted.
  - [ ] STILL TODO: Add `ConfigFingerprint(rc ReleaseCtx) string` method (sha256 of imageRef + name + restart + sorted ports/volumes + run_args order + sorted env).

Work completed in prior iters (already landed):
- [x] `docker_container_test.go`: 7 docker tests (golden linux/windows pull+run, digest, rollback, CurrentImageID/D3, metachar-safe, preflight-fail).
- [x] `docker_steplog_test.go`: `dockerNode` fake with `runImage`/`rollbackImage`/`runScripts`; tests for success-records-imageID, empty-imageID-fails-closed, health-failure-rolls-back-prev-version.
- [x] engine.go iter-3 fixes (rollback rbRC, post-run fail-closed).

NOT yet done this iter (pending):
- [ ] Engine item 1: propagate pre-run inspect error before D4.
- [ ] Engine item 2: persist failed manifest at NEW version on post-run inspect failure.
- [ ] Engine item 3: idempotency check compares `m.Extra["config_hash"] == dc.ConfigFingerprint(rc)`; success + rolled_back manifests must store `config_hash`.
- [ ] New tests for items 1, 2, 3.
- [ ] Regenerate goldens (registry now quoted, windows ref quoted, `--restart 'always'`) and update test assertions.
- [ ] Build + full test run.
- [ ] Update `.forge/iter-notes.md`.
</work_done>

<technical_details>
- **Go project**, cwd `C:\forge\release\.worktree\stage-8.1-docker-container-pattern`. Windows env, use backslash paths. Build: `go build ./...`; tests: `go test ./internal/pattern/ ./internal/engine/`. Engine tests are SLOW (~120s). Use gate command `go build ./...` (exit 0). Do NOT run git mutations. This is Go, NOT dotnet (prompt boilerplate mentions dotnet — ignore).
- **Quoting helpers** (already exist): `shq(s)` = `'…'` with `'\''` escaping (console_node_dotnet.go:40); `psq(s)` = `'…'` with `''` escaping (pattern.go:106). `d.q(t, s)` dispatches shq/psq by `t.OS()`.
- **`d.q()` for empty registry gotcha**: `docker login` with empty registry must NOT emit `''`. Solution: `regArg` is `""` when registry empty, else `" " + d.q(t, registry)`; format string is `docker login%s -u %s`.
- **imageRef** (pattern, unexported): digest→`image@digest`, else `image:tag` (tag falls back to version). Put `ConfigFingerprint` in pattern package to access imageRef.
- **Idempotency plan (item 3)**: add `dc.ConfigFingerprint(rc)` (sha256 hex of ref/name/restart/sorted ports/sorted volumes/ordered run_args/sorted env). Store in success manifest `Extra["config_hash"]` and rolled_back manifest `Extra["config_hash"]` (using rbRC for prev config). Failed manifests deliberately OMIT config_hash so they never no-op (forces reconverge; Read also appends `!failed` marker).
- **Item 2 plan**: on post-run inspect failure, call `e.dockerFinalizeFailed(ctx, sl, t, s, p, s.Artifact.Version, strings.TrimSpace(newImage), started)` to persist failed manifest at NEW version (so Read shows drift + reconverges), then return coded ERR_CONNECT/FINALIZE error. `dockerFinalizeFailed` returns nil if version=="" but s.Artifact.Version is non-empty.
- **Item 1 plan**: `cur, cerr := dc.CurrentImageID(...); if cerr != nil { return nil, cerr }; if cur != "" { oldImage = cur }`. CurrentImageID returns "" with NO error when container absent (script does `|| echo ''` exit 0); err!=nil only on genuine transport failure.
- **dockerNode fake** (docker_steplog_test.go): intercepts docker pull/inspect/run/stop; delegates other scripts (health via `Invoke-WebRequest`, manifest I/O) to embedded `fakeHost`. For NEW tests need fields like `inspectErr bool` (fail all `{{.Image}}` inspects) and `inspectErrAfterRun bool` + `didRun bool` (fail inspect only after a docker run) to distinguish item-1 (pre-run) vs item-2 (post-run) inspect failures.
- **Health gating quirk**: `RunHealthCheck` retries internally, so a call-counter healthGate lets retries pass. Gate on `n.image` instead; rollback run detected by `strings.Contains(s, n.rollbackImage)` flips `n.image` to old id so recheck passes. Mirrors cluster test pattern (`healthGate = func() bool { return strings.Contains(current, "2.0.0") }`).
- **Golden regeneration**: `-update` flag is `*updateConsole` (declared in console_app_test.go, package-scoped — reuse, don't redeclare). `checkGolden` writes-then-compares so goldens update even on failing assertion. Run `go test ./internal/pattern/ -run TestDocker -update`.
- **Manifest JSON** (manifest.go:38): `current_version`, `previous_version`, `extra` (map, key `image_id`), `last_operation.result` (success|failed|rolled_back). Manifest path in tests: `C:\deploy\sample-svc\manifest.json` (const `dockerManifestPath`).
- **`releaseCtxVersion(s, p, version)`** (engine.go:717) builds ReleaseCtx at a specific version (threads LD_VERSION); `releaseCtx(s,p)` uses s.Artifact.Version.
- **Golden assertion updates needed**: pull goldens — registry now `'registry.example.com'` (was unquoted), windows ref now `'registry.example.com/app:2.0.0'` (was unquoted); run goldens — `--restart 'always'` (was `--restart always`). Update both goldens (via -update) and the string assertions in docker_container_test.go (e.g. `docker login registry.example.com -u 'svc'` → `docker login 'registry.example.com' -u 'svc'`).
</technical_details>

<important_files>
- `internal/pattern/docker_container.go`
  - Core pattern implementation (D1..D7). Actively editing for item 3 (ConfigFingerprint) + item 4 (quoting).
  - Done: imports, Pull (quoted), CurrentImageID (quoted qn), runArgs restart (quoted).
  - TODO: quote names in `Status` (~210-227), `Stop` (~194-204), `Uninstall` (~229-239); add `ConfigFingerprint` method. `q()` helper at ~157.
- `internal/engine/engine.go`
  - `deployDocker` at lines 488-630. TODO all 3 engine items: idempotency (493-496), pre-run inspect (512-514), post-run inspect fail (604-608). Success manifest Extra at 613, rolled_back Extra at 583, `dockerFinalizeFailed` at 635.
- `internal/pattern/docker_container_test.go`
  - 7 docker tests. Will need updated pull/registry + `--restart 'always'` assertions after regenerating goldens; add item-4 metachar tests for pull/inspect/status/name paths.
- `internal/engine/docker_steplog_test.go`
  - `dockerNode` fake + engine docker tests. Need new fields (`inspectErr`, `inspectErrAfterRun`, `didRun`) and 3 new tests (pre-run inspect error, post-run inspect failed-state, idempotency config-change).
- `internal/pattern/testdata/docker_*.golden`
  - 6 committed fixtures; pull_linux/pull_windows/run_linux/run_windows/run_rollback need regeneration after quoting changes.
- `.forge/iter-notes.md`
  - Must overwrite before finishing with iter-4 reflection.
- `docs/stories/release-RELEASE-PROVIDER/implementation-plan.md`
  - Stage 8.1 at line 411; 2 test-scenario boxes already checked (lines 422-423).
</important_files>

<next_steps>
Immediate next steps (finish item 4 in docker_container.go):
1. Quote container name in `Status`, `Stop`, `Uninstall` via `qn := d.q(t, name)` in both OS branches.
2. Add `ConfigFingerprint(rc ReleaseCtx) string` method (sha256 hex over imageRef, name, restart, sorted ports, sorted volumes, ordered run_args, sorted env).

Then engine.go (items 1-3):
3. Item 1: pre-run inspect (512-514) — propagate `cerr` before Start/D4.
4. Item 2: post-run inspect fail (604-608) — call `dockerFinalizeFailed(...s.Artifact.Version...)` to persist failed manifest at new version, then return coded error.
5. Item 3: idempotency (493-496) — add `&& m.Extra["config_hash"] == dc.ConfigFingerprint(rc)`; add `config_hash` to success manifest Extra (613) and rolled_back Extra (583, using rbRC).

Then tests + verify:
6. Regenerate goldens: `go test ./internal/pattern/ -run TestDocker -update`; inspect; update string assertions in docker_container_test.go.
7. Add metachar tests (item 4) for pull/inspect/status/name; add engine tests: pre-run inspect error (item 1, assert no docker run executed), post-run inspect failed-manifest-at-new-version (item 2), same-version config-change-redeploys + same-config-noop (item 3). Add dockerNode fields for inspect failure control.
8. `go build ./...` (exit 0), `go vet`, `go test ./internal/pattern/ ./internal/engine/` (green).
9. Overwrite `.forge/iter-notes.md` with iter-4 reflection; write `### Prior feedback resolution` block in final summary mirroring all 4 items marked FIXED.
</next_steps>