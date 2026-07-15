# release provider -- E2E Scenarios (QA acceptance)

Gherkin-style feature scenarios for `terraform-provider-labdeploy` (Go,
terraform-plugin-framework, plugin protocol v6). Scenario IDs map 1:1 to the
normative matrix in `.forge-attachments/DESIGN.md` section 18; test names MUST
carry the same ID (e.g. `TestAcc_WSV_03_FailStartRollback`).

## Proof-tier model (why phases are typed the way they are)

This document is anchored to the **Test & Environment Contract** in
`architecture.md` section 6, which is authoritative:

- **The gate host has NO docker and NO pre-provisioned services**
  (`architecture.md:363`). Therefore `compose` (docker-compose) is NOT a usable
  Type for this provider -- there is no docker on the gate. Every phase here is
  either `inline` (gate-host proofs) or `lab-*` (live targets).
- **Gate-host proof tier** (`architecture.md` section 6.1, pins T1..T9):
  deterministic proofs that run in Forge's gate with zero external
  dependencies -- `in-process` (table-driven `go test`), `golden` (committed
  snapshots), `service:httptest` (Go `net/http/httptest` ephemeral, bundled
  stdlib, no docker), and `service:local-shell` (the gate's own `powershell`/`sh`
  via `transport: local`). These are Phases 1-3 below; they self-provision and
  never skip.
- **Live-lab acceptance tier** (`architecture.md` section 6.2, pins L1..L4;
  `implementation-plan.md` Phase 9): the DESIGN section 18 matrix
  (CON/ART/CAP/WSV/NOD/NET/CLU/RBK/DRF/DST/IDP/LCK/E2E/PIP) mutates real remote
  targets and asserts persisted state, so it can only run on lab VMs/clusters
  behind `TF_ACC=1`. These are Phases 4-8; a missing-target skip is allowed here
  (and ONLY here) per the closed-set rule.

Cross-references (do not duplicate): component/`Transport`/`Pattern`/engine
contracts in `architecture.md`; schema field tables + error taxonomy in
`tech-spec.md`; build order (Phases 1-9) in `implementation-plan.md`. Phase
numbers below track that plan: gate proofs are authored alongside their code
(Phases 1-8 of the plan) and the live matrix is the Phase 9 acceptance gate,
here decomposed by target class (Linux, Windows, cluster, pipeline).

## Conventions (apply to every scenario)

- **Secrets are env-var NAMES, never values** (DESIGN section 11, D6). Specs and
  Terraform state carry only the *name* of a runner env var (e.g.
  `password_env: LABDEPLOY_PASSWORD`); the provider resolves the value with
  `os.Getenv` at runtime. No scenario embeds a literal hostname, password, token,
  or key.
- **Target/host indirection.** Every lab target host is supplied through an env
  var (`LD_W1_HOST`, `LD_L1_HOST`, `LD_CN1_HOST`, `LD_CN2_HOST`, ...), so a
  scenario body is identical across developer VMs and lab pools.
- **Lab connection switch.** Each `lab-*` phase reads its target-endpoint env var
  (named in Setup). When it is unset, the acceptance case skips (allowed only for
  `lab-*`); the deterministic slice of that behavior is already proven in the
  gate tier (Phases 1-3), so coverage is never zero.
- **Terminology.** "apply => error CODE" means `terraform apply` exits non-zero
  and stderr contains `[CODE]`. "plan empty" means
  `terraform plan -detailed-exitcode` exits 0. Every scenario ends by asserting
  `.lock` is absent on all touched hosts (DESIGN section 13).
- **Targets are pre-provisioned.** Base OS tooling (WinRM, node/npm, .NET
  runtime, vstest, docker) is installed out of band; the provider never installs
  third-party binaries and lab bootstrap only VERIFIES prerequisites
  (`tech-spec.md:165-170`, DESIGN D2).
- **Test app.** `sample-svc` (tiny .NET worker) packaged as zip + nupkg at
  `1.0.0`, `1.1.0`, `1.2.0-bad` (fail-start). `sample-tests` (vstest) has 3 pass
  tests plus a fail-on-demand test via `FAIL_ONE=1`. Health endpoint returns
  `200` with body `v=<LD_VERSION>`; `--fail-start` exits immediately;
  `--health 500` serves 500.

---

# Phase 1: Spec Validation and Provider Schema (gate proofs)

Gate-host proofs pinned `in-process`/`golden` (`architecture.md` section 6.1 T1,
T2, T9). Covers DESIGN section 18.1 (VAL) plus canonical-hash stability and the
Terraform schema one-of / RequiresReplace rules (DESIGN section 5). No target is
contacted; these run in Forge's gate on every change.

### Setup
- **Type**: inline
- **Local**: `go test ./internal/provider/... ./internal/spec/... -run VAL`
  (table-driven unit + `terraform-plugin-testing` schema cases). No network, no
  VM, no docker.
- **CI runner**: GitHub-hosted `ubuntu-latest`, with a `windows-latest` matrix
  leg for path/regex parity. No labels required; this is the gate host.
- **Secrets**: none.
- **Pre-test bootstrap**: none (Go toolchain from `go.mod`, Terraform CLI from
  `hashicorp/setup-terraform`).
- **Gate provisioning**: dependency-free by construction. Validation
  short-circuits before any dial; VAL-01 proves "no connection attempted" by
  pointing `target.hosts` at `env(LD_UNROUTABLE_HOST)` (default `192.0.2.1`,
  TEST-NET-1) and asserting `ERR_SPEC_INVALID`, never `ERR_CONNECT`. Canonical
  hash uses a committed `internal/spec/testdata` fixture (golden). No embedded
  service, no docker.

