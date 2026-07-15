# release provider -- Architecture

Story: `release:RELEASE-PROVIDER` -- "implement terraform provider for release".
Normative source: `.forge-attachments/DESIGN.md` (terraform-provider-labdeploy v1.0).
Code is already scaffolded under `internal/`; this document describes the
component boundaries, data model, interfaces, and end-to-end flows that the
implementation must realize, plus the Test & Environment Contract that binds the
sibling `implementation-plan.md` and `e2e-scenarios.md`.

This doc anchors on the REAL tree (module
`github.com/smartpcr/terraform-provider-labdeploy`), which differs slightly from
the DESIGN sketch: paths live in `internal/layout` (not `engine/paths.go`), and
the three simple service/console patterns share `internal/pattern/console_node_dotnet.go`.

Sibling docs (some may not exist on iter 1) own detail this doc references by
name only:
- `tech-spec.md` -- exhaustive schema/validation field tables (DESIGN sec 6/sec 7).
- `implementation-plan.md` -- milestone order + package build sequence (DESIGN sec 19).
- `e2e-scenarios.md` -- the full E2E matrix (DESIGN sec 18).

---

## 1. Component Map & Responsibilities

The provider is a single Go binary (`terraform-plugin-framework`, plugin protocol
v6) that always runs on the CI runner and drives target lab machines over
SSH / WinRM / local exec. All secrets stay as env-var NAMES in spec/state; values
are resolved from the runner environment at apply time (DESIGN sec 11, D6).

```
+-----------------------------------------------------------------------+
| terraform CLI  --gRPC-->  terraform-provider-labdeploy (runner binary) |
+-----------------------------------------------------------------------+
        |                                                       
        v                                                       
  internal/provider   (TF plugin surface: provider + 2 resources)         
        |                                                       
        v                                                       
  internal/engine     (deploy/destroy/read state machine + cluster)       
   |      |       |        |                                    
   v      v       v        v                                    
 spec  artifact  layout  pattern  --uses-->  transport  --> Target machine(s)
                                              (ssh|winrm|local)             
                          logs (collect results + event logs -> runner)     
```

| Package (dir) | Responsibility | Key files (real) |
|---|---|---|
| `internal/provider` | terraform-plugin-framework wiring: provider block, `labdeploy_deployment`, `labdeploy_e2e_test`; schema, plan modifiers (RequiresReplace on immutable paths), CRUD glue, state marshaling. | `provider.go`, `deployment_resource.go`, `e2e_test_resource.go` |
| `internal/spec` | Parse YAML/JSON -> typed structs; `${var:*}` substitution + escape; `version_override`; validation of every sec 6/sec 7 rule; canonical JSON + `spec_hash`. | `types.go`, `parse.go`, `validate.go` |
| `internal/transport` | `Transport` abstraction over ssh / winrm / local. Encoded PowerShell 5.1 on Windows, `sh -c` on Linux; secret-safe env injection; chunked upload; connect retry/backoff; ERR_AUTH vs ERR_CONNECT classification. | `transport.go`, `ssh.go`, `winrm.go`, `local.go` |
| `internal/artifact` | Fetch zip/nupkg/docker ref: http/file/nuget streaming with sha256 verify (runner_push) or emit target-pull scripts (target_pull). | `fetch.go` |
| `internal/layout` | Path algebra for `<root>/<app>/{releases,current,shared,staging,manifest.json,.lock}`; builtin env (`LD_APP`, `LD_VERSION`, `LD_RELEASE_DIR`, `LD_SHARED_DIR`, `PORT`); env merge (spec wins). | `paths.go` |
| `internal/pattern` | Per-pattern verbs (`Configure/Stop/Start/Status/Uninstall/Preflight`): SCM/WinSW/docker/cluster service registration and lifecycle. Engine owns files/junction/health/manifest. | `pattern.go`, `windows_service.go`, `console_node_dotnet.go`, `cluster_generic.go`, `docker_container.go` |
| `internal/engine` | The orchestration state machine: VALIDATE..UNLOCK step sequence, idempotency short-circuit, rollback matrix, Read reconciliation, destroy modes, locking, manifest, health, pruning; cluster rolling update; TestRun execution. | `engine.go`, `cluster.go`, `manifest.go`, `health.go`, `testrun.go` |
| `internal/logs` | Collect result files (TRX/JUnit) + globbed logs + Windows event logs from target; zip/download/unzip into `results_dir`; parse counters; write `summary.json`. | `collect.go` |
| `main.go` | `providerserver.Serve` with address `registry.local/smartpcr/labdeploy`. | `main.go` |

