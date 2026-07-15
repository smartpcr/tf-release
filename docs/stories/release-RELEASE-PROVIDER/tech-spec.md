# Tech Spec -- release:RELEASE-PROVIDER (`terraform-provider-labdeploy`)

Status: v1 planning artifact (normative framing).
Scope owner: Architect agent. Companion docs (do not duplicate): `architecture.md`
(component map, data model, interfaces, flows), `implementation-plan.md`
(sequencing, milestones), `e2e-scenarios.md` (acceptance matrix).

This document fixes the **problem framing** for the story: what we are building
and why, what is explicitly in and out of scope, the non-goals, the hard
constraints that bound every design choice, and the risks that must be actively
managed. Every claim here is anchored in the operator attachment
`docs/DESIGN.md` (cited as `DESIGN sec N`).

---

## 1. Problem Statement

The story "release provider -- implement terraform provider for release" asks for
a production Terraform provider that lets a CI pipeline deploy build artifacts to
lab machines, manage their app lifecycle, run E2E tests against them, and support
rollout / rollback / drift / destroy -- all declaratively (DESIGN sec 1).

Today a release to a lab target is imperative and hand-rolled: a pipeline copies
an artifact over SSH/WinRM, stops a service, swaps files, restarts, and hopes.
There is no single source of truth for "what version is on which host," no
first-class rollback, no drift detection, and no uniform contract for the five
deployment shapes the lab actually uses (console apps, Windows services, Node web
apps, .NET APIs, and Windows Failover Cluster generic-service roles). Secrets leak
into command lines and logs. Test result collection is bespoke per pipeline.

The provider (`terraform-provider-labdeploy`, Go, terraform-plugin-framework,
plugin protocol v6 -- DESIGN sec 1, sec 20) makes the target machine's deployment
state a Terraform-managed resource. A pipeline runs `terraform apply -var
version=<build>` and the provider:

1. Parses/validates a declarative `Deployment` spec (YAML or JSON), fetches the
   artifact (zip / nupkg / docker image), verifies its sha256, and installs it to
   an immutable, versioned release directory with an atomic `current` junction /
   symlink switchover (DESIGN sec 6, sec 9).
2. Manages app lifecycle for the five P0 patterns plus one P1 pattern
   (`docker_container`) across Windows and Linux targets (DESIGN sec 9, sec 14).
3. Runs E2E test binaries on the target via a second resource
   (`labdeploy_e2e_test`), evaluates pass criteria, and collects TRX/JUnit results
   plus logs back to the runner for pipeline publishing (DESIGN sec 5.3, sec 7).
4. Provides rollout (create/upgrade), automatic rollback-on-failure, explicit
   rollback (downgrade apply), drift detection via a provider-owned manifest, and
   destroy (DESIGN sec 10).

Success = the two resources (`labdeploy_deployment`, `labdeploy_e2e_test`) drive
the full acceptance matrix (DESIGN sec 18) green against real lab VMs, with
`terraform plan` empty on re-apply of an unchanged spec (idempotency), and with
secrets never appearing in state, plan, logs, or remote command lines
(DESIGN sec 11).

### 1.1 Primary users and the "deploy to target" clarification

- **CI pipeline (GitHub Actions / Azure DevOps)** is the caller. Secrets live in
  the pipeline as env vars; the provider reads only env-var *names* from the spec
  (DESIGN sec 11, sec 16).
- The story phrase "implement terraform provider for release" and the user
  requirement "deploy the provider to the target machine" are satisfied by
  `transport: local`: the provider binary always runs on the runner, and when the
  pipeline job runs on the target's own build agent, runner == target
  (DESIGN sec 3, Decision D1). This is a binding decision, not an open question.

---

## 2. In Scope (v1)

The following are committed deliverables for this story and are covered by the
acceptance matrix (DESIGN sec 18) referenced by ID.

### 2.1 Terraform surface

- Provider block `labdeploy` with an optional `default_target` (host, transport,
  os, port, credentials-by-env-name, winrm options) (DESIGN sec 5.1).