### Scenarios

**Feature: Deployment spec schema validation**

  Scenario: VAL-01 Malformed YAML is rejected with a line number
    Given a `labdeploy_deployment` whose `spec` has a tab-broken YAML block
    And `target.hosts = [ env(LD_UNROUTABLE_HOST) ]`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` with the offending line number
    And no connection is attempted (no `ERR_CONNECT`, wall time < 2s).

  Scenario: VAL-02 Missing required artifact.version
    Given a spec without `artifact.version`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` naming the JSON path `artifact.version`.

  Scenario: VAL-03 Pattern/OS matrix violation
    Given `pattern.type = windows_service` and `target.os = linux`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` citing the section 14 support matrix.

  Scenario: VAL-04 Cluster pattern with too few hosts
    Given `pattern.type = cluster_generic_service` and `target.hosts` length 1
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` with `hosts: need 2..16`.

  Scenario: VAL-05 Non-sha256 checksum
    Given `artifact.checksum = md5:...`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` referencing the `sha256:<64 hex>` regex.

  Scenario: VAL-06 Both spec and spec_file set (schema-level)
    Given both `spec` and `spec_file` are set on the resource
    When Terraform validates the configuration
    Then it returns a schema error `exactly one of spec, spec_file`.

  Scenario: VAL-07 Unresolved variable token
    Given `${var:missing}` in the spec and `variables` omits `missing`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` `unresolved variable missing at <path>`.

  Scenario: VAL-08 Referenced secret env var not set
    Given `credentials.password_env = NOT_SET_VAR` and `NOT_SET_VAR` is unset
    When I run `terraform apply`
    Then it errors `[ERR_SPEC_INVALID]` `env var NOT_SET_VAR ... is not set`
    And the error occurs before any dial.

  Scenario: VAL-09 Artifact/pattern mismatch
    Given `artifact.type = docker_image` and `pattern.type = windows_service`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]` (artifact x pattern rule, section 14).

**Feature: Provider schema and canonical hashing (T2, T9)**

  Scenario: VAL-10 Immutable field forces replace
    Given an applied deployment with `pattern.install_root = C:\deploy`
    When I change `install_root` and run `terraform plan`
    Then the plan shows `# forces replacement` on the resource
    And no in-place update is proposed (DESIGN section 5.2 immutable paths).

  Scenario: HSH-01 Canonical JSON hash is key-order stable
    Given two specs identical except for key ordering
    When each is canonicalized and hashed
    Then `spec_hash` is byte-identical (golden fixture compare).

---

# Phase 2: Transport and Artifact Gate Proofs

Gate-host proofs pinned `golden`, `service:local-shell`, and `service:httptest`
(`architecture.md` section 6.1 T3, T4, T5). These prove the deterministic,
target-independent slices of transport and artifact handling -- PowerShell
EncodedCommand bytes, chunking math, env-escaping, local round-trip, sha/404
fetch, NuGet URL, target-pull script, and the in-process host-key-mismatch
mapping (`implementation-plan.md:127` pins host-key mismatch `in-process`). Live
SSH/WinRM round-trips and persisted-state assertions are NOT here -- they are
lab (Phases 4-5), per `implementation-plan.md:126`.

### Setup
- **Type**: inline
- **Local**: `go test ./internal/transport/... ./internal/artifact/...
  -run 'ENC|LCL|ART_FETCH|NUG|TPS|CON_04'`.
- **CI runner**: GitHub-hosted `ubuntu-latest` (plus `windows-latest` for the
  PowerShell EncodedCommand and `transport: local` Windows legs). Gate host.
- **Secrets**: none. The artifact auth-header leg reads a throwaway value from a
  test-scoped env var name; no real credential is involved.
- **Pre-test bootstrap**: none.
- **Gate provisioning**: the artifact fetch cases start a Go
  `net/http/httptest.Server` on `127.0.0.1:0` (bundled stdlib, no docker) with a
  200 route, a 404 route, and a bearer-auth route; when `LD_ARTIFACT_URL` is
  unset this in-process server IS the endpoint. Local round-trip uses the gate's
  own OS shell via `transport: local` (`service:local-shell`), gated on
  `runtime.GOOS` matching the pattern OS. EncodedCommand/chunking/NuGet-URL/
  target-pull-script are compared against committed `.golden` fixtures under
  `internal/transport/testdata` and `internal/artifact/testdata`. Host-key
  mismatch is proven against an in-process stub key -- no live sshd, no
  third-party SSH server dependency.

### Scenarios

**Feature: Transport encoding and local execution (T3, T4)**

  Scenario: ENC-01 PowerShell EncodedCommand is byte-exact
    Given a known script and env map with a quote (`O'Brien`)
    When the Windows transport builds the `-EncodedCommand` payload
    Then the base64(UTF-16LE) bytes match the golden
    And env values are single-quote escaped, never on the command line.

  Scenario: ENC-02 Upload chunk math boundaries
    Given payloads of 0, 1, 48000, and 48001 bytes
    When the WinRM chunked-append upload is planned
    Then the chunk count and offsets match the golden for each size.

  Scenario: LCL-01 Local transport round-trip
    Given `transport = local` with `os` matching the gate host
    When `Exec`, `Upload`, and `Download` run against a temp dir
    Then data round-trips and no network socket is opened.

  Scenario: CON-04 SSH host-key mismatch mapping (in-process)
    Given a pinned wrong `ssh.host_key` and an in-process stub server key
    When `Connect` runs
    Then it errors `[ERR_CONNECT]` with detail `host key mismatch`
    (no live sshd; per `implementation-plan.md:127`).