Design invariant: **engine owns the release machinery** (fetch, stage, extract,
junction switch, files/env render, health, manifest, prune, lock); **patterns own
the service/SCM/docker/cluster verbs** (DESIGN sec 8.4). This split is why the same
engine drives all six patterns via one `Pattern` interface.

---

## 2. Data Model (entities + key fields)

### 2.1 Terraform resource state (persisted)

`labdeploy_deployment` (DESIGN sec 5.2) -- configured + computed:

| Field | Kind | Notes |
|---|---|---|
| `spec` / `spec_file` | config, one-of | inline YAML/JSON or path (content hashed into plan) |
| `variables` | config | `${var:NAME}` source map (never process env) |
| `version_override` | config | pipeline build number overrides `artifact.version` |
| `destroy_mode` | config | `purge` (default) \| `unregister` \| `abandon` |
| `id` | computed | `sha1(sorted(hosts)+"/"+name)[0:12] + ":" + name` |
| `name` | computed | from `metadata.name` |
| `deployed_version` / `previous_version` | computed | reconciled from manifest by Read |
| `hosts` | computed | resolved targets |
| `release_path` | computed | current release path (owner node for cluster) |
| `service_status` | computed | `running\|stopped\|not_installed\|n/a\|online\|offline` |
| `spec_hash` | computed | sha256 of canonical JSON post substitution+override |

`labdeploy_e2e_test` (DESIGN sec 5.3) -- computed outputs: `passed`, `exit_code`,
`total_tests`/`passed_tests`/`failed_tests`/`skipped_tests` (`-1` when
`results.format: none`), `results_dir` (LOCAL runner path), `duration_seconds`,
`summary`. `triggers` map is RequiresReplace (re-run per build).

### 2.2 Spec domain model (`internal/spec`, in-memory)

`Deployment` (kind `Deployment`) is a discriminated union on `pattern.type`:
`Envelope{apiVersion,kind,metadata}` + `Target` + `Artifact` + `Pattern` +
`Environment` + `Files[]` + `HealthCheck` + `Strategy` + `Logs`. `TestRun`
(kind `TestRun`) reuses `Target`/`Artifact` and adds `Runner`/`Results`/
`PassCriteria`/`Collect`. Full field tables live in `tech-spec.md`; the six
pattern variants and the pattern x os support matrix (DESIGN sec 14) are the
canonical validation surface.

`OSKind` = `windows|linux`. Enums: transport (`ssh|winrm|local`), artifact type
(`zip|nupkg|docker_image`), source type (`http|file|nuget_feed|docker_registry`),
pattern type (`console_app|windows_service|node_web_app|dotnet_api|
cluster_generic_service|docker_container`).

### 2.3 Target-side manifest (provider-owned source of truth for Read/drift)

`internal/engine/manifest.go` -> `Manifest` at `<root>/<app>/manifest.json`:

| Field | Meaning |
|---|---|
| `current_version` | active release version (junction target) |
| `previous_version` | prior release (`""` when none); rollback target |
| `artifact_checksum` | `sha256:...` of current release (idempotency + downgrade cache) |
| `pattern_type` | pattern that owns the unit |
| `service_name` / `role_name` | registered SCM service / cluster group |
| `install_root` / `release_path` | resolved paths |
| `last_operation` (`LastOp`) | `{type, result: succeeded\|failed\|rolled_back, ts}` (drives sec 10.4 drift) |
| `extra` | pattern scratch (e.g. docker image id for rollback) |

