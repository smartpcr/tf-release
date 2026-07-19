# Iter notes — Docker Container Pattern (Stage 8.1) — iter 7

## ROOT CAUSE of the stall (why iter 5/6 repeated the same 3 items)
The iter-5 edits were **never persisted to git** — the worktree reset to
iter-4 state (`e511a5c`), so `docker_container.go`/`engine.go` on disk still
had the OLD masking/rollback/registry code and the evaluator re-flagged the
identical 3 items. This iter I RE-DID all three fixes on disk, verified each
is present via `Select-String`, and left them uncommitted for Forge to stage.

### Prior feedback resolution
1. FIXED — `docker_container.go` `CurrentImageID` (~120-158): D3 inspect now
   classifies via SENTINEL EXIT CODES — 0=found (return trimmed id), 20=absent
   ("No such object/container" -> "" nil), 21/other (daemon down, permission,
   CLI error) -> coded `ERR_CONNECT` so the engine aborts BEFORE D4 `rm -f`.
   Previously ANY nonzero was masked as empty, letting a real failure bypass
   the pre-run abort and destructively replace a live container. Single-pass
   script (sh: `out=$(... 2>&1); rc=$?; ... case *"No such"* -> exit 20; exit 21`;
   PS: `Out-String` + `$LASTEXITCODE` + `-match 'No such'`). Test
   `TestDockerCurrentImageID` asserts exit20->"" and exit21->ERR_CONNECT.
2. FIXED — `engine.go` rollback (~560-573): now restores the PRIOR
   configuration, not just LD_VERSION. `DockerConfigSnapshot`+`Snapshot`
   (ports/volumes/restart/run_args/env/name) persisted as JSON in
   `manifest.extra["docker_config"]` on every success + rolled_back write.
   New `dockerRollbackCtx(s,p,prev,snapJSON)` rebuilds `rbRC` from the prior
   snapshot (falls back to `releaseCtxVersion` when no snapshot);
   `dockerConfigJSON` marshals it. Test `TestDockerRollbackRestoresPriorConfig`:
   combined version+port+env change, health fails -> rollback run carries prior
   `-p '8080:8080'` + `-e 'FOO=old'` + `LD_VERSION=1.0.0`, NOT the rejected
   9090/FOO=new; rolled_back manifest records 8080 not 9090.
3. FIXED — `docker_container.go` `Pull` registry extraction (~67-79): the
   dotless host `localhost` is now recognized as a REGISTRY
   (`host == "localhost" || strings.ContainsAny(host, ".:")`). Authenticated
   `localhost/repo` pulls now `docker login 'localhost'` instead of Docker Hub.
   Test `TestDockerPullLocalhostRegistry` covers localhost (registry, Linux +
   Windows) and `library/nginx` (Hub namespace, no registry host).

## Files touched this iter
- internal/pattern/docker_container.go: localhost registry rule; sentinel-exit
  CurrentImageID; DockerConfigSnapshot + Snapshot.
- internal/engine/engine.go: rollback uses prior-config snapshot
  (dockerRollbackCtx); docker_config persisted in success + rolled_back
  manifests; dockerConfigJSON/dockerRollbackCtx helpers.
- internal/pattern/docker_container_test.go: CurrentImageID exit20/exit21;
  TestDockerPullLocalhostRegistry.
- internal/engine/docker_steplog_test.go: TestDockerRollbackRestoresPriorConfig.

## Verification
- go build ./... = 0; go vet ./internal/pattern/ ./internal/engine/ = clean.
- go test ./internal/engine/ -run <8 docker tests> = PASS (incl new
  TestDockerRollbackRestoresPriorConfig).
- go test ./internal/pattern/ ./internal/engine/ = PASS (pattern 3.1s,
  engine 136s).
- Grep-confirmed all three fixes are on disk this time (localhost rule,
  exit 20/21 + ERR_CONNECT, DockerConfigSnapshot, dockerRollbackCtx/
  dockerConfigJSON/docker_config).

## What's left
- Nothing outstanding for Stage 8.1. Edits left uncommitted for Forge to stage.