**Feature: Artifact fetch and pull-script generation (T5)**

  Scenario: ART_FETCH-01 Sha stream verify and 404 (httptest)
    Given the httptest server serving a zip with a correct sha and a 404 route
    When `Fetch` runs against each
    Then the good sha passes and the 404 yields `[ERR_ARTIFACT_FETCH]`
    including `404` and the first 256B of the body.

  Scenario: ART_FETCH-02 Header auth is injected
    Given an http source with `auth.header` and `auth.token_env`
    When `Fetch` runs against the bearer-auth httptest route
    Then the request carries the header value from the env var
    And the value never appears in logs.

  Scenario: NUG-01 NuGet v3 flat-container URL construction
    Given a `nuget_feed` source with `package_id` and `artifact.version`
    When the download URL is built
    Then it matches `<feed>/flatcontainer/<idLower>/<verLower>/<idLower>.<verLower>.nupkg`.

  Scenario: TPS-01 Target-pull script matches golden
    Given an http source with `fetch_mode = target_pull`
    When `TargetPullScript` is generated for windows and linux
    Then each script matches the committed golden (exit 41 on sha mismatch)
    And `LD_AUTH_VALUE` is injected via `Cmd.Env`, never inline.

---

# Phase 3: Pattern, Engine, and Logs Gate Proofs

Gate-host proofs pinned `golden` and `in-process` (`architecture.md` section 6.1
T6, T7, T8). Proves pattern script generation (all 5 patterns + winsw + cluster),
the engine state machine over a fake `Transport` (every DESIGN section 10.2
rollback row, prune-keeps-previous, stale-vs-fresh lock, idempotent
short-circuit), and TRX/JUnit counter parsing. Zero external I/O; this is where
the deterministic core of the live matrix (WSV/CLU/RBK/DRF/IDP/LCK/E2E) is
proven so the gate never depends on a lab.

### Setup
- **Type**: inline
- **Local**: `go test ./internal/pattern/... ./internal/engine/...
  ./internal/logs/... -run 'PAT|ENG|LOG'`.
- **CI runner**: GitHub-hosted `ubuntu-latest`. Gate host.
- **Secrets**: none.
- **Pre-test bootstrap**: none.
- **Gate provisioning**: the engine tests inject a fake `Transport` with a
  scripted `Result` queue through the engine's `NewTransport` seam (no sockets);
  pattern and logs tests compare generated PowerShell/xml and parsed counters
  against committed `.golden`/fixture files under `internal/*/testdata`. No
  docker, no service, no target.

### Scenarios

**Feature: Pattern script generation (T6)**

  Scenario: PAT-01 Fresh vs update S4 scripts for all five patterns
    Given each of console_app, windows_service, node_web_app, dotnet_api,
    cluster_generic_service
    When the S4 switchover script is generated for fresh and update
    Then each matches its committed golden.

  Scenario: PAT-02 WinSW xml and cluster scripts
    Given a winsw-wrapped service and a cluster role
    When the winsw xml and the cluster create/move/rollback scripts are generated
    Then each matches its golden.

**Feature: Engine state machine over fake transport (T7)**

  Scenario: ENG-01 Rollback matrix rows
    Given a fake transport scripted to fail at each DESIGN section 10.2 step
    When Deploy runs
    Then the resulting machine/state matches the matrix row (fresh vs update)
    And the `.lock` is released on every error path.

  Scenario: ENG-02 Prune keeps previous and honors keep_releases
    Given `keep_releases = 2` and a scripted successful finalize
    When prune runs
    Then the oldest is removed and `previous_version` is never deleted.

  Scenario: ENG-03 Lock stale vs fresh and idempotent short-circuit
    Given a scripted existing `.lock` (fresh, then aged beyond timeout) and a
    manifest whose version+checksum already match
    When Deploy runs
    Then fresh lock yields `[ERR_LOCKED]`, aged lock is overridden with a WARN,
    and the matching manifest short-circuits to a no-op (DESIGN section 10.1).

**Feature: Result parsing (T8)**

  Scenario: LOG-01 TRX and JUnit counter parse
    Given committed TRX and JUnit fixtures (single and multiple files)
    When counters are parsed
    Then total/passed/failed/skipped match the expected sums
    And `results.format: none` yields counters of `-1`.

---

# Phase 4: Linux and Docker Single-Target Acceptance (lab)

Live acceptance pinned `lab` (`architecture.md` section 6.2 L2;
`implementation-plan.md:476,483`). Covers the Linux console path (DESIGN 18.4
CAP-linux), Linux drift/idempotency/lock lifecycle (DESIGN 18.8 subset), and the
docker patterns (DESIGN 18.3 ART-06 + docker_container). The gate host has no
docker, so these run only on a Linux lab VM.