Each release dir also carries `.labdeploy-release.json` =
`{version, sha256, extracted_at}` (local proof used by drift + downgrade cache).

### 2.4 Lock

`.lock` JSON `{owner:"<runner-host>/<pid>", op, started_utc}`, created with
exclusive `CreateNew` (win) / `set -C` (linux) semantics. Fresh & within
`lock_timeout_seconds` -> ERR_LOCKED (fail fast <5s); stale -> overwrite + WARN.
Cluster acquires on every node in hosts order, releases reverse.

### 2.5 Path model (`internal/layout`)

`Paths` (from `NewPaths(os, install_root, app, version)`) exposes App/Release/
Current/Shared/Staging/Manifest/Lock, with `\` on Windows and `/` on Linux.
`BuiltinEnv` + `MergeEnv` produce the effective process/service environment
(spec `environment` wins over builtins).

---

## 3. Interfaces Between Components

### 3.1 provider -> engine

Resources translate TF config/state to a validated `*spec.Deployment` (or
`*spec.TestRun`) and call the engine. `engine.New()` builds an `Engine` whose
`NewTransport` factory is swappable for fake-transport unit tests (DESIGN sec 17).

```go
// internal/engine
func (e *Engine) Deploy(ctx, s *spec.Deployment) (*Status, error)   // Create + Update
func (e *Engine) Destroy(ctx, s *spec.Deployment, mode string) error
func (e *Engine) ReadStatus(ctx, s *spec.Deployment) (*Status, error) // refresh/drift
// Status{DeployedVersion, PreviousVersion, ReleasePath, ServiceStatus, Hosts}
```

`Deploy` routes `pattern.type == cluster_generic_service` to `cluster.go`
(`deployCluster`), everything else to `deploySingle`. Errors surface as
`*engine.CodedError` (`Code`, `Host`, `Step`, `Err`) rendered into TF diagnostics
as Summary `[<CODE>] <short>` + Detail `host=<h> step=<STEP>` (DESIGN sec 12).

### 3.2 engine -> transport

```go
// internal/transport
type Transport interface {
    Connect(ctx) error; Close() error
    OS() spec.OSKind; Host() string
    Exec(ctx, Cmd) (Result, error)         // err = transport failure only; app exit -> Result
    Upload(ctx, local io.Reader, size int64, remote string) error
    Download(ctx, remote, local string) error
}
type Cmd struct{ Shell Shell; Script string; TimeoutSec int; Env map[string]string }
type Result struct{ ExitCode int; Stdout, Stderr string }
```

Contract highlights: Windows scripts always run as encoded PowerShell 5.1
(`-EncodedCommand base64(UTF-16LE)`), identical over WinRM and SSH; env injected
by prepended `$env:K='V';` lines (never on the command line -- secret safety);
Linux uses `sh -c` with `K='V'` prefix; auth rejection is never retried (ERR_AUTH),
dial/timeout is (ERR_CONNECT). `Result.ExitCode` carries the remote exit markers
(41-48) that the engine maps to the error taxonomy.

### 3.3 engine -> pattern

```go
// internal/pattern
type Pattern interface {
    Preflight(ctx, t, rc ReleaseCtx) error
    Configure(ctx, t, rc ReleaseCtx) error  // idempotent; files already switched
    Stop(ctx, t, rc) error                   // no-op if absent/stopped
    Start(ctx, t, rc) error                  // blocks until Running / ok
    Status(ctx, t, rc) (string, error)       // sec 5.2 service_status values
    Uninstall(ctx, t, rc, purge bool) error
}
type ReleaseCtx struct{ App, Version, PrevVersion string; P layout.Paths;
                        Spec *spec.Deployment; Env map[string]string }
```

Pattern selection is a factory on `pattern.type`. Exit markers are shared consts
(`ExitChecksum=41`..`ExitClusterMove=47`) so remote scripts and Go error mapping
stay in lockstep.

### 3.4 engine -> artifact / layout / logs

```go
// internal/artifact
Fetch(ctx, s spec.Artifact, secrets) (Fetched{LocalPath,Sha256,Size}, error) // runner_push
TargetPullScript(s spec.Artifact) (script string, ok bool)                   // target_pull

