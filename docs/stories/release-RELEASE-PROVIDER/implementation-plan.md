---
title: "release provider"
storyId: "release:RELEASE-PROVIDER"
---

# Phase 1: Project Scaffold and Spec Engine

## Dependencies
- _none -- start phase_

## Stage 1.1: Repository Bootstrap and Toolchain

> Baseline note: this scaffold already exists in the worktree (module `github.com/smartpcr/terraform-provider-labdeploy`, `main.go`, `Makefile`, and the `internal/{provider,spec,transport,artifact,pattern,engine,logs,layout}` package tree with initial implementations). The boxes below are checked to reflect that landed baseline; later phases harden, test, and complete each module.

### Implementation Steps
- [x] Go module `github.com/smartpcr/terraform-provider-labdeploy` (`go.mod`, Go 1.22) with `main.go` wiring `providerserver.Serve` at plugin protocol v6 for address `registry.local/smartpcr/labdeploy`.
- [x] Package tree under `internal/{provider,spec,transport,artifact,pattern,engine,logs,layout}` created with initial sources so later stages land in isolation.
- [x] Dependencies declared in `go.mod`: terraform-plugin-framework v1.11.0, terraform-plugin-log v0.9.0, masterzen/winrm, golang.org/x/crypto, pkg/sftp, gopkg.in/yaml.v3.
- [x] `Makefile` with `build`/`test`/`lint` targets present; keep it aligned with DESIGN sec 20 (`golangci-lint run` clean).
- [ ] Run `go mod tidy` and commit the resulting `go.sum` (absent in the worktree today) so dependency checksums are pinned and builds are reproducible.
- [ ] Enforce the no-cgo build invariant: set `CGO_ENABLED=0` in the Makefile `build` target (currently line 11 runs plain `go build`) and in the goreleaser config (Stage 8.3).
- [ ] Add a CI workflow that runs `make build test lint` on the gate host (not yet in the worktree).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Module builds -- Given the existing repo, When `make build` runs on the gate host, Then binary `terraform-provider-labdeploy_v0.1.0` is produced with `CGO_ENABLED=0` [proof: service:go-toolchain; deps: go-toolchain = the gate host's own Go compiler + make (build tool + emitted binary, not pure logic)]
- [ ] Scenario: Lint clean -- Given the repo, When `golangci-lint run` runs on the gate host, Then it exits 0 with no findings [proof: service:go-toolchain; deps: go-toolchain = the gate host's golangci-lint binary (external tool invocation)]
- [ ] Scenario: Provider advertises protocol v6 -- Given `main.go`, When the provider is served under the terraform-plugin-testing harness, Then it advertises plugin protocol v6 and address `registry.local/smartpcr/labdeploy` [proof: service:tf-plugin-server; deps: tf-plugin-server = the in-process terraform-plugin-testing gRPC provider server (bundled, no external target)]

## Stage 1.2: Spec Types and YAML JSON Parsing

### Implementation Steps
- [x] Define Go structs in `internal/spec/types.go` for the `Deployment` envelope, `target`, `artifact`, the `pattern` discriminated union, `environment`/`files`/`health_check`/`strategy`/`logs`, and the `TestRun` kind (DESIGN sec 6, sec 7).
- [x] Implement `internal/spec/parse.go`: accept YAML or JSON, convert YAML to JSON, unmarshal into typed structs, and detect `apiVersion`/`kind`.
- [x] Decode the `pattern.type` discriminated union into the concrete pattern struct (console_app, windows_service, node_web_app, dotnet_api, cluster_generic_service, docker_container).
- [x] Add `OSKind` and `TransportKind` enums plus provider-default merge helpers used by validation.

### Dependencies
- phase-project-scaffold-and-spec-engine/stage-repository-bootstrap-and-toolchain

### Test Scenarios
- [ ] Scenario: YAML equals JSON -- Given the same Deployment spec authored in YAML and in JSON, When both are parsed, Then they produce identical in-memory structs [proof: in-process; deps: none]
- [ ] Scenario: Pattern union decode -- Given a spec with `pattern.type: windows_service`, When parsed, Then the windows_service concrete struct is populated and other union members are nil [proof: in-process; deps: none]

## Stage 1.3: Spec Validation and Variable Substitution

### Implementation Steps
- [x] Implement `internal/spec/validate.go` covering every DESIGN sec 6/sec 7 rule: envelope regex, target merge + required fields, artifact regex/checksum rules, pattern x os matrix (sec 14), and artifact x pattern matrix.
- [x] Implement `${var:NAME}` substitution sourced only from the resource `variables` map, with `$${var:...}` escape and unresolved-token error `unresolved variable NAME at <json-path>`.
- [x] Implement env-var NAME hygiene: specs carry env-var names only; a referenced-but-unset env var yields `ERR_SPEC_INVALID env var <NAME> ... is not set` before any dial.
- [x] Emit every `ERR_SPEC_INVALID` with the offending JSON path, matching the VAL-01..VAL-09 message contracts.

### Dependencies
- phase-project-scaffold-and-spec-engine/stage-spec-types-and-yaml-json-parsing

### Test Scenarios
- [ ] Scenario: Validation matrix -- Given the table of VAL-01..VAL-09 invalid specs, When each is validated, Then each returns `ERR_SPEC_INVALID` with the expected JSON path and message fragment [proof: in-process; deps: none]
- [ ] Scenario: Variable substitution and escape -- Given a spec with `${var:X}` and `$${var:Y}`, When substituted with `variables={X:...}`, Then `X` is replaced, literal `${var:Y}` is preserved, and an unknown name yields `ERR_SPEC_INVALID` [proof: in-process; deps: none]

## Stage 1.4: Canonical JSON and Spec Hashing

### Implementation Steps
- [x] Implement a canonical JSON emitter (sorted keys, no insignificant whitespace) over the substituted spec.
- [x] Compute `spec_hash = sha256(canonical JSON)` after `${var}` substitution and `version_override` application.
- [x] Apply `version_override` to `artifact.version` before hashing so the pipeline build number drives the release dir name.
- [x] Provide the immutable-field extraction helper (pattern.type, service_name, role_name, install_root, metadata.name, hosts-set, os) consumed by the RequiresReplace plan modifier in Stage 5.2.

### Dependencies
- phase-project-scaffold-and-spec-engine/stage-spec-validation-and-variable-substitution

### Test Scenarios
- [ ] Scenario: Hash stable across key order -- Given a committed canonical-JSON fixture rendered with keys in different orders, When hashed, Then the `spec_hash` is byte-identical (T2 preservation proof) [proof: golden; deps: none -- committed canonical-JSON fixture under internal/spec/testdata]
- [ ] Scenario: Override changes hash -- Given a base spec, When `version_override` is applied, Then `artifact.version` reflects the override and `spec_hash` differs from the base [proof: in-process; deps: none]

# Phase 2: Transport and Artifact Acquisition

## Dependencies
- phase-project-scaffold-and-spec-engine

## Stage 2.1: Transport Interface and Local Execution

### Implementation Steps
- [x] Define the `Transport` interface plus `Cmd`/`Result`/`Shell` types in `internal/transport/transport.go` (DESIGN sec 8.1).
- [x] Implement `local.go` with `os/exec` + file copy; `Connect` verifies `runtime.GOOS` matches `target.os`.
- [x] Implement the connect retry loop (`connect_retries`, fixed 5s backoff) with `ERR_CONNECT` vs `ERR_AUTH` mapping seam.
- [x] Add the `NewTransport` factory dispatching on transport kind (local/ssh/winrm), reused by the engine seam for fake-transport tests.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Local round-trip -- Given `transport: local` on the gate OS, When `Exec` + `Upload` + `Download` run, Then a file survives the round-trip and `Exec` returns app exit codes in `Result` (transport error only on transport failure) [proof: service:local-shell; deps: local-shell = the gate host's own powershell/sh via transport local, gated on runtime.GOOS per architecture D-gate-1]
- [ ] Scenario: OS mismatch rejected -- Given `transport: local` with `target.os` not matching `runtime.GOOS`, When `Connect` runs, Then it errors before executing any command [proof: in-process; deps: none]

## Stage 2.2: PowerShell Encoding and WinRM Transport

### Implementation Steps
- [x] Implement the `EncodedCommand` builder: `base64(UTF-16LE(script))` invoked as `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand`.
- [x] Implement env injection by prepending `$env:K='V';` lines with single-quote escaping; secret values never appear on the command line.
- [x] Implement `winrm.go` (masterzen/winrm) `Exec` with https/insecure toggles; a 401 maps to `ERR_AUTH` and is not retried.
- [x] Implement WinRM chunked base64 upload (48000-byte raw chunks, `CreateNew` first then `FileStream` append) and download.

### Dependencies
- phase-transport-and-artifact-acquisition/stage-transport-interface-and-local-execution

### Test Scenarios
- [ ] Scenario: EncodedCommand exact bytes -- Given a known script plus env with `O'Brien`, When encoded, Then the bytes match the committed golden fixture and the single-quote escaping is correct (T3) [proof: golden; deps: none -- committed expected-bytes fixtures under internal/transport/testdata]
- [ ] Scenario: Chunk math boundaries -- Given upload payloads of 0/1/48000/48001 bytes, When chunked, Then the chunk counts and offsets match the golden expectation (T3) [proof: golden; deps: none -- committed fixtures under internal/transport/testdata]

## Stage 2.3: SSH Transport and SFTP Transfer

### Implementation Steps
- [x] Implement `ssh.go` (golang.org/x/crypto/ssh) dial with `host_key` pinning ("" accepts any + WARN diag) and password/private-key env auth.
- [x] Implement SFTP upload/download via pkg/sftp.
- [x] Map ssh auth failure to `ERR_AUTH` (no retry), dial/timeout to `ERR_CONNECT`, and pinned-key mismatch to `ERR_CONNECT` with `host key mismatch`.
- [x] Wire the Linux `sh -c` env-prepend (`K='V' `) for Exec.

### Dependencies
- phase-transport-and-artifact-acquisition/stage-transport-interface-and-local-execution

### Test Scenarios
- [ ] Scenario: SSH dial and SFTP round-trip -- Given a real SSH endpoint, When `Connect`/`Exec`/`Upload`/`Download` run over the ssh transport, Then data round-trips and auth rejection is not retried; a local shell cannot exercise ssh/sftp, so this belongs to lab acceptance [proof: lab; deps: L1 Linux VM with sshd, which architecture assigns to acceptance area L2 (live SSH is never local-shell)]
- [ ] Scenario: Host key mismatch mapping -- Given a pinned wrong `ssh.host_key`, When `Connect` runs against a stub key, Then the error is `ERR_CONNECT` with detail `host key mismatch` [proof: in-process; deps: none]

## Stage 2.4: Artifact Fetch and Target Pull

### Implementation Steps
- [x] Implement `internal/artifact/fetch.go` for http/file/nuget: stream to a runner temp file, compute sha256 while streaming, compare to `artifact.checksum` (mismatch = `ERR_CHECKSUM_MISMATCH`, 4xx/5xx = `ERR_ARTIFACT_FETCH` with status + first 256B body).
- [x] Implement NuGet v3 flat-container URL construction and bearer/basic `Authorization` from `auth.token_env`.
- [x] Implement `TargetPullScript` generation (win `Invoke-WebRequest` + `Get-FileHash`, exit 41; linux `curl` + `sha256sum`) with `LD_AUTH_VALUE` injected via `Cmd.Env`.
- [x] Implement docker-ref passthrough (no runner fetch; pull deferred to the docker pattern).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Fetch sha verify and 404 -- Given an httptest server serving a zip with a correct sha and a 404 route, When `Fetch` runs, Then the good sha passes and the 404 yields `ERR_ARTIFACT_FETCH` including `404` (T5) [proof: service:httptest; deps: httptest = Go net/http/httptest in-process ephemeral server, bundled stdlib, no docker]
- [ ] Scenario: NuGet URL and target-pull script -- Given a `nuget_feed` source, When the download URL is built and `TargetPullScript` generated, Then the URL matches the flat-container form and the script matches the committed golden [proof: golden; deps: none -- committed fixtures under internal/artifact/testdata]

# Phase 3: Engine Core and Manifest State

## Dependencies
- phase-transport-and-artifact-acquisition

## Stage 3.1: Path Model and Target Layout

### Implementation Steps
- [x] Implement `internal/layout` `Paths` deriving `releases/`, `current`, `shared/`, `staging/`, `manifest.json`, `.lock` with the correct separator per os (DESIGN sec 9.1).
- [x] Generate directory-creation scripts (`New-Item -ItemType Directory -Force` / `mkdir -p`).
- [x] Provide the `LD_*` env injection map (`LD_APP`, `LD_VERSION`, `LD_RELEASE_DIR`, `LD_SHARED_DIR`, and `PORT` for node) merged under spec `environment` with spec winning.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Path derivation both OSes -- Given `install_root` and app name for windows and linux, When paths are computed, Then the separators and layout match DESIGN sec 9.1 exactly [proof: in-process; deps: none]
- [ ] Scenario: Layout script golden -- Given a ReleaseCtx, When the dir-creation script is generated for win and linux, Then it matches the committed golden [proof: golden; deps: none -- committed fixtures under internal/layout/testdata]

## Stage 3.2: Locking and Manifest Persistence

### Implementation Steps
- [x] Implement `AcquireLock`/`ReleaseLock` in `internal/engine/manifest.go` (per architecture sec 3.5, lock ownership lives in `manifest.go`, not a separate `lock.go`): atomic create-new (`[IO.File]::Open CreateNew` / `set -C`) returning exit 48 -> `ERR_LOCKED`; age >= `lock_timeout_seconds` overwrites with WARN; release runs in defer on all post-LOCK error paths.
- [x] Implement `engine/manifest.go` read/write of the JSON manifest (`current_version`, `previous_version`, `artifact_checksum`, `last_operation`, `extra`).
- [x] Implement the manifest-driven Read reconciliation helper: absent -> remove resource; `last_operation.result==failed` -> `<version>!failed` marker.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Lock fresh vs stale -- Given a fake transport returning exit 48 then an aged `.lock`, When acquire runs, Then the fresh case yields `ERR_LOCKED` naming the owner and the aged case overwrites with a WARN diag [proof: in-process; deps: none -- fake Transport with a scripted Result queue]
- [ ] Scenario: Manifest round-trip -- Given a manifest struct, When written then re-read through a fake transport, Then all fields including `last_operation` survive unchanged [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

## Stage 3.3: Staging Extraction and Junction Switch

### Implementation Steps
- [x] Implement staging wipe plus upload-or-target-pull orchestration into `staging/`.
- [x] Implement extraction scripts (`Expand-Archive -Force` / `unzip -o`, nupkg renamed to .zip) and the `.labdeploy-release.json` `{version,sha256,extracted_at}` writer.
- [x] Implement the junction switch (win `rmdir` + `mklink /J`, exit 42 -> `ERR_SWITCH`; linux `ln -sfn`).
- [x] Implement prune keeping `strategy.keep_releases` and never deleting `previous_version`.

### Dependencies
- phase-engine-core-and-manifest-state/stage-path-model-and-target-layout

### Test Scenarios
- [ ] Scenario: Switch and extract scripts golden -- Given a ReleaseCtx, When the extract and junction-switch scripts are generated for win and linux, Then they match the committed golden including the exit-42 guard [proof: golden; deps: none -- committed fixtures under internal/engine/testdata]
- [ ] Scenario: Prune keeps previous -- Given a releases set with `keep_releases=2` and a `previous_version`, When prune runs against a fake transport, Then the oldest is removed and `previous_version` is retained [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

## Stage 3.4: Health Checks and Log Collection

### Implementation Steps
- [x] Implement `engine/health.go` for http/tcp/exec/none executed ON the target with `initial_delay`/`interval`/`timeout` budget; exhaustion -> `ERR_HEALTH_CHECK`.
- [x] Implement `internal/logs/collect.go`: glob-to-zip on target, download, unzip into `results/`, and `windows_event_logs` collected since operation start.
- [x] Implement TRX and JUnit counter parsing (namespace-insensitive; sum counters across multiple result files).

### Dependencies
- phase-engine-core-and-manifest-state/stage-path-model-and-target-layout

### Test Scenarios
- [ ] Scenario: Health script generation -- Given each `health_check.type`, When scripts are generated for win/linux, Then they match the committed golden and encode `expect_status`/`expect_body_regex` logic [proof: golden; deps: none -- committed fixtures under internal/engine/testdata]
- [ ] Scenario: TRX and JUnit counters -- Given committed TRX and JUnit fixtures, When parsed, Then total/passed/failed/skipped match the expected counters (T8) [proof: golden; deps: none -- committed TRX/JUnit fixtures under internal/logs/testdata]

## Stage 3.5: Deploy State Machine and Rollback Matrix

### Implementation Steps
- [x] Define the `pattern.Pattern` seam interface and `ReleaseCtx` in `internal/pattern/pattern.go` (`Configure`/`Preflight`/`Stop`/`Start`/`Status`/`Uninstall`) plus the `pattern.For(type)` factory, so the engine orchestrates against a compile-safe interface before any concrete pattern lands (breaks the engine<->pattern cycle; concrete patterns follow in Phase 4).
- [x] Implement `engine/engine.go` `Deploy` orchestration emitting the exact named steps (VALIDATE..UNLOCK) via tflog with `app/host/step/version/duration_ms` fields, calling patterns through the `pattern.Pattern` seam.
- [x] Implement the idempotency short-circuit (DESIGN sec 10.1): current version + checksum + healthy status -> NO-OP with no fetch/restart.
- [x] Implement the single-target rollback matrix (DESIGN sec 10.2) for fresh vs update, and `ERR_ROLLBACK_FAILED` (sec 10.6) with detail starting `MACHINE IN UNKNOWN STATE host=<h>`.
- [x] Implement `Destroy` modes (purge/unregister/abandon) and `ReadStatus` reconciliation.

### Dependencies
- phase-engine-core-and-manifest-state/stage-locking-and-manifest-persistence
- phase-engine-core-and-manifest-state/stage-staging-extraction-and-junction-switch
- phase-engine-core-and-manifest-state/stage-health-checks-and-log-collection

### Test Scenarios
- [ ] Scenario: Rollback matrix rows -- Given a fake transport scripting a failure at each DESIGN sec 10.2 step, When `Deploy` runs, Then the resulting state matches the matrix row (staging wiped / fresh cleaned / update restored to previous) (T7) [proof: in-process; deps: none -- fake Transport with a scripted Result queue]
- [ ] Scenario: Idempotent short-circuit -- Given a manifest whose version and checksum equal the spec and a healthy status, When `Deploy` runs, Then it returns NO-OP with no FETCH or SWITCH step logged [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

# Phase 4: Single Target Deployment Patterns

## Dependencies
- phase-engine-core-and-manifest-state

## Stage 4.1: Pattern Interface and Console App

### Implementation Steps
- [x] Implement the concrete `ConsoleApp` against the existing `pattern.Pattern` seam (interface defined in Stage 3.5; its methods are `Configure`/`Preflight`/`Stop`/`Start`/`Status`/`Uninstall` -- there is no `Validate` method, static validation lives in `internal/spec`).
- [x] `console_app` behavior: no service registration, `post_install` then `verify_command` in the current release dir, `service_status` reports `n/a` or drift; `Start`/`Stop` are no-ops.
- [x] Wire the pattern-specific preflight tool-check seam invoked by the engine after connect.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Console verify flow -- Given a console_app ReleaseCtx, When `Configure`/`Start` scripts are generated, Then `Start` is a no-op and the `verify_command` script matches the committed golden [proof: golden; deps: none -- committed fixtures under internal/pattern/testdata]
- [ ] Scenario: Console status drift -- Given a manifest version that differs from `.labdeploy-release.json`, When `Status` is computed via a fake transport, Then it reports drift rather than `n/a` [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

## Stage 4.2: Windows Service Pattern

### Implementation Steps
- [x] Implement the `windows_service` S-steps: `sc.exe create`/`config` binPath, `start_type`, description, recovery actions, and non-builtin account with password via env-injected variable.
- [x] Implement stop with poll + `FORCE_KILL` (`taskkill /T /F`, exit 43 -> `ERR_SERVICE_STOP`) and start with poll + System event-log capture (exit 44 -> `ERR_SERVICE_START`).
- [x] Implement the HKLM `Environment` `REG_MULTI_SZ` writer (S5) using the full merged env map.
- [x] Implement the WinSW wrapper path: `.winsw.xml` generation and install/refresh (fallback uninstall+install).

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: S4 fresh vs update golden -- Given a windows_service spec, When the S4 scripts are generated for fresh install and update, Then both match the committed golden including recovery actions and env injection (T6) [proof: golden; deps: none -- committed fixtures under internal/pattern/testdata]
- [ ] Scenario: WinSW xml golden -- Given `wrapper: winsw`, When the WinSW xml is generated, Then it matches the committed golden with `stopwait` and env entries (T6) [proof: golden; deps: none -- committed fixtures under internal/pattern/testdata]

## Stage 4.3: Node and Dotnet Service Patterns

### Implementation Steps
- [x] Implement `node_web_app`: `npm ci --omit=dev` preflight (requires package-lock; missing -> `ERR_SERVICE_INSTALL`) then WinSW wrapper with `node_exe`/`entry`/`PORT`.
- [x] Implement `dotnet_api`: `launcher=exe` vs `dotnet_dll` binPath, aspnet-runtime preflight (`dotnet --list-runtimes` contains `Microsoft.AspNetCore.App`), `hosting` native/winsw, and `ASPNETCORE_URLS`.
- [x] Add pattern preflight tool checks (`node --version`, `npm --version`, `dotnet --list-runtimes`).

### Dependencies
- phase-single-target-deployment-patterns/stage-windows-service-pattern

### Test Scenarios
- [ ] Scenario: Dotnet binPath golden -- Given `launcher=dotnet_dll`, When the S4 script is generated, Then the binPath is the quoted `"<dotnet_exe>" "<release>\<dll>" <args>` form matching the committed golden [proof: golden; deps: none -- committed fixtures under internal/pattern/testdata]
- [ ] Scenario: Node install_deps preflight -- Given `install_deps=true` with no `package-lock.json`, When Preflight/Configure run via a fake transport, Then it yields `ERR_SERVICE_INSTALL` naming the lockfile [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

# Phase 5: Terraform Provider Surface

## Dependencies
- phase-single-target-deployment-patterns

## Stage 5.1: Provider and Deployment Resource Schema

### Implementation Steps
- [x] Implement `provider.go` with the optional `default_target` block and `Configure` passing defaults into resources.
- [x] Implement the `labdeploy_deployment` schema (arguments + computed attributes per DESIGN sec 5.2) with a `spec`/`spec_file` exactly-one-of validator.
- [x] Implement `spec_file` content hashing into the plan so file edits produce a diff.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: One-of validation -- Given both `spec` and `spec_file` set (and separately neither), When the schema is validated, Then the framework returns an `exactly one of` error (T9) [proof: in-process; deps: none -- terraform-plugin-framework schema unit test, no provider server]
- [ ] Scenario: Schema round-trip -- Given a deployment state, When read then written through the schema, Then the state round-trips without attribute loss [proof: in-process; deps: none -- terraform-plugin-framework schema unit test]

## Stage 5.2: Deployment Resource CRUD and Plan Modifiers

### Implementation Steps
- [x] Implement `Create`/`Update`/`Delete` calling engine `Deploy`/`Destroy`, mapping every engine error to a diagnostic whose Summary is `[<CODE>] <short>`.
- [x] Implement the RequiresReplace plan modifier that parses old+new spec and compares the immutable paths from Stage 1.4.
- [x] Populate computed outputs (`id`, `deployed_version`, `previous_version`, `hosts`, `release_path`, `service_status`, `spec_hash`).
- [x] Implement the timeouts block (create/update 30m, delete 15m) and the import-unsupported error `import is not supported; adopt via apply`.

### Dependencies
- phase-terraform-provider-surface/stage-provider-and-deployment-resource-schema

### Test Scenarios
- [ ] Scenario: RequiresReplace on immutable paths -- Given a plan changing `pattern.type` / `service_name` / `target.hosts` / `target.os`, When the plan is computed, Then each change triggers RequiresReplace (T9) [proof: in-process; deps: none -- terraform-plugin-framework plan unit test]
- [ ] Scenario: Import unsupported -- Given `terraform import` on `labdeploy_deployment`, When invoked, Then the error is `import is not supported; adopt via apply` [proof: in-process; deps: none -- terraform-plugin-framework schema unit test]

## Stage 5.3: Read Drift Reconciliation and Destroy Modes

### Implementation Steps
- [x] Implement `Read` refresh from the target manifest: absent -> `RemoveResource`; `last_operation.result==failed` -> `<version>!failed` marker forcing a converging plan; `service_status` from `pattern.Status`.
- [x] Implement `destroy_mode` purge/unregister/abandon in `Delete`.
- [x] Emit a once-per-apply WARN diag for insecure transport (`winrm.insecure_skip_verify=true` or `ssh.host_key=""`).

### Dependencies
- phase-terraform-provider-surface/stage-deployment-resource-crud-and-plan-modifiers

### Test Scenarios
- [ ] Scenario: Read absent manifest removes resource -- Given a missing manifest, When `Read` runs via a fake transport, Then the resource is removed from state (next plan = create) [proof: in-process; deps: none -- fake Transport with a scripted Result queue]
- [ ] Scenario: Failed op marker forces plan change -- Given a manifest with `last_operation.result==failed`, When `Read` runs, Then `deployed_version` carries the `!failed` suffix and the next plan is non-empty [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

# Phase 6: Failover Cluster Support

## Dependencies
- phase-terraform-provider-surface

## Stage 6.1: Cluster Generic Service Pattern Scripts

### Implementation Steps
- [x] Implement cluster preflight: `Import-Module FailoverClusters`, `Get-ClusterNode` Up-set superset of spec hosts, and role/service binding conflict -> `ERR_SERVICE_INSTALL` naming both services.
- [x] Implement per-node `sc.exe create start=demand` plus role binding scripts (`Add-ClusterGenericServiceRole`, optional `-StaticAddress`).
- [x] Implement `Set-ClusterOwnerNode` / `Move-ClusterGroup` / `Start-ClusterGroup` / `Stop-ClusterGroup` script generation.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Cluster create scripts golden -- Given a cluster_generic_service spec, When the create and move scripts are generated, Then they match the committed golden including `StaticAddress` and `preferred_owner` (T6) [proof: golden; deps: none -- committed fixtures under internal/pattern/testdata]
- [ ] Scenario: Role binding conflict -- Given an existing role bound to `other-svc`, When preflight runs via a fake transport, Then it yields `ERR_SERVICE_INSTALL` naming both services and nothing is modified [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

## Stage 6.2: Cluster Rolling Update and Rollback Engine

### Implementation Steps
- [x] Implement `engine/cluster.go` all-or-nothing multi-node lock (hosts order, release reverse) and CREATE steps C1..C6.
- [x] Implement UPDATE steps U0..U8 rolling passive-first with exactly one `MOVE_GROUP` failover.
- [x] Implement cluster rollback R1..R5 with `ERR_CLUSTER_MOVE` / `ERR_ROLLBACK_FAILED`.
- [x] Implement cluster `Read` (owner node, role State lowercased) and destroy (`Stop-ClusterGroup` -> `Remove-ClusterGroup -RemoveResources -Force`).

### Dependencies
- phase-failover-cluster-support/stage-cluster-generic-service-pattern-scripts

### Test Scenarios
- [ ] Scenario: Rolling update order -- Given a 2-3 node cluster fake transport, When UPDATE runs, Then passives are updated in hosts order, exactly one `MOVE_GROUP` occurs, and the owner ends on a new-release node [proof: in-process; deps: none -- fake Transport with a scripted Result queue]
- [ ] Scenario: Cluster health rollback -- Given a health failure on `firstNew` via a fake transport, When UPDATE runs, Then the role moves back to the original owner, both nodes junction to the previous version, and manifests read `rolled_back` [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

# Phase 7: E2E Test Resource and Results

## Dependencies
- phase-terraform-provider-surface

## Stage 7.1: TestRun Spec and Result Parsing

### Implementation Steps
- [x] Implement `TestRun` validation (target `hosts==1`, `docker_image` forbidden, runner types `exec`/`vstest`/`dotnet_test`/`npm`).
- [x] Implement runner command expansion (`vstest.console.exe ... /Logger:trx`, `dotnet test --logger trx --no-build`, `npm`).
- [x] Implement pass-criteria evaluation (`exit_codes`, `min_pass_rate` over `total-skipped`) and the `summary.json` writer; `results.format: none` yields counters of -1.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Runner expansion golden -- Given each `runner.type`, When the command is expanded, Then it matches the committed golden including results-dir flags [proof: golden; deps: none -- committed fixtures under internal/logs/testdata]
- [ ] Scenario: Pass criteria evaluation -- Given parsed counters plus criteria, When evaluated, Then `passed` reflects `exit_codes` and `min_pass_rate`, and `format: none` yields -1 counters [proof: in-process; deps: none]

## Stage 7.2: E2E Test Resource and Collection

### Implementation Steps
- [x] Implement the `labdeploy_e2e_test` schema (`spec`/`spec_file`, `deployment_id` edge, `triggers` RequiresReplace, `fail_on_test_failure`, computed outputs).
- [x] Implement `Create` running tests with process-tree kill on `runner.timeout_seconds` (`ERR_TIMEOUT`) and collection always before returning a test-failure error.
- [x] Implement results download/unzip into `destination_dir`, log + `windows_event_logs` collection since test start, and best-effort `Delete`.

### Dependencies
- phase-e2e-test-resource-and-results/stage-testrun-spec-and-result-parsing

### Test Scenarios
- [ ] Scenario: Collection before failure -- Given `fail_on_test_failure=true` and a failing run, When `Create` runs against a fake transport serving the on-target results zip, Then the resulting local `results_dir` tree (results + logs + `summary.json`) is byte-identical to a committed golden snapshot AND is fully written before `ERR_TEST_FAILED` is returned [proof: golden; deps: none -- committed expected results_dir snapshot under internal/engine/testdata; equality of the persisted output tree is the preservation proof]
- [ ] Scenario: Timeout kills tree -- Given `runner.timeout_seconds` exceeded, When `Create` runs via a fake transport, Then it returns `ERR_TIMEOUT` and issues the process-tree kill script [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

# Phase 8: Docker Examples and Packaging

## Dependencies
- phase-terraform-provider-surface

## Stage 8.1: Docker Container Pattern

### Implementation Steps
- [x] Implement the `docker_container` pattern D1..D7: login, pull by tag or digest, `rm -f`, `run` with ports/env/volumes/restart policy.
- [x] Implement rollback to the recorded old image id on health failure, and `docker logout` when login ran.
- [x] Implement the docker preflight (`docker version` -> `ERR_PREFLIGHT`) and record the image id in `manifest.extra`.

### Dependencies
- _none -- start stage_

### Test Scenarios
- [ ] Scenario: Docker run script golden -- Given a docker_container spec, When the D1..D7 scripts are generated, Then they match the committed golden including digest pull and rollback [proof: golden; deps: none -- committed fixtures under internal/pattern/testdata]
- [ ] Scenario: Docker preflight fail -- Given `docker version` failing via a fake transport, When `Preflight` runs, Then it yields `ERR_PREFLIGHT` [proof: in-process; deps: none -- fake Transport with a scripted Result queue]

## Stage 8.2: Examples and Pipeline Templates

### Implementation Steps
- [x] Author `examples/main.tf` and `examples/specs/*.yaml` (one per pattern plus e2e).
- [x] Author `examples/pipelines/github-deploy.yml` (deploy/e2e/rollback jobs, `always()` artifact upload).
- [x] Author `examples/pipelines/azure-pipelines.yml` (Deploy/publish/Rollback stages with `condition: always()` publish).
- [x] Add the `~/.terraformrc` filesystem-mirror example and the `required_providers` snippet for `registry.local/smartpcr/labdeploy`.

### Dependencies
- phase-docker-examples-and-packaging/stage-docker-container-pattern
- phase-e2e-test-resource-and-results/stage-e2e-test-resource-and-collection

### Test Scenarios
- [ ] Scenario: Example specs validate -- Given each committed `examples/specs/*.yaml`, When run through the spec validator, Then all parse and validate without error [proof: golden; deps: none -- committed examples/specs/*.yaml fixtures read from disk]
- [ ] Scenario: Pipeline YAML well-formed -- Given the two committed pipeline files, When parsed with a YAML parser, Then both are well-formed and contain the `always()`/`condition: always()` publish steps [proof: golden; deps: none -- committed examples/pipelines/*.yml fixtures read from disk]

## Stage 8.3: GoReleaser Packaging and Distribution

### Implementation Steps
- [ ] Add `.goreleaser.yml` producing `terraform-provider-labdeploy_v<version>_<os>_<arch>.zip` for linux_amd64 and windows_amd64 with `CGO_ENABLED=0`.
- [ ] Document the filesystem-mirror install layout for runners (win and linux paths per DESIGN sec 16.1).
- [ ] Add a release workflow invoking `goreleaser`.

### Dependencies
- phase-docker-examples-and-packaging/stage-examples-and-pipeline-templates

### Test Scenarios
- [ ] Scenario: Build matrix -- Given the goreleaser config, When `goreleaser build --snapshot` runs on the gate host, Then zips for linux_amd64 and windows_amd64 are produced with the correct binary name [proof: service:go-toolchain; deps: go-toolchain = the gate host's Go compiler + goreleaser (external build tool + emitted binaries)]
- [ ] Scenario: No cgo -- Given the built binaries, When inspected on the gate host, Then they are statically built with `CGO_ENABLED=0` [proof: service:go-toolchain; deps: go-toolchain = the gate host's Go toolchain producing the inspected binaries]

# Phase 9: Lab Acceptance Gate

## Dependencies
- phase-failover-cluster-support
- phase-e2e-test-resource-and-results
- phase-docker-examples-and-packaging

## Stage 9.1: Windows and Linux Single Target Acceptance

### Implementation Steps
- [ ] Wire the terraform-plugin-testing / terratest acceptance harness behind `TF_ACC=1` with env-provided W1/L1 targets, asserting `.lock` absent on all touched hosts after each scenario.
- [ ] Implement VAL/CON/ART/CAP scenario tests 1:1 with their DESIGN sec 18 IDs.
- [ ] Implement WSV/NOD/NET scenario tests on W1.
- [ ] Implement DRF/DST/IDP/LCK/RBK scenario tests.

### Dependencies
- phase-terraform-provider-surface/stage-read-drift-reconciliation-and-destroy-modes
- phase-single-target-deployment-patterns/stage-node-and-dotnet-service-patterns
- phase-docker-examples-and-packaging/stage-docker-container-pattern

### Test Scenarios
- [ ] Scenario: Windows single-target matrix -- Given W1 (Windows Server 2022, WinRM https, node+.NET+vstest), When WSV/NOD/NET/CAP acceptance run under `TF_ACC=1`, Then all pass and `.lock` is absent post-run [proof: lab; deps: W1 physical/VM Windows lab per architecture L1]
- [ ] Scenario: Linux console and drift -- Given L1 (Linux VM, ssh), When CAP-linux plus DRF/DST/IDP/LCK/RBK scenarios run under `TF_ACC=1`, Then the `current` symlink and drift/convergence behavior match DESIGN sec 18 [proof: lab; deps: L1 Linux VM per architecture L2]

## Stage 9.2: Cluster and Pipeline Acceptance

### Implementation Steps
- [ ] Implement CLU-01..08 acceptance against C2/C3 WSFC clusters under `TF_ACC=1`.
- [ ] Implement E2E-01..07 `labdeploy_e2e_test` acceptance against W1.
- [ ] Implement docker_container acceptance (ART-06 and docker patterns) against L1.
- [ ] Implement PIP-01..03 pipeline smoke via GitHub Actions + Azure DevOps against W1/C2.

### Dependencies
- phase-lab-acceptance-gate/stage-windows-and-linux-single-target-acceptance
- phase-failover-cluster-support/stage-cluster-rolling-update-and-rollback-engine
- phase-e2e-test-resource-and-results/stage-e2e-test-resource-and-collection
- phase-docker-examples-and-packaging/stage-examples-and-pipeline-templates

### Test Scenarios
- [ ] Scenario: Cluster rolling acceptance -- Given a C2/C3 WSFC lab, When CLU-01..08 run under `TF_ACC=1`, Then single-failover gap, correct owner, health rollback, and destroy are verified [proof: lab; deps: C2/C3 two/three-node WSFC lab per architecture L3]
- [ ] Scenario: Pipeline smoke -- Given a GitHub Actions runner and ADO agent against W1, When PIP-01..03 run, Then artifacts publish and the rollback job recovers the target to `v=1.1.0` [proof: lab; deps: GH Actions + ADO runners per architecture L4]