### Setup
- **Type**: lab-bare-metal
- **Local**: point `LD_L1_HOST` at a personal Linux VM (sshd + docker), set
  `LABDEPLOY_PASSWORD` (or `LD_SSH_KEY`), then
  `go test ./test/e2e/linux/... -run 'CAP_03|IDP|LCK|DRF_02|DST_02|ART_06|DKR'
  -tags acc` with `TF_ACC=1`.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, linux, forge-lab-hw]`. L1 Linux VM (Ubuntu 22.04) with sshd and
  docker preinstalled.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-hw-vg`, mapped to `env: LABDEPLOY_PASSWORD`)
  **and** GitHub environment `lab-linux` secret `LAB_PASSWORD` (mapped to
  `env: LABDEPLOY_PASSWORD`). Target host and registry come from KeyVault entries
  `labdeploy-l1-host`/`labdeploy-registry-password` and GitHub environment
  variables `LD_L1_HOST`/`LD_REGISTRY`.
- **Pre-test bootstrap**: `bash tests/e2e/linux/verify-l1.sh` -- VERIFIES (does
  not install) `sshd`, `docker version`, and free disk; fails fast if a
  prerequisite is missing (targets are pre-provisioned, `tech-spec.md:165-170`).
- **Gate provisioning**: n/a (lab). These scenarios skip when `LD_L1_HOST` is
  unset (allowed for `lab-*`); the deterministic slices (fetch, prune, lock,
  idempotent short-circuit, docker script generation) are proven in Phases 2-3
  so the gate is never empty.

### Scenarios

**Feature: console_app on Linux and generic lifecycle**

  Scenario: CAP-03 Linux console over ssh uses a symlink
    Given a fresh apply of `sample-svc` v1.0.0 over `ssh` on linux
    When I apply
    Then apply succeeds and `current` is a symlink verified via `readlink`
    And outputs `deployed_version = 1.0.0`, `service_status = n/a`.

  Scenario: IDP-01 Re-apply of identical spec is a no-op
    Given an applied deployment (app running)
    When I re-apply the identical spec
    Then plan is empty, `-refresh-only` shows no changes, and mtimes are unchanged.

  Scenario: DRF-02 Deleted manifest forces clean recreate
    Given I delete `manifest.json` on the target
    When I run `terraform plan`
    Then the plan is CREATE and apply reinstalls cleanly over the leftovers.

  Scenario: LCK-01 Second concurrent apply fails fast
    Given apply A is mid-flight holding `.lock` (slow-artifact route)
    When apply B runs in parallel against the same target
    Then B fails in < 5s with `[ERR_LOCKED]` naming A's owner; A completes.

  Scenario: LCK-02 Stale lock is overridden
    Given a planted `.lock` aged beyond `lock_timeout_seconds`
    When I apply
    Then apply succeeds with a WARN `stale lock ... overridden`.

  Scenario: DST-02 Destroy mode abandon makes no connection
    Given `destroy_mode = abandon` and an unroutable `LD_L1_HOST`
    When I run `terraform destroy`
    Then destroy succeeds without contacting the target and state is empty.

**Feature: docker_container and docker_registry**

  Scenario: ART-06 docker_registry bad credentials
    Given a `docker_registry` source whose `password_env` value is wrong
    When I apply
    Then it errors `[ERR_ARTIFACT_FETCH]` containing a `docker login` stderr
    snippet and the container list is unchanged.

  Scenario: DKR-01 Fresh container deploy by tag
    Given a `docker_container` spec for `sample-svc` with `ports` and a
    `restart_policy`
    When I apply
    Then the container runs, `GET /health` returns 200
    And `manifest.json` records the running image reference.

  Scenario: DKR-02 Upgrade replaces the container by digest
    Given a running container at v1.0.0
    When I apply v1.1.0 pinned by `digest`
    Then the old container is replaced, `/health` serves `v=1.1.0`, digest verified.

  Scenario: DKR-03 Destroy purge removes the container
    Given a purge-mode destroy
    When I run `terraform destroy`
    Then the container is absent from `docker ps -a` and state is empty.

---

# Phase 5: Windows Single-Target Acceptance (lab)

Live acceptance pinned `lab` (`architecture.md` section 6.2 L1;
`implementation-plan.md:466,475`). Covers WinRM connectivity (DESIGN 18.2),
persisted-state artifacts (18.3), Windows console (18.4), all service patterns
(18.5 WSV, 18.6 NOD/NET), and Windows service drift / downgrade / destroy (18.8
RBK/DRF/DST). Every scenario mutates a real Windows Server; nothing here is
gate-provable, so its deterministic core lives in Phases 1-3.

### Setup
- **Type**: lab-bare-metal
- **Local**: point `LD_W1_HOST` at a personal Windows Server 2022 VM (WinRM HTTPS
  enabled), set `LABDEPLOY_PASSWORD`, then
  `go test ./test/e2e/windows/... -run
  'CON|ART_0[1-5]|CAP_0[12]|WSV|NOD|NET|RBK|DRF_0[13]|DST_01' -tags acc` with
  `TF_ACC=1`.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, windows, forge-lab-hw]`. W1 Windows Server 2022 with PowerShell
  5.1, WinRM HTTPS, node 20+npm, .NET 8 runtime + `vstest.console.exe`
  (all pre-provisioned).
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-hw-vg`, mapped to `env: LABDEPLOY_PASSWORD`)
  **and** GitHub environment `lab-windows` secret `LAB_PASSWORD` (mapped to
  `env: LABDEPLOY_PASSWORD`). Target host from KeyVault entry `labdeploy-w1-host`
  / GitHub environment variable `LD_W1_HOST`.
- **Pre-test bootstrap**: `pwsh tests/e2e/windows/verify-w1.ps1` -- VERIFIES (does
  not install) `$PSVersionTable.PSVersion.Major >= 5`, WinRM HTTPS listener,
  `node`/`npm`/`dotnet --list-runtimes` (`Microsoft.AspNetCore.App`), and
  `vstest.console.exe` on PATH; fails fast on a missing prerequisite (targets are
  pre-provisioned, `tech-spec.md:165-170`). It also removes leftover
  `sample-svc` services/releases from a prior run.