// internal/logs
Collect(ctx, t, spec.Collect, destDir) (Summary, error) // TRX/JUnit parse + zip/download
```

### 3.5 engine internal contracts (`manifest.go`)

`ReadManifest` / `WriteManifest`, `AcquireLock(...timeoutSec) (staleWarn, err)` /
`ReleaseLock`, and the `coded(code,host,step,err)` constructor are the shared
primitives every step uses. Every remote step is logged via `tflog` with fields
`app, host, step, version, duration_ms`; step names are the fixed set VALIDATE
CONNECT PREFLIGHT LOCK FETCH CHECKSUM STAGE EXTRACT RENDER CONFIGURE STOP SWITCH
START HEALTH FINALIZE PRUNE UNLOCK ROLLBACK FORCE_KILL MOVE_GROUP.

---

## 4. End-to-End Sequence Flows

### 4.1 Fresh single-target deploy (windows_service, primary happy path)

```
terraform apply -var version=1.0.0
 provider.Create -> spec.Parse -> substitute vars -> version_override
                 -> spec.Validate (sec 6, sec 14 matrix) -> canonical JSON + spec_hash
 engine.Deploy -> deploySingle(host):
   CONNECT (retry/backoff)  -> ERR_CONNECT | ERR_AUTH
   PREFLIGHT (PS>=5, disk>=2x, node/dotnet/docker tool checks) -> ERR_PREFLIGHT
   LOCK (.lock CreateNew)   -> ERR_LOCKED (fast); defer UNLOCK on all paths
   [idempotency short-circuit sec 10.1: manifest.version==spec & checksum==spec &
    Status healthy -> NO-OP success, empty plan on re-apply]
   FETCH  (runner_push: artifact.Fetch + Upload | target_pull: TargetPullScript)
   CHECKSUM (sha256 verify) -> ERR_CHECKSUM_MISMATCH (exit 41; staging wiped)
   STAGE + EXTRACT (Expand-Archive) -> write .labdeploy-release.json
   RENDER (files[] + env merge)
   pattern.Configure: SWITCH junction (mklink /J, exit 42) -> S4 sc create
        -> S5 HKLM Environment REG_MULTI_SZ
   pattern.Start: Start-Service, poll Running<=60s -> ERR_SERVICE_START (exit 44,
        captures 7000/7009/7031/7034 events)
   HEALTH (sec 6.5 on target) -> ERR_HEALTH_CHECK (exit 45)
   FINALIZE (WriteManifest current=1.0.0) -> PRUNE (keep_releases, never previous)
   UNLOCK
 provider writes state: deployed_version, service_status=running, spec_hash, ...
```

### 4.2 In-place update with rollback-on-failure (DESIGN sec 10.2, WSV-03/WSV-04)

Same pipeline as 4.1 but with an existing manifest. On failure at
SWITCH/CONFIGURE/START/HEALTH the engine performs ROLLBACK: `Stop -> junction ->
prev -> Configure(prev) -> Env(prev) -> Start -> Health(prev)`. Result: old
version RUNNING, TF Update returns error so prior state is retained -> consistent
by construction (D11). If rollback ITSELF fails -> ERR_ROLLBACK_FAILED, `.lock`
removed, `last_operation=failed`, diag Detail starts `MACHINE IN UNKNOWN STATE
host=<h>`; next plan repairs (sec 10.6).

### 4.3 Cluster rolling update (cluster_generic_service, CLU-02)

```
engine.deployCluster (cluster.go), passive-first (DESIGN sec 9.5 U-steps):
 U0 LOCK all nodes (all-or-nothing)
 U1 owner := Get-ClusterGroup.OwnerNode ; passives := hosts - owner
 U2 stage+extract new release on each passive (no switch)  [failure -> abort, clean]
 U3 passives: SWITCH + Configure(update) + Env
 U4 MOVE_GROUP -> firstNew passive; verify Online -> ERR_CLUSTER_MOVE (exit 47)
 U5 HEALTH on firstNew
      FAIL -> CLUSTER ROLLBACK: move back to owner, restore passives to prev,
              manifests last_operation=rolled_back, ERR_HEALTH_CHECK
 U6 old owner: stage+extract+SWITCH+Configure+Env (service stopped there)
 U7 optional Move to preferred_owner
 U8 FINALIZE all manifests (current=new), PRUNE per node, UNLOCK reverse