- Resource `labdeploy_deployment`: `spec`/`spec_file` (exactly one), `variables`,
  `version_override`, `destroy_mode`; computed `id`, `deployed_version`,
  `previous_version`, `hosts`, `release_path`, `service_status`, `spec_hash`;
  RequiresReplace plan modifier on immutable spec paths; per-op timeouts
  (create/update 30m, delete 15m) (DESIGN sec 5.2).
- Resource `labdeploy_e2e_test`: `spec`/`spec_file`, `variables`, `deployment_id`
  (dependency edge), `triggers` (RequiresReplace), `fail_on_test_failure`;
  computed `passed`, `exit_code`, counters, `results_dir`, `duration_seconds`,
  `summary`; collection always runs before a test-failure error is returned
  (DESIGN sec 5.3).

### 2.2 Transports (DESIGN sec 8.1)

- `ssh` (SFTP for file transfer), `winrm` (chunked base64 upload), and `local`
  (`os/exec` + file copy). Windows scripts are PowerShell 5.1 via
  `-EncodedCommand`; Linux scripts are `sh -c`. Connect retries with fixed
  backoff; auth rejection is not retried.

### 2.3 Artifact acquisition (DESIGN sec 6.3, sec 8.2, sec 8.3)

- Sources: `http`, `file`, `nuget_feed` (v3 flat container), `docker_registry`.
- Fetch modes: `target_pull` (default; runner never proxies large payloads) and
  `runner_push`. sha256 verification mandatory for zip/nupkg. nupkg treated as
  zip, unzipped in place (no NuGet client on target, Decision D3).

### 2.4 Deployment patterns (DESIGN sec 9, sec 14)

- P0: `console_app` (win+linux), `windows_service`, `node_web_app` (WinSW),
  `dotnet_api` (native Windows service or WinSW), `cluster_generic_service`
  (2..16 node WSFC generic-service role, rolling passive-first update).
- P1: `docker_container` (linux, or windows with docker preflight).

### 2.5 Lifecycle semantics (DESIGN sec 10)

- Idempotency short-circuit (empty plan on unchanged re-apply, IDP-01).
- Single-target failure/rollback matrix (fresh vs update) (sec 10.2).
- Explicit rollback via downgrade apply with release-cache reuse (sec 10.3, D8).
- Read/refresh reconciled from the provider-owned `manifest.json`, including the
  `!failed` drift signal (sec 10.4).
- Destroy modes `purge` / `unregister` / `abandon` (sec 10.5).
- `ERR_ROLLBACK_FAILED` "machine in unknown state" contract (sec 10.6).

### 2.6 E2E test execution (DESIGN sec 7)

- Runners `exec`, `vstest`, `dotnet_test`, `npm`; TRX/JUnit parsing; pass criteria
  (exit codes + min pass rate); result + log collection to a local `results_dir`
  with a `summary.json`.

### 2.7 Cross-cutting (DESIGN sec 11, sec 12, sec 13, sec 15)

- Secret hygiene (env-var-name indirection; values never persisted).
- Stable error taxonomy (`ERR_*` codes with remote exit-code markers).
- Per-host `.lock` concurrency control with stale-lock override.
- Structured `tflog` observability with secret redaction.

### 2.8 Shipped examples and packaging (DESIGN sec 4, sec 16, sec 20)

- `examples/main.tf`, one spec per pattern under `examples/specs/`, and reference
  pipelines (`github-deploy.yml`, `azure-pipelines.yml`).
- Filesystem-mirror provider distribution; `goreleaser` packaging; `make build
  test`; `golangci-lint` clean; Go >= 1.22, no cgo.

---

## 3. Out of Scope (v1)

These are explicitly excluded; attempting them would break the story budget or
the constraints in sec 5. Each maps to a DESIGN non-goal or a "v2" deferral.

- **Multi-tenant orchestration** across many apps/teams in one apply
  (DESIGN sec 1 non-goals).
- **Kubernetes** as a deployment target or hosting model (DESIGN sec 1).
- **IIS hosting** for web apps (Node/.NET run as services or console, never under
  IIS) (DESIGN sec 1).
- **Linux systemd services** -- on Linux only `console_app` and `docker_container`
  are supported; no unit-file management (DESIGN sec 1, sec 14).
- **CSV-hosted cluster binaries** -- cluster binaries live on each node's local
  disk at identical paths; Cluster Shared Volumes are v2 (DESIGN sec 9.5,
  Decision D5).