- **Gate provisioning**: n/a (lab). Scenarios skip when `LD_W1_HOST` is unset
  (allowed for `lab-*`); the deterministic slices (transport encoding, pattern
  script generation, engine rollback matrix, counter parse) are proven in
  Phases 1-3.

### Scenarios

**Feature: WinRM connectivity and artifact persistence**

  Scenario: CON-01 WinRM HTTPS with self-signed cert
    Given a Windows target over `winrm` HTTPS with `insecure_skip_verify = true`
    When I apply a minimal `console_app` spec
    Then apply succeeds and exactly one WARN diag about insecure TLS is emitted.

  Scenario: CON-02 Wrong password is not retried
    Given credentials whose `password_env` resolves to a wrong value
    When I apply
    Then it errors `[ERR_AUTH]`, exactly one auth attempt, wall time < 15s.

  Scenario: CON-03 Firewall-dropped port exhausts retries
    Given `connect_retries = 2` against a dropped port
    When I apply
    Then it errors `[ERR_CONNECT]` and the log shows `attempts=3`.

  Scenario: ART-01 HTTP zip with correct sha, runner_push
    Given an http zip with matching checksum and `fetch_mode = runner_push`
    When I apply
    Then `releases/<ver>/.labdeploy-release.json` records a matching sha.

  Scenario: ART-02 Checksum mismatch leaves target untouched
    Given `artifact.checksum` off by one hex digit
    When I apply
    Then it errors `[ERR_CHECKSUM_MISMATCH]`, no `releases/<ver>` dir, no manifest
    change, staging empty, any pre-existing service still Running.

  Scenario: ART-03 HTTP 404
    Given an http url returning 404
    When I apply
    Then it errors `[ERR_ARTIFACT_FETCH]` including `404`.

  Scenario: ART-04 nupkg is unzipped in place
    Given a `nupkg` source for `sample-svc`
    When I apply
    Then files land under `releases/<ver>/lib/...` and the app runs.

  Scenario: ART-05 target_pull with auth header does not proxy payload
    Given `fetch_mode = target_pull` with an auth header env var
    When I apply (optionally with runner egress to the package host blocked)
    Then apply succeeds and runner counters confirm the payload was not proxied.

**Feature: windows console and service lifecycle**

  Scenario: CAP-01 Fresh Windows console apply with verify_command
    Given a fresh apply v1.0.0 with `verify_command = sample-svc.exe --version`
    When I apply
    Then the `current` junction resolves to `releases\1.0.0`
    And outputs `deployed_version = 1.0.0`, `service_status = n/a`.

  Scenario: CAP-02 Prune honors keep_releases and never previous
    Given `keep_releases = 2`
    When I roll v1.1.0 -> v1.0.0 -> v1.1.0 three times
    Then exactly two dirs remain under `releases` and the pruned dir is gone.

  Scenario: WSV-01 Fresh install with health check and env delivery
    Given a fresh apply v1.0.0 with an http `/health` check
    When I apply
    Then `sc query` is RUNNING, `start_type = AUTO_START`, recovery actions set
    And HKLM Environment has `LD_VERSION=1.0.0` and spec env
    And health passes and `app.log` proves env delivery.

  Scenario: WSV-02 In-place upgrade keeps downtime bounded
    Given v1.0.0 running
    When I apply v1.1.0
    Then `/health` body is `v=1.1.0`, downtime <= `stop_timeout + 15s`
    And `releases` holds 1.0.0 and 1.1.0, output `previous_version = 1.0.0`.

  Scenario: WSV-03 Failed start rolls back to previous version
    Given v1.1.0 running
    When I apply v1.2.0-bad (fail-start)
    Then apply errors `[ERR_SERVICE_START]` with an event-log 7000/7009 line
    And the service is RUNNING on v1.1.0 (`/health` `v=1.1.0`), junction -> 1.1.0
    And TF `deployed_version = 1.1.0` and the next plan is empty.

  Scenario: WSV-04 Failing health check rolls back
    Given v1.0.0 running and `rollback_on_failure = true`
    When I apply v1.1.0 built with `--health 500`
    Then apply errors `[ERR_HEALTH_CHECK]` and the prior version is restored healthy.

  Scenario: WSV-05 Rollback disabled leaves drift
    Given the WSV-03 setup but `rollback_on_failure = false`
    When I apply v1.2.0-bad
    Then apply errors, service stopped/crashed on 1.2.0-bad,
    `manifest.last_operation.result = failed`, next plan non-empty, a follow-up
    apply of 1.1.0 repairs it.

  Scenario: WSV-06 WinSW wrapper for a console build
    Given `wrapper = winsw` wrapping the console build
    When I apply fresh then upgrade
    Then both succeed and the WinSW xml is regenerated on upgrade (slow-stop honored).

  Scenario: WSV-07 Stuck service is force-killed within budget
    Given a slow-stop build and `stop_timeout_seconds = 10`
    When I deploy
    Then deploy succeeds, logs contain `FORCE_KILL`, total STOP phase < 25s.

  Scenario: WSV-08 Non-builtin account, secret never logged
    Given account `.\svcuser` with `password_env`
    When I apply
    Then `ObjectName = .\svcuser` and the secret is absent from any TRACE `sc qc`.