```

Downtime budget: exactly one failover (single probe gap <=30s, CLU-02).

### 4.4 Read / drift reconciliation (DESIGN sec 10.4, DRF-01..03)

```
provider.Read -> engine.ReadStatus:
 CONNECT (coordinator for cluster) -> ReadManifest
   absent            -> resp.State.RemoveResource (next plan = create)
   present           -> set deployed/previous/release_path; service_status=pattern.Status
   last_op==failed   -> WARN diag + deployed_version = current+"!failed" (forces converge)
 network error       -> Read returns error (loud, no silent drop)
```
A manual `Stop-Service` surfaces `service_status running->stopped`; apply then
performs Start ONLY (no FETCH/SWITCH) and converges.

### 4.5 E2E test run (labdeploy_e2e_test, E2E-01/E2E-02)

```
provider.Create -> engine.RunTestRun (testrun.go):
 CONNECT + LOCK-free test dir <install_root>/_tests/<name>/releases/<version>
 FETCH+EXTRACT test package -> run runner (exec|vstest|dotnet_test|npm), timeout
   -> ERR_TIMEOUT kills process tree
 ALWAYS: logs.Collect -> zip _results + globbed logs + event logs -> download
   into results_dir; parse TRX/JUnit counters; write summary.json
 evaluate pass_criteria (exit_codes, min_pass_rate)
   fail & fail_on_test_failure -> ERR_TEST_FAILED **after** collection