- **Cluster + service wrapper (WinSW)** -- cluster generic-service roles require an
  SCM-aware exe; wrapper is not supported for clusters v1 (Decision D5b).
- **Blue/green with load balancers** (DESIGN sec 1).
- **Secret storage / vault integration** -- secrets always arrive as pipeline env
  vars; the provider stores none (DESIGN sec 1, sec 11, Decision D6).
- **Terraform import** -- unsupported v1; adoption is via apply, with a clear error
  (DESIGN sec 5.2, Decision D9).
- **Provider registry publishing** -- distribution is via filesystem mirror only;
  no Terraform Registry release (DESIGN sec 16.1).
- **Target-machine bootstrapping** -- installing the GH/ADO build agent and base OS
  tooling on the target is out of scope; targets are pre-provisioned
  (DESIGN sec 2, sec 18 harness notes).
- **Third-party binary downloads** -- the provider never downloads WinSW,
  node/npm, dotnet, unzip, or docker; the artifact must ship WinSW (`winsw_exe`)
  and the target must pre-provide the rest (Decision D2; preflight enforces).
- **Docker image checksum via `artifact.checksum`** -- docker uses registry digest,
  not sha256 (DESIGN sec 6.3).
- **docker_image artifacts for the E2E test resource** -- `TestRun` rejects
  `docker_image` (DESIGN sec 7).

---

## 4. Non-Goals (design intent -- what we deliberately will NOT optimize for)