**Feature: node_web_app and dotnet_api**

  Scenario: NOD-01 Bundled node modules
    Given a node app with bundled `node_modules`, `install_deps = false`, port
    from `LD_NODE_PORT`
    When I apply
    Then the service runs, `GET /health` returns 200, env `PORT` in `app.log`.

  Scenario: NOD-02 install_deps without a lockfile
    Given `install_deps = true` and no `package-lock.json`
    When I apply
    Then it errors `[ERR_SERVICE_INSTALL]` mentioning the lockfile, no service created.

  Scenario: NOD-03 install_deps with a lockfile
    Given `install_deps = true` with a lockfile
    When I apply
    Then `node_modules` exists in the release dir and the app is healthy.

  Scenario: NET-01 dotnet self-contained, native hosting
    Given a self-contained dotnet app, native hosting, urls from `LD_ASPNET_URLS`
    When I apply
    Then it runs, health passes, `ASPNETCORE_URLS` present in HKLM env.

  Scenario: NET-02 dotnet_dll without the ASP.NET runtime
    Given `launcher = dotnet_dll` on a box lacking `Microsoft.AspNetCore.App`
    When I apply
    Then it errors `[ERR_PREFLIGHT]` naming `Microsoft.AspNetCore.App` before any
    file is copied.

**Feature: service drift, downgrade, and destroy**

  Scenario: RBK-01 Explicit downgrade reuses the release cache
    Given the post-WSV-02 state (1.1.0 running, 1.0.0 cached)
    When I apply `-var version=1.0.0` with egress to the artifact host blocked
    Then apply succeeds with FETCH `cache_hit=true`, `/health` `v=1.0.0`,
    `previous_version = 1.1.0`.

  Scenario: RBK-02 Downgrade re-fetches when cache pruned
    Given `keep_releases = 1` so 1.0.0 was pruned
    When I apply 1.0.0
    Then apply succeeds WITH a re-fetch (`cache_hit=false`).

  Scenario: DRF-01 Manual Stop-Service drives a Start-only converge
    Given a running service that I stop manually
    When I run `terraform plan`
    Then the plan shows `service_status running -> stopped`
    And apply performs Start ONLY (no FETCH/SWITCH) and converges.

  Scenario: DRF-03 Manually repointed junction converges without fetch
    Given state says 1.1.0 but I repoint the junction to 1.0.0
    When I run `terraform plan`
    Then the plan shows `deployed_version` drift and apply converges to 1.1.0
    without an artifact fetch.

  Scenario: DST-01 Destroy purge removes service, files, and state
    Given a purge-mode destroy of a WSV deployment
    When I run `terraform destroy`
    Then `sc query` returns 1060 (absent), the dir is gone, state is empty.

---

# Phase 6: Failover Cluster Acceptance (lab)

Live acceptance pinned `lab` (`architecture.md` section 6.2 L3;
`implementation-plan.md:481`). Covers DESIGN section 18.7 (CLU-01..08):
`cluster_generic_service` on a real WSFC -- rolling update with a single
`Move-ClusterGroup`, rollback-on-failure back to the original owner, mid-test
node isolation, role/service conflict, and `preferred_owner` placement.

### Setup
- **Type**: lab-wsfc
- **Local**: point `LD_CN1_HOST`/`LD_CN2_HOST` at a personal two-node WSFC (no
  CSV), set `LABDEPLOY_PASSWORD`, then
  `go test ./test/e2e/cluster/... -run CLU -tags acc` with `TF_ACC=1`.
