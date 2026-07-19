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
- go test ./internal/pattern/ ./internal/engine/ = PASS (8 docker engine tests incl new rollback-config test; all docker pattern tests).
- go test ./internal/provider/ ./internal/spec/ ./internal/layout/ ./internal/transport/ = PASS.

## What's left
- Nothing outstanding for Stage 8.1.