Distinct from "out of scope" (a feature we don't build), these are properties we
deliberately do not pursue even where tempting, because DESIGN pins a simpler
contract:

- **No zero-downtime for single-node service updates.** A single `windows_service`
  update accepts a brief stop->switch->start window bounded by
  `stop_timeout_seconds + ~15s` (DESIGN sec 18 WSV-02). Only the cluster pattern
  targets bounded, single-failover downtime (DESIGN sec 9.5, CLU-02). We do not
  build connection draining or overlapping instances for non-cluster patterns.
- **No provider-side secret management.** We do not encrypt, cache, or broker
  secrets; env-var-name indirection is the whole model (DESIGN sec 11).
- **No retained Terraform state for failed E2E runs.** A failed
  `labdeploy_e2e_test` Create leaves no TF state; `triggers`-based replace makes
  re-runs cheap. This is intentional, not a bug (DESIGN Decision D13, E2E-02).
- **No cross-runner distributed locking.** Locking is a single per-host `.lock`
  file; we do not build a lock service or lease coordinator (DESIGN sec 13).
- **No abstraction for future hosting models.** We will not pre-build generic
  "host" plumbing for IIS/systemd/k8s "just in case." The pattern interface
  (DESIGN sec 8.4) is added to only when a concrete pattern needs it.
- **No PowerShell 7 (pwsh) code paths.** Windows targets are Windows PowerShell
  5.1 only; we do not detect or prefer pwsh7 (Decision D10).
- **No runner-side heavy proxying.** Large payloads are pulled by the target
  (`target_pull`); we do not optimize a runner-through streaming path beyond the
  `runner_push` fallback (DESIGN sec 8.3).

---

## 5. Hard Constraints (binding -- violating any one fails the story)

These bound every downstream design and implementation choice. They are sourced
from DESIGN and are non-negotiable for v1.

### 5.1 Platform / protocol

- Go >= 1.22, **no cgo**; terraform-plugin-framework v1.x, **plugin protocol v6**;
  deps pinned in `go.mod` (winrm, x/crypto, pkg/sftp, yaml.v3, tflog).
  `golangci-lint run` clean; `make build test` green. Binary named
  `terraform-provider-labdeploy_v<version>` (DESIGN sec 20).
- Windows target scripting baseline is **Windows PowerShell 5.1**, invoked as
  `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass
  -EncodedCommand <base64(UTF-16LE)>`; identical over WinRM and SSH. Linux is
  `sh -c` (DESIGN sec 8.1, Decision D10).
- No external runner dependencies beyond Terraform itself; the provider never
  downloads third-party binaries (DESIGN sec 20, Decision D2).

### 5.2 Secret hygiene (MUSTs -- DESIGN sec 11)

- Specs and state carry env-var **names** only; values are resolved at runtime via
  `os.Getenv`. Missing env var => `ERR_SPEC_INVALID` before any dial.
- Secret values MUST NEVER appear in TF state, plan diffs, `tflog` at any level,
  error messages, or remote command lines (env-injection only; single-quote
  escaped, prepended to the encoded script).
- Variable substitution `${var:NAME}` draws only from the resource `variables`
  map, never process env, to prevent secret leakage into `spec_hash`/state
  (DESIGN sec 6.6).
- Insecure lab options (`winrm.insecure_skip_verify=true`, `ssh.host_key=""`) are
  permitted but MUST emit exactly one WARN diag per apply (DESIGN sec 11).

### 5.3 State / plan correctness

- Immutable spec paths MUST force RequiresReplace: `pattern.type`,
  `pattern.*.service_name`, `pattern.*.role_name`, `pattern.install_root`,
  `metadata.name`, `target.hosts` (set inequality), `target.os`
  (DESIGN sec 5.2).
- `spec_hash` MUST be sha256 of **canonical JSON** (sorted keys, no insignificant
  whitespace) computed after YAML->JSON, `${var:*}` substitution, and
  `version_override`; hash MUST be stable across key ordering (DESIGN sec 6, D7).
- Re-apply of an unchanged spec MUST yield an empty plan (idempotency
  short-circuit, DESIGN sec 10.1, IDP-01).
- On a failed **update**, TF state MUST keep the old version while the machine is
  restored to the old version -- consistent by construction (DESIGN Decision D11,
  sec 10.2).
- Read MUST reconcile from the provider-owned `manifest.json`; a missing manifest
  MUST `RemoveResource`; a failed last operation MUST surface drift via the
  `!failed` marker (DESIGN sec 10.4).

### 5.4 Deployment invariants

- Releases are **immutable** and versioned at `<root>/<app>/releases/<version>/`;
  `current` is an atomic junction (Windows `mklink /J`) / symlink (Linux
  `ln -sfn`); switch failure => `ERR_SWITCH` (exit 42) (DESIGN sec 9.1).
- Health checks MUST execute **on the target** (localhost, firewall-independent),
  never from the runner (DESIGN Decision D4, sec 6.5).
- Every scenario ends with the `.lock` **absent** on all touched hosts; the lock
  is released in FINALIZE and in all post-LOCK error paths (DESIGN sec 13, sec 18).
- Prune keeps at least `keep_releases` (default 3) and MUST NEVER delete the
  `previous_version`; pruning runs only after a successful FINALIZE
  (DESIGN sec 6.5, Decision D12).
- `keep_releases >= 1`; cluster binaries reside on each node's local disk at
  identical `install_root` paths (DESIGN sec 9.5, Decision D5).

### 5.5 Pattern x OS support matrix (validation -- DESIGN sec 14)

- `windows_service`, `node_web_app`, `dotnet_api`, `cluster_generic_service` are
  Windows-only; Linux use => `ERR_SPEC_INVALID`. `console_app` is both;
  `docker_container` is Linux or Windows-with-docker-preflight.
- Artifact x pattern: `docker_image` only with `docker_container`; `zip`/`nupkg`
  with everything except `docker_container`; else `ERR_SPEC_INVALID`.

### 5.6 Contracts that pipelines and tests depend on

- Error taxonomy is a **stable contract**: every diagnostic Summary is exactly
  `[<CODE>] <short>`; Detail includes `host=<h> step=<STEP>` plus remediation.
  Remote exit-code markers are fixed (e.g. 41 `ERR_CHECKSUM_MISMATCH`, 42
  `ERR_SWITCH`, 43 `ERR_SERVICE_STOP`, 44 `ERR_SERVICE_START`, 45
  `ERR_HEALTH_CHECK`, 46 `ERR_SERVICE_INSTALL`, 47 `ERR_CLUSTER_MOVE`, 48 lock)
  (DESIGN sec 12, sec 13).
- Engine step names are fixed and logged verbatim (VALIDATE, CONNECT, PREFLIGHT,
  LOCK, FETCH, CHECKSUM, STAGE, EXTRACT, RENDER, CONFIGURE, STOP, SWITCH, START,
  HEALTH, FINALIZE, PRUNE, UNLOCK, ROLLBACK, FORCE_KILL, MOVE_GROUP)
  (DESIGN sec 8.5).
- E2E collection MUST populate `results_dir` fully **before** returning a
  test-failure error, so `always()` pipeline steps can publish (DESIGN sec 5.3,
  sec 16, E2E-02).
- Scenario IDs in DESIGN sec 18 MUST appear 1:1 in test names (DESIGN sec 18).

---

## 6. Identified Risks

Ranked by likelihood x blast radius. Each names a concrete mitigation anchored in
DESIGN; residual risk is where the design leaves us exposed.

### R1 -- Lab-environment fidelity for cluster and multi-OS tests (HIGH)

The acceptance gate (DESIGN sec 18) requires a two-node WSFC (C2), an optional
three-node WSFC (C3), a Windows Server 2022 box with Node/.NET/vstest (W1), and a
Linux+docker VM (L1). Provisioning these is explicitly out of scope for this repo
(DESIGN sec 18 harness notes), so CI can only partially self-verify.
- *Mitigation:* unit tests with a fake `Transport` cover every rollback-matrix row
  and all script generation via golden files (DESIGN sec 17); `transport: local`
  gives honest coverage of the real Exec/Upload/Download path on the gate host
  without external services (per sibling `architecture.md` sec 7 decision
  D-gate-1). Cluster/service script generation is pinned by golden tests when the
  gate OS cannot run the pattern live.
- *Residual:* true failover behavior (CLU-02/04) can only be proven on real WSFC
  hardware; the gate relies on golden pins for those paths.

### R2 -- Rollback correctness under partial mutation (HIGH)

The single-target and cluster rollback flows (DESIGN sec 10.2, sec 9.5 U5/R-steps)
are the hardest correctness surface: a failed update must leave the old version
*running* with TF state unchanged, and a failed rollback must produce the precise
`ERR_ROLLBACK_FAILED` "MACHINE IN UNKNOWN STATE" contract (sec 10.6).
- *Mitigation:* the failure/rollback matrix is enumerated (sec 10.2) and each row
  is a required engine unit test with a scripted fake-transport Result queue
  (DESIGN sec 17); WSV-03/04/05 and CLU-04 assert live post-state.
- *Residual:* the cluster double-fault path (new fails AND restore fails) is
  timing-dependent and only exercisable on real hardware.

### R3 -- Secret leakage into state/plan/logs (HIGH impact, LOW likelihood)

A single stray interpolation of a resolved secret into `spec_hash`, a diagnostic,
or a remote command line violates sec 5.2 and is a security defect.
- *Mitigation:* env-var-name indirection everywhere (Decision D6); `${var:*}`
  never reads process env (sec 6.6); env values injected only via encoded-script
  env prepending; `TF_LOG=DEBUG` redacts values (`$env:K='***'`, sec 15). Unit
  tests assert secrets are absent from `sc qc` captures at TRACE (WSV-08).
- *Residual:* new patterns could reintroduce a command-line secret; requires a
  standing review rule on any `Configure`/`Exec` that handles credentials.

### R4 -- PowerShell 5.1 / WinRM encoding and chunking correctness (MEDIUM)

All Windows behavior rides on `-EncodedCommand` (base64 of UTF-16LE) and chunked
base64 WinRM upload (48,000-byte chunks). Off-by-one chunking or bad env escaping
silently corrupts deployments (DESIGN sec 8.1).
- *Mitigation:* transport unit tests assert exact encoded bytes for a known
  script, chunking math at boundaries (0, 1, 48000, 48001), and single-quote
  escaping (`O'Brien`) (DESIGN sec 17).
- *Residual:* PS 5.1 quoting edge cases in operator-supplied `post_install` /
  `verify_command` strings remain a sharp edge.

### R5 -- Drift-detection false positives / convergence loops (MEDIUM)

Read maps `manifest.last_operation.result==failed` to a `!failed` deployed_version
so the plan is non-empty (sec 10.4). A mis-scoped comparison could produce a
perpetual non-empty plan or, worse, mask real drift.
- *Mitigation:* DRF-01/02/03 assert precise plan shapes (Start-only, CREATE,
  version drift) and idempotency (IDP-01) asserts unchanged mtimes and empty plan.
- *Residual:* `service_status` semantics differ per pattern (`n/a`, `online`,
  `running`); a pattern returning an unexpected status string would surface as
  spurious drift.

### R6 -- Preflight gaps for target tooling (MEDIUM)

Patterns assume `node`/`npm`, `dotnet` runtime (incl. `Microsoft.AspNetCore.App`),
`unzip`, `docker`, and the `FailoverClusters` module are present -- but the
provider never installs them (Decision D2, sec 8.1 preflight).
- *Mitigation:* pattern-specific preflight runs before any file copy and fails
  fast with `ERR_PREFLIGHT` naming the missing tool (NET-02, NOD-02); disk-space
  preflight (>= 2x artifact size) guards extraction.
- *Residual:* version-skew (e.g. wrong Node major) passes preflight but fails at
  runtime; mitigated only by the app's own health check.

### R7 -- Concurrency / stale-lock handling (MEDIUM)

Two overlapping applies to the same host must fail-fast (`ERR_LOCKED`, < 5s), yet
a crashed prior apply must not wedge the host forever (DESIGN sec 13).
- *Mitigation:* atomic `CreateNew`/`set -C` lock creation; age >= `lock_timeout`
  overrides with a WARN (LCK-01, LCK-02); lock released in FINALIZE and all error
  paths (defer). Cluster locks acquired in hosts order, released reverse.
- *Residual:* clock skew across cluster nodes can misjudge lock age; the design
  uses per-host wall clock with a generous 900s default.

### R8 -- Artifact source variability (LOW/MEDIUM)

Four source types (http, file, nuget flat-container, docker registry) each have
distinct auth and URL-construction rules (DESIGN sec 6.3, sec 8.2). A wrong NuGet
flat-container URL or unverified checksum ships a bad payload.
- *Mitigation:* streamed sha256 verification with `ERR_CHECKSUM_MISMATCH` and
  temp-file cleanup (ART-02); nuget URL construction unit-tested; `target_pull`
  vs `runner_push` selected deterministically (ART-05); docker uses digest.
- *Residual:* private-feed auth-scheme variety (bearer vs basic) may need tuning
  per real feed.

### R9 -- Scope creep from the terse story prompt (LOW likelihood, HIGH impact)

The one-line story ("implement terraform provider for release") is far smaller
than the 21-section DESIGN. Divergent interpretation (e.g. adding IIS, systemd, or
import) would blow the 8-point budget.
- *Mitigation:* DESIGN sec 21 Decision Table is binding and resolves every
  ambiguity; this doc's sec 3/sec 4 pin the exclusions; the sibling
  `architecture.md` sec 7 records that no open questions remain.
- *Residual:* none if the Decision Table is treated as authoritative -- which this
  spec mandates.

---

## 7. Traceability

| This doc | DESIGN source | Acceptance IDs |
|---|---|---|
| sec 1 Problem | sec 1, sec 3, D1 | -- |
| sec 2 In scope | sec 4-16 | VAL/CON/ART/CAP/WSV/NOD/NET/CLU/RBK/DRF/DST/IDP/LCK/E2E/PIP |
| sec 3 Out of scope | sec 1 non-goals, D3/D5/D5b/D9 | -- |
| sec 4 Non-goals | D6/D10/D11/D13 | E2E-02 |
| sec 5 Constraints | sec 5, 6, 8, 10-14, 20 | IDP-01, WSV-02/08 |
| sec 6 Risks | sec 9, 10, 11, 13, 17, 18 | CLU-02/04, DRF-*, LCK-*, NET-02 |

Cross-doc note: this spec and `architecture.md` agree that `transport: local` on
the gate host (arch sec 7, D-gate-1) is the honest CI coverage path; no
inconsistency found at iter 1. If `implementation-plan.md` or `e2e-scenarios.md`
later restate the acceptance matrix, they should reference DESIGN sec 18 IDs
rather than re-deriving them, to keep a single source of truth.

---

## 8. Open Questions

None. All ambiguity in the story prompt is resolved by the DESIGN Decision Table
(DESIGN sec 21) and pinned in sec 3-5 above; the sibling `architecture.md` sec 7
also emits no open questions. This document therefore emits no `open-questions`
block.