- **CI runner**: ADO agent pool `forge-lab-wsfc`; GitHub self-hosted runner labels
  `[self-hosted, windows, wsfc, forge-lab]`. Runner sits outside the cluster and
  reaches both nodes over WinRM HTTPS; C3 (CLU-03) additionally needs a runner
  labeled `wsfc-3node`.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-wsfc-vg`, mapped to `env: LABDEPLOY_PASSWORD`)
  **and** GitHub environment `lab-wsfc` secret `LAB_PASSWORD` (mapped to
  `env: LABDEPLOY_PASSWORD`). Node hostnames from KeyVault entries
  `labdeploy-cn1-host`/`labdeploy-cn2-host` and GitHub environment variables
  `LD_CN1_HOST`/`LD_CN2_HOST`.
- **Pre-test bootstrap**: `pwsh tests/e2e/cluster/verify-wsfc.ps1` -- VERIFIES
  (does not install) the `FailoverClusters` module, that the cluster and both
  nodes are Up, and removes any leftover `sample-role`/`sample-svc`; fails fast
  if the cluster is not pre-provisioned.
- **Gate provisioning**: n/a (lab). CLU scenarios skip when `LD_CN1_HOST` /
  `LD_CN2_HOST` are unset (allowed for `lab-*`); the cluster state machine
  (hosts-order update, single MOVE_GROUP, rollback to original owner) is proven
  in Phase 3 (ENG/PAT golden + fake transport).

### Scenarios

**Feature: cluster_generic_service rollout and rollback**

  Scenario: CLU-01 Fresh cluster role install
    Given a fresh apply v1.0.0 for role `sample-role`
    When I apply
    Then the service exists on cn1 and cn2 (`start_type = DEMAND`)
    And `Get-ClusterGroup sample-role` is Online, owner in {cn1, cn2}
    And health on the owner passes, both manifests `current = 1.0.0`,
    TF `service_status = online`.

  Scenario: CLU-02 Rolling update keeps a single short outage
    Given v1.0.0 Online and a 1 Hz `/health` probe loop for 120s
    When I apply v1.1.0
    Then the probe sees at most ONE drain-induced gap <= 30s
    And the final owner is the pre-update PASSIVE node, both junctions -> 1.1.0.

  Scenario: CLU-03 Three-node update order (C3)
    Given a three-node cluster on v1.0.0
    When I apply v1.1.0
    Then passives update in hosts order (log order asserts), the former owner
    updates last, exactly one MOVE_GROUP (plus optional preferred move).

  Scenario: CLU-04 Failed health rolls the role back to the original owner
    Given v1.0.0 Online
    When I apply v1.1.0 built with `--health 500`
    Then apply errors `[ERR_HEALTH_CHECK]`, role Online on the ORIGINAL owner
    serving `v=1.0.0`, both junctions -> 1.0.0, manifests `last_operation =
    rolled_back`, next plan shows a pending change to 1.1.0.

  Scenario: CLU-05 Node isolation mid-apply causes no partial switch
    Given WinRM to cn2 blocked before apply
    When I apply v1.1.0
    Then it errors `[ERR_CONNECT]` `host=cn2` during CONNECT/U0 and cn1 is
    untouched (junction and role owner unchanged).

  Scenario: CLU-06 Role bound to a foreign service is rejected
    Given a pre-created `sample-role` bound to `other-svc`
    When I apply
    Then it errors `[ERR_SERVICE_INSTALL]` naming both `sample-role` and
    `other-svc` and nothing is modified.

  Scenario: CLU-07 Destroy purge removes the role and services on all nodes
    Given a purge-mode destroy
    When I run `terraform destroy`
    Then the role is absent, services deleted on both nodes, `<root>\sample-svc`
    gone on both nodes.

  Scenario: CLU-08 preferred_owner placement
    Given `preferred_owner = cn2`
    When I apply fresh then update
    Then `OwnerNode == cn2` after each apply.

---

# Phase 7: E2E Test Resource Acceptance (lab)

Live acceptance pinned `lab` (`architecture.md` section 6.2 L1;
`implementation-plan.md:482`). Covers DESIGN section 18.9 (E2E-01..07) for the
`labdeploy_e2e_test` resource against W1: run `sample-tests` on the target, parse
TRX counters, enforce `pass_criteria` and `fail_on_test_failure`, honor
`triggers` replace, kill a runaway tree on timeout, filter Windows event logs,
and ALWAYS collect before failing. TRX/JUnit parsing itself is already proven
in-process (Phase 3, LOG-01); these scenarios add the live-target execution.

### Setup
- **Type**: lab-bare-metal
- **Local**: point `LD_W1_HOST` at a Windows Server 2022 VM with
  `vstest.console.exe`, set `LABDEPLOY_PASSWORD`, then
  `go test ./test/e2e/testrun/... -run E2E -tags acc` with `TF_ACC=1`.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, windows, forge-lab-hw]`. Same W1 class as Phase 5.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-hw-vg`, mapped to `env: LABDEPLOY_PASSWORD`)
  **and** GitHub environment `lab-windows` secret `LAB_PASSWORD` (mapped to
  `env: LABDEPLOY_PASSWORD`). Target host from KeyVault `labdeploy-w1-host` /
  GitHub environment variable `LD_W1_HOST`.
- **Pre-test bootstrap**: reuse `pwsh tests/e2e/windows/verify-w1.ps1` (VERIFIES
  `vstest.console.exe` and .NET; installs nothing).
- **Gate provisioning**: n/a (lab). E2E scenarios skip when `LD_W1_HOST` is unset
  (allowed for `lab-*`); counter math (E2E-07 shape) is proven in Phase 3
  (LOG-01) on committed fixtures.

### Scenarios

**Feature: labdeploy_e2e_test execution and collection**

  Scenario: E2E-01 All tests pass and results are collected
    Given a deployed 1.1.0 and a vstest TestRun (all pass), `triggers.run = 1`
    When I apply
    Then outputs `total = 3`, `passed_tests = 3`, `failed_tests = 0`,
    `passed = true`, `results_dir/results/*.trx` exists, `summary.json` matches,
    and the logs dir contains `app.log` from the shared dir.

  Scenario: E2E-02 Test failure still collects, leaves no test state
    Given `FAIL_ONE=1` and `fail_on_test_failure = true`
    When I apply
    Then it errors `[ERR_TEST_FAILED]`, `results_dir` is fully populated anyway,
    no TF state exists for the test resource, deployment resource unaffected.

  Scenario: E2E-03 Reporting mode surfaces failure without failing apply
    Given `FAIL_ONE=1` and `fail_on_test_failure = false`
    When I apply
    Then apply succeeds with `passed = false`, `failed_tests = 1`.

  Scenario: E2E-04 Timeout kills the test process tree
    Given `runner.timeout_seconds = 10` against a sleep-forever test
    When I apply
    Then it errors `[ERR_TIMEOUT]`, the test process tree on the target is dead,
    partial logs are collected.

  Scenario: E2E-05 Event-log collection is time-filtered
    Given `collect.windows_event_logs` with a provider filter
    When I apply
    Then the events json contains ONLY events at/after test start.

  Scenario: E2E-06 Bumping triggers.run re-runs the tests
    Given a passing TestRun with `triggers.run = 1`
    When I change `triggers.run` to 2 and plan
    Then the plan is a replace and the tests re-run.

  Scenario: E2E-07 Exit-code-only runner with results.format none
    Given `results.format = none` and an `exec` runner
    When I apply
    Then `passed` reflects `pass_criteria.exit_codes` and counters are `-1`.

---

# Phase 8: Pipeline Integration Acceptance (lab)

Live acceptance pinned `lab` (`architecture.md` section 6.2 L4;
`implementation-plan.md:484`). Covers DESIGN section 18.10 (PIP-01..03): the
shipped reference pipelines in `examples/pipelines/` driving the provider end to
end via GitHub Actions and Azure DevOps, including `always()` artifact publishing
and the auto-rollback job/stage, against the W1 lab and filesystem-mirror
provider distribution (DESIGN section 16.1).

### Setup
- **Type**: lab-bare-metal
- **Local**: not typically run locally; a developer can
  `gh workflow run github-deploy.yml -f version=1.1.0` against a personal W1 and
  self-hosted runner, or queue the ADO pipeline manually.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, windows, forge-lab-hw]` (same W1 class as Phase 5). The runner
  lays the provider into its filesystem mirror per DESIGN section 16.1 before the
  workflow runs.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-hw-vg`, mapped to
  `env: LABDEPLOY_PASSWORD: $(LabPassword)`) **and** GitHub environment
  `lab-windows` secret `LAB_PASSWORD` (mapped to `env: LABDEPLOY_PASSWORD`).
  Target host from KeyVault `labdeploy-w1-host` / GitHub environment variable
  `LD_W1_HOST`.
- **Pre-test bootstrap**: `pwsh tests/e2e/pipeline/install-provider.ps1` -- builds
  or downloads `terraform-provider-labdeploy_v<version>` and lays it into the
  `%APPDATA%\terraform.d\plugins\registry.local\smartpcr\labdeploy\...` mirror
  with the `~/.terraformrc` filesystem_mirror block; then reuses `verify-w1.ps1`.
  This provisions the PROVIDER binary only; the target OS tooling is
  pre-provisioned.
- **Gate provisioning**: n/a (lab). PIP scenarios skip when `LD_W1_HOST` / the
  self-hosted runner are unavailable (allowed for `lab-*`); provider binary
  behavior and spec substitution are proven in Phases 1-3.

### Scenarios

**Feature: reference pipeline integration**

  Scenario: PIP-01 GitHub deploy workflow publishes results
    Given `github-deploy.yml` dispatched with `version=1.1.0`
    When the workflow runs
    Then it is green, the uploaded `e2e-results` artifact contains
    trx + summary + logs, and the job summary echoes the `summary` output.

  Scenario: PIP-02 GitHub deploy failure triggers auto-rollback
    Given dispatch with `version=1.2.0-bad`, `auto_rollback=true`,
    `rollback_to=1.1.0`
    When the workflow runs
    Then the deploy job fails with `[ERR_SERVICE_START]`, the rollback job runs
    and ends green, and the final target serves `v=1.1.0`.

  Scenario: PIP-03 Azure DevOps pass case publishes to the Tests tab
    Given a run of `azure-pipelines.yml` (pass case)
    When the pipeline runs
    Then the Tests tab shows 3 VSTest results and the pipeline artifact
    `labdeploy-logs` is present.

---

## Traceability

Every scenario ID is drawn from DESIGN section 18 and MUST appear verbatim in the
corresponding test name (`go test` unit/golden for the gate tier;
`terraform-plugin-testing` / terratest for the lab tier), satisfying the
"Scenario IDs MUST appear in test names 1:1" requirement. Each row cites its
`architecture.md` section 6 proof pin so phase Type never contradicts the
authoritative contract.

| Phase | Tier | Scenario IDs | Type | Proof pin |
|---|---|---|---|---|
| 1 Spec/Schema | gate | VAL-01..10, HSH-01 | inline | T1, T2, T9 |
| 2 Transport/Artifact proofs | gate | ENC-01..02, LCL-01, CON-04, ART_FETCH-01..02, NUG-01, TPS-01 | inline | T3, T4, T5 |
| 3 Pattern/Engine/Logs proofs | gate | PAT-01..02, ENG-01..03, LOG-01 | inline | T6, T7, T8 |
| 4 Linux + Docker | lab | CAP-03, IDP-01, DRF-02, LCK-01..02, DST-02, ART-06, DKR-01..03 | lab-bare-metal | L2 |
| 5 Windows single-target | lab | CON-01..03, ART-01..05, CAP-01..02, WSV-01..08, NOD-01..03, NET-01..02, RBK-01..02, DRF-01/03, DST-01 | lab-bare-metal | L1 |
| 6 Failover cluster | lab | CLU-01..08 | lab-wsfc | L3 |
| 7 E2E test resource | lab | E2E-01..07 | lab-bare-metal | L1 |
| 8 Pipeline integration | lab | PIP-01..03 | lab-bare-metal | L4 |

Notes:
- CON-04 (host-key mismatch) and CON-05 (local, proven as LCL-01) are gate-tier
  `in-process`/`service:local-shell` per `implementation-plan.md:127` and
  `architecture.md` T4; the live WinRM legs CON-01..03 are lab-tier (Phase 5).
- ART fetch/404/header (ART_FETCH-01..02, `service:httptest`, T5) are gate-tier;
  the persisted-state legs ART-01..05 and the docker leg ART-06 are lab-tier
  (Phases 4-5). ART-06 appears exactly once (Phase 4).
- No phase uses `compose`: the gate host has no docker (`architecture.md:363`),
  so docker-backed acceptance (docker_container, ART-06) is lab-only (Phase 4).