```
Collection happens before any test-failure error so pipelines publish via
`always()`. Delete removes the remote test dir best-effort (never fails destroy).

### 4.6 Destroy (DESIGN sec 10.5)

`purge`: Stop -> Uninstall(service/role/container) -> remove `<root>/<app>`.
`unregister`: Stop -> Uninstall, delete manifest, keep releases.
`abandon`: no connection at all; TF state dropped.

---

## 5. Cross-Cutting Concerns

- **Secret hygiene**: env-var NAMES only in spec/state; values via `os.Getenv` at
  runtime, injected as `$env:K='V';` (redacted `***` at TF_LOG=DEBUG). Missing env
  var -> ERR_SPEC_INVALID before any dial (VAL-08).
- **Plan semantics / RequiresReplace**: plan modifier parses old+new spec and
  forces replace on immutable paths (`pattern.type`, `service_name`, `role_name`,
  `install_root`, `metadata.name`, `target.hosts` set inequality, `target.os`).
- **Idempotency**: sec 10.1 short-circuit + Read reconciliation make re-apply an
  empty plan (IDP-01).
- **Observability**: fixed step-name vocabulary + structured tflog; WARN diags
  only for stale-lock override and insecure-transport notices.
- **Distribution**: filesystem-mirror install at
  `registry.local/smartpcr/labdeploy/0.1.0/<os>_<arch>/`; pipelines pass
  `-var version=...` and map secrets to env (DESIGN sec 16).

---

## 6. Test & Environment Contract

The gate host has **NO docker and NO pre-provisioned services**. Each primary
acceptance area below pins a PROOF STRATEGY and, when it needs a backing service,
NAMES it and states how the gate provides it. Downstream `implementation-plan.md`
and `e2e-scenarios.md` MUST honor these pins; a strategy marked `service:*` can
NEVER be realized as an inline unit test.

Legend: `in-process` (pure unit/logic, zero external I/O) | `golden` (committed
snapshot for byte-identical/equality proofs) | `service:<name>` (live backing
service; embedded/ephemeral only if schema needs no non-bundled extension, else a
provided DSN) | `lab` (physical/hardware lab only).

### 6.1 Provider-repo proof pins (run on the gate host)

| # | Acceptance area | Proof strategy | Dependency & how the gate provides it |
|---|---|---|---|
| T1 | Spec parse/validate: YAML==JSON, every sec 6/sec 7 rule, `${var}` subst+escape+unresolved, pattern x os matrix, artifact x pattern (VAL-01..09) | `in-process` | Table-driven `go test ./internal/spec`; zero I/O. |
| T2 | Canonical JSON + `spec_hash` stability across key order | `golden` | Committed canonical-JSON fixture; assert byte-identical hash. Honest choice: hash equality is a preservation proof. |
| T3 | Transport encoding: EncodedCommand exact bytes, chunk math (0/1/48000/48001), single-quote env escaping (`O'Brien`) | `golden` | Committed expected-bytes fixtures; `go test ./internal/transport`. |
| T4 | Local transport round-trip (Exec + Upload/Download) | `service:local-shell` | The gate's own OS shell (`powershell`/`sh`) via `transport: local`, os matches host. No external service, no docker. |
| T5 | Artifact fetch: sha stream verify, 404, header auth injected, nuget flat-container URL | `service:httptest` | Go `net/http/httptest` in-process ephemeral server (bundled stdlib, no extension). NOT inline: needs a live socket. |
| T6 | Pattern script generation: S4 fresh vs update (5 patterns), winsw xml, cluster create/move/rollback scripts | `golden` | Committed `.golden` PowerShell/xml snapshots; `go test ./internal/pattern`. |
| T7 | Engine state machine: every sec 10.2 rollback row, prune-keeps-previous, stale vs fresh lock, idempotent short-circuit | `in-process` | Fake `Transport` with a scripted `Result` queue (engine's `NewTransport` seam); zero I/O. |
| T8 | logs: TRX/JUnit counter parse; glob->zip script | `golden` | Committed TRX/JUnit fixtures + expected counters and script snapshot. |
| T9 | provider schema: spec/spec_file one-of, RequiresReplace on each immutable path, state round-trip | `in-process` | `terraform-plugin-framework` schema/plan unit tests; no provider server, no target. |

### 6.2 Live-lab acceptance (NOT runnable on the gate host)

The DESIGN sec 18 matrix (CON/ART/CAP/WSV/NOD/NET/CLU/RBK/DRF/DST/IDP/LCK/E2E/PIP)
requires real Windows/Linux VMs and a WSFC cluster. These are `lab` and gated
behind `TF_ACC=1` with env-provided targets; they are the release acceptance gate
but MUST NOT be wired into the CI gate host.

| # | Acceptance area | Proof strategy | Dependency & how it is provided |
|---|---|---|---|
| L1 | Windows single-target patterns (WSV/NOD/NET/CAP-win) | `lab` | W1 Windows Server 2022 (WinRM https, node+npm, .NET 8+vstest). Physical/VM lab only. |
| L2 | Linux console + docker (CAP-linux, ART-06, docker_container) | `lab` | L1 Linux VM with docker installed. Gate host has no docker -> lab only. |
| L3 | Failover cluster rolling update/rollback (CLU-01..08) | `lab` | C2 two-node WSFC (`cn1`,`cn2`); C3 three-node for P1. Hardware/VM cluster lab only. |
| L4 | Pipeline integration smoke (PIP-01..03) | `lab` | GH Actions runner + ADO agent against W1/C2. Lab only. |

Rationale for the split: the provider's entire value is remote target mutation,
which cannot be honestly proven in-process. Everything deterministic (parsing,
script generation, state-machine branching, counter parsing) is pinned
`in-process`/`golden` and runs on the gate; everything requiring a live target is
pinned `service:*` (only the bundled `httptest`/`local-shell`, which need no
docker) or `lab`. No acceptance criterion that needs a Windows service, WinRM
endpoint, or cluster is ever claimed as `inline`.

---

## 7. Open Questions

See the JSON block after the Iteration Summary. Primary ambiguity: whether the
gate should attempt any `transport: local` proof (T4) on the gate OS or treat ALL
target interaction as `lab`.
