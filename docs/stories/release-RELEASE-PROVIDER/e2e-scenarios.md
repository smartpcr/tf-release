# release provider -- E2E Scenarios (QA acceptance)

Gherkin-style feature scenarios for `terraform-provider-labdeploy` (Go,
terraform-plugin-framework, plugin protocol v6).

**Two ID namespaces (do not conflate):**
- **Normative acceptance IDs** are drawn verbatim from the DESIGN section 18
  matrix (`VAL-01..09`, `CON-`, `ART-`, `CAP-`, `WSV-`, `NOD-`, `NET-`, `CLU-`,
  `RBK-`, `DRF-`, `DST-`, `IDP-`, `LCK-`, `E2E-`, `PIP-`, plus the P1 `DKR-` docker
  cases). The normative `VAL-` range is `VAL-01..09` (spec parse/validate, T1 per
  `architecture.md:378`); nothing above `VAL-09` is a DESIGN section 18 ID. Their
  test names MUST carry the same ID (e.g. `TestAcc_WSV_03_FailStartRollback`), per
  `tech-spec.md:294`.
- **Supplemental gate-proof IDs** (`HSH-`, `RRP-`, `SRT-`, `ENC-`, `LCL-`,
  `ART_FETCH-`, `NUG-`, `TPS-`, `PAT-`, `ENG-`, `LOG-`, `PIP_LINT-`) are NOT DESIGN
  section 18 IDs. They name the deterministic gate-host proofs that implement
  `architecture.md` section 6.1 pins T1-T9 -- in particular `RRP-`/`SRT-` cover T9
  (RequiresReplace on each immutable path + schema state round-trip), and
  `PIP_LINT-` is the structural pipeline-YAML proof (`implementation-plan.md:438`),
  which the DESIGN section 18 matrix does not enumerate. They exist so the gate has
  coverage without a lab; they are marked "supplemental (T#)" wherever they appear
  and are listed separately in the Traceability table.

## Proof-tier model (why phases are typed the way they are)

This document is anchored to the **Test & Environment Contract** in
`architecture.md` section 6, which is authoritative:

- **Gate-host tier types.** Pure logic proofs with zero sockets
  (`in-process` table-driven `go test`, `golden` committed snapshots) are
  `inline` (Phases 1 and 3). Proofs that need a live socket -- `service:httptest`
  (Go `net/http/httptest` ephemeral) and `service:local-shell` (the gate's own
  `powershell`/`sh` via `transport: local`) -- are `compose` (Phase 2):
  `architecture.md:366-383` states a `service:*` pin can NEVER be realized as an
  inline unit test and calls T5 a live-socket proof. The `compose` env-var-set
  path is the CI service stack; when the connection env-var is unset the suite
  self-provisions the SAME dependency IN-PROCESS (httptest server, local shell,
  in-process stub key -- no docker), so Phase 2 still runs in Forge's gate and
  never skips.
- **Gate-host proof tier** (`architecture.md` section 6.1, pins T1..T9):
  deterministic proofs that run in Forge's gate with zero pre-provisioned
  services -- `in-process`, `golden`, `service:httptest`, and
  `service:local-shell`. These are Phases 1-3 below; they self-provision and
  never skip.
- **No lab-only docker on the gate.** Docker-backed acceptance
  (docker_container, ART-06) needs a real container runtime, which the gate host
  lacks (`architecture.md:363`), so it is lab-only (Phase 4), never `compose`.
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

**Feature: Provider schema, RequiresReplace, and canonical hashing (T2, T9)**

  Scenario: RRP-01 Every immutable path forces replace (table-driven, T9)
    Given an applied deployment
    When I change, one at a time, each immutable path -- `pattern.type`,
    `pattern.*.service_name`, `pattern.*.role_name`, `pattern.install_root`,
    `metadata.name`, `target.hosts` (set inequality), `target.os`
    Then each change's `terraform plan` shows `# forces replacement` on the
    resource and proposes no in-place update (DESIGN section 5.2 immutable set;
    `implementation-plan.md:311`).

  Scenario: RRP-02 Mutable spec change is an in-place update (T9)
    Given an applied deployment
    When I change a mutable field (e.g. `artifact.version` / `environment`)
    Then the plan is an in-place Update (no replacement), confirming the plan
    modifier only trips on the immutable set.

  Scenario: SRT-01 Provider state round-trip (T9)
    Given a `labdeploy_deployment` (and a `labdeploy_e2e_test`) config
    When the framework marshals the schema to state and back via
    `terraform-plugin-framework` state round-trip (no provider server, no target)
    Then every attribute (spec/spec_file one-of, computed `id`, `deployed_version`,
    `hosts`, `spec_hash`, ...) survives unchanged and no `Unknown`/`Null`
    mismatch is raised (`architecture.md:386` T9; `implementation-plan.md:293`).

  Scenario: HSH-01 Canonical JSON hash is key-order stable (T2)
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
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/transport-artifact/docker-compose.yml
  up -d --wait` then `go test ./internal/transport/... ./internal/artifact/...
  -run 'ENC|LCL|ART_FETCH|NUG|TPS|CON_04'`. Set `LD_ARTIFACT_URL` to the compose
  artifact service; leave it unset to use the in-process httptest fallback.
- **CI runner**: GitHub-hosted `ubuntu-latest` (plus `windows-latest` for the
  PowerShell EncodedCommand and `transport: local` Windows legs). Gate host.
- **Secrets**: none. The artifact auth-header leg reads a throwaway value from a
  test-scoped env var name; the compose artifact host uses non-secret container
  credentials from the compose `.env`; no real credential is involved.
- **Pre-test bootstrap**:
  `docker compose -f tests/e2e/transport-artifact/docker-compose.yml up -d --wait`
  (only for the env-var-set path; the gate/unset path needs no bootstrap).
- **Gate provisioning**: when `LD_ARTIFACT_URL` is unset the artifact fetch cases
  start a Go `net/http/httptest.Server` on `127.0.0.1:0` (bundled stdlib, no
  docker) with a 200 route, a 404 route, and a bearer-auth route; when set, they
  target the compose `artifacts` service. Local round-trip always uses the gate's
  own OS shell via `transport: local` (`service:local-shell`), gated on
  `runtime.GOOS` matching the pattern OS. EncodedCommand/chunking/NuGet-URL/
  target-pull-script are compared against committed `.golden` fixtures under
  `internal/transport/testdata` and `internal/artifact/testdata`. Host-key
  mismatch (CON-04) is proven against an in-process stub key -- no live sshd, no
  third-party SSH server dependency. Thus every required scenario runs in the
  gate with the env-var unset; the compose stack is only the env-var-set path.

**docker-compose.yml** (`tests/e2e/transport-artifact/docker-compose.yml`)
services (env-var-set path only):
- `artifacts` -- nginx serving `sample-svc` zip/nupkg plus a 404 route and a
  bearer-auth location, mirroring the httptest routes (target of
  `LD_ARTIFACT_URL`).

No sshd service is provisioned here: live SSH/SFTP round-trip is lab-only
(`implementation-plan.md:126`) and is exercised as CON-06 in Phase 4. Phase 2
proves ONLY the in-process host-key-mismatch mapping (CON-04) with a stub key.

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

  Scenario: PAT-03 docker_container run/rollback golden
    Given a `docker_container` spec (ports, volumes, env, restart_policy, digest)
    When the `docker run`/replace and rollback (previous-image restore) scripts
    are generated
    Then each matches its committed golden (`implementation-plan.md:421`),
    covering the sixth pattern that PAT-01 omits.

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
  `go test ./test/e2e/linux/... -run
  'CON_06|CAP_03|IDP|LCK|DRF_02|DST_02|ART_06|DKR' -tags acc` with `TF_ACC=1`.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, linux, forge-lab-hw]`. L1 Linux VM (Ubuntu 22.04) with sshd and
  docker preinstalled.
- **Artifact source (HTTP, with injectable per-request delay)**: the http zip
  source is addressed by env-var name `LD_L1_ARTIFACT_URL`; the seeding script
  stands up a tiny static file host on the L1 VM whose `/slow/*` route reads a
  `delay_ms` QUERY PARAMETER on each request and sleeps that long before the first
  byte (per-request, so a long-running server started at bootstrap honors it
  without any runner-process env change). LCK-01 points apply A at
  `${LD_L1_ARTIFACT_URL}/slow/sample-svc-1.0.0.zip?delay_ms=30000` to hold the
  `.lock` open deterministically while apply B races; no wall-clock guesswork and
  no dependency on process-environment observation.
- **Registry source**: the docker registry (ART-06, DKR-01..03) is addressed by
  env-var name only: `LD_REGISTRY` (registry host), `LD_REGISTRY_USER` (username),
  and `LD_REGISTRY_PASSWORD` (resolved via `auth.password_env`). The
  `sample-svc` image at `1.0.0`/`1.1.0` (with a captured `digest` for DKR-02) is
  seeded into the registry by bootstrap.
- **Secrets**: Azure Key Vault `kv-forge-lab` entries `labdeploy-lab-password`
  (target auth), `labdeploy-registry-user`, and `labdeploy-registry-password`
  (registry auth), via ADO variable group `forge-lab-hw-vg` mapped to
  `env: LABDEPLOY_PASSWORD`, `env: LD_REGISTRY_USER`, `env: LD_REGISTRY_PASSWORD`
  **and** GitHub environment `lab-linux` secrets `LAB_PASSWORD`, `REGISTRY_USER`,
  `REGISTRY_PASSWORD` (mapped to the same env-var names). Target host from
  KeyVault entry `labdeploy-l1-host` / GitHub environment variable `LD_L1_HOST`;
  registry host + artifact host from GitHub environment variables `LD_REGISTRY` /
  `LD_L1_ARTIFACT_URL`.
- **Pre-test bootstrap**: `bash tests/e2e/linux/verify-l1.sh` -- VERIFIES (does
  not install) `sshd`, `docker version`, and free disk, then SEEDS (a) the http
  artifact host serving `sample-svc` zip fixtures incl. the delay-controlled
  `/slow/*` route and (b) the registry by building/tagging and `docker push`ing
  the `sample-svc` `1.0.0`/`1.1.0` images to `LD_REGISTRY` (recording the `1.1.0`
  digest into a fixture for DKR-02); idempotent. It VERIFIES OS prerequisites and
  only SEEDS test fixtures; it never installs docker or runtimes (targets are
  pre-provisioned, `tech-spec.md:165-170`). ART-06 supplies a deliberately wrong
  `LD_REGISTRY_PASSWORD` to force the `docker login` failure.
- **Gate provisioning**: n/a (lab). These scenarios skip when `LD_L1_HOST` is
  unset (allowed for `lab-*`); the deterministic slices (fetch, prune, lock,
  idempotent short-circuit, and docker run/rollback script generation via PAT-03)
  are proven in Phases 2-3 so the gate is never empty.

### Scenarios

**Feature: Linux transport, console_app, and generic lifecycle**

  Scenario: CON-06 Live SSH dial and SFTP round-trip
    Given `env(LD_L1_HOST)` reachable over `ssh` with `LABDEPLOY_PASSWORD` or
    `LD_SSH_KEY`
    When `Connect`/`Exec`/`Upload`/`Download` run over the ssh transport
    Then data round-trips, auth rejection is not retried, and a bad
    `ssh.host_key` maps to `[ERR_CONNECT]` `host key mismatch`
    (live SSH is lab per `implementation-plan.md:126`; the in-process host-key
    stub is the supplemental CON-04 in Phase 2).

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
    Given apply A is mid-flight holding `.lock`, fetching
    `${LD_L1_ARTIFACT_URL}/slow/sample-svc-1.0.0.zip?delay_ms=30000` (the server
    sleeps 30s per request before the first byte)
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
  (all pre-provisioned). A second pre-provisioned box `LD_W1_NOASPNET_HOST`
  (labelled `[self-hosted, windows, forge-lab-hw, no-aspnet]`) has the .NET
  runtime but intentionally NO `Microsoft.AspNetCore.App`, used only by NET-02.
- **Artifact source**: the lab HTTP/NuGet artifact host is pre-provisioned and
  addressed by env-var name only: `LD_ARTIFACT_URL` (http zip/nupkg base),
  `LD_ARTIFACT_TOKEN` (bearer token for the ART-05 authenticated pull, resolved
  via `auth.token_env`), `LD_NUGET_FEED` (NuGet v3 flat-container base), and
  `LD_NUGET_TOKEN`. These are never literal URLs/credentials in specs.
- **Secrets**: Azure Key Vault `kv-forge-lab` entries `labdeploy-lab-password`
  (target auth) and `labdeploy-artifact-token` / `labdeploy-nuget-token`
  (artifact-source auth), via ADO variable group `forge-lab-hw-vg` mapped to
  `env: LABDEPLOY_PASSWORD`, `env: LD_ARTIFACT_TOKEN`, `env: LD_NUGET_TOKEN`
  **and** GitHub environment `lab-windows` secrets `LAB_PASSWORD`,
  `ARTIFACT_TOKEN`, `NUGET_TOKEN` (mapped to the same env-var names). Target hosts
  from KeyVault entries `labdeploy-w1-host` / `labdeploy-w1-noaspnet-host` and
  GitHub environment variables `LD_W1_HOST` / `LD_W1_NOASPNET_HOST`; the artifact
  host from GitHub environment variables `LD_ARTIFACT_URL` / `LD_NUGET_FEED`.
- **Pre-test bootstrap**: `pwsh tests/e2e/windows/verify-w1.ps1` -- VERIFIES (does
  not install) `$PSVersionTable.PSVersion.Major >= 5`, WinRM HTTPS listener,
  `node`/`npm`/`dotnet --list-runtimes` (`Microsoft.AspNetCore.App` on the
  primary W1; ABSENT on the no-aspnet target), and `vstest.console.exe` on PATH;
  also seeds the artifact source by publishing the `sample-svc`
  `1.0.0`/`1.1.0`/`1.2.0-bad` zip+nupkg fixtures to `LD_ARTIFACT_URL` /
  `LD_NUGET_FEED` (`tests/e2e/windows/seed-artifacts.ps1`, idempotent) and
  removes leftover `sample-svc` services/releases. It VERIFIES OS prerequisites
  and only SEEDS test fixtures; it never installs runtimes (targets are
  pre-provisioned, `tech-spec.md:165-170`).
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

  Scenario: CON-05 Local transport on W1 opens no socket
    Given the runner IS W1, `transport = local`, `os = windows`,
    `hosts = [localhost]`
    When I apply a `console_app` spec
    Then apply succeeds without opening any socket
    (the transport-independent local round-trip is also proven gate-side as the
    supplemental LCL-01; this DESIGN ID exercises it on a real W1).

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
    Given `launcher = dotnet_dll` targeting `env(LD_W1_NOASPNET_HOST)`, a W1-class
    box intentionally lacking `Microsoft.AspNetCore.App`
    When I apply
    Then it errors `[ERR_PREFLIGHT]` naming `Microsoft.AspNetCore.App` before any
    file is copied.
    (Uses the dedicated no-ASP.NET target, NOT the primary W1; see Setup. Skips
    when `LD_W1_NOASPNET_HOST` is unset.)

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
`implementation-plan.md:484`,`:494`). Covers DESIGN section 18.10 (PIP-01..03):
the shipped reference pipelines `examples/pipelines/github-deploy.yml` (workflow
`name: deploy-lab`) and `examples/pipelines/azure-pipelines.yml` driving the
provider end to end, including `always()` artifact publishing and the
`if: failure()` / `condition: failed()` rollback job/stage, against the W1 lab
and filesystem-mirror provider distribution (DESIGN section 16.1).

This phase does NOT invent any repository or workflow. The dispatchable workflow
`.github/workflows/deploy-lab.yml` is a REPOSITORY ASSET owned by
`implementation-plan.md` Stage 8.2 (line 428, "Author `examples/pipelines/...`");
that stage promotes the example into `.github/workflows/deploy-lab.yml` on this
repo's default branch (the only place `workflow_dispatch` registers), identical
to the example except for an added `environment: lab` job binding. The gate-tier
`PIP_LINT-01` scenario mechanically proves that asset stays step-for-step equal
to the example so the reference workflow cannot silently drift
(`implementation-plan.md:438`, "Pipeline YAML well-formed", proof golden). The
example under `examples/pipelines/` intentionally omits `environment:` because it
is a user-copyable template; the repo's own CI asset adds the binding so the
GitHub environment `lab` secrets resolve.

### Setup
- **Type**: lab-bare-metal
- **Local**: not typically run locally. Because the dispatchable workflow is a
  committed default-branch asset (owned by `implementation-plan.md` Stage 8.2), no
  runtime repository mutation is needed: a developer runs
  `gh workflow run deploy-lab.yml -f version=1.1.0 -f checksum=sha256:<hex>`
  (BOTH `version` and `checksum` are required inputs per `github-deploy.yml:5-11`).
  The ADO leg is queued manually with the same `version` + `checksum` parameters.
  The gate-tier `PIP_LINT-01` needs no runner and runs in Forge's gate.
- **CI runner**: ADO agent pool `LabAgents` (the pool named in
  `azure-pipelines.yml:13`); GitHub self-hosted runner labels `[self-hosted, lab]`
  (the labels in `github-deploy.yml:17`), same W1 class as Phase 5. The runner
  lays the provider into its filesystem mirror via `make install`
  (`github-deploy.yml:28`) before the workflow runs.
- **Secrets**: Azure Key Vault `kv-forge-lab` entries `labdeploy-lab-password`,
  `labdeploy-nuget-pat`, and `labdeploy-gh-dispatch-token` (a least-privilege
  fine-grained GitHub PAT scoped to `actions:write` on this repo only -- used
  solely to trigger `workflow_dispatch`; it can neither push code nor edit repo
  variables), surfaced through the ADO variable group `lab-secrets` (referenced at
  `azure-pipelines.yml:11`) as secret variables `LABDEPLOY_PASSWORD`, `NUGET_PAT`,
  and `GH_ACTIONS_DISPATCH_TOKEN` **and** through the GitHub environment `lab`
  (bound via the `environment: lab` job binding on the committed
  `.github/workflows/deploy-lab.yml` asset -- without an `environment:` binding
  GitHub environment secrets do not resolve) with secrets `LABDEPLOY_PASSWORD` and
  `NUGET_PAT` (the exact names read at `github-deploy.yml:21-22` /
  `azure-pipelines.yml:33-34`). Target host from KeyVault `labdeploy-w1-host` /
  GitHub environment variable `LD_W1_HOST`. Rollback inputs are read from
  pre-existing GitHub/ADO variables `LAST_GOOD_VERSION` / `LAST_GOOD_CHECKSUM`
  (maintained by the release process, `github-deploy.yml:61-62`,
  `azure-pipelines.yml:61-62`); the test never writes them.
- **Pre-test bootstrap**: `pwsh tests/e2e/pipeline/install-provider.ps1` -- builds
  or downloads `terraform-provider-labdeploy_v<version>` and lays it into the
  `%APPDATA%\terraform.d\plugins\registry.local\smartpcr\labdeploy\...` mirror
  with the `~/.terraformrc` filesystem_mirror block; then reuses `verify-w1.ps1`.
  This provisions the PROVIDER binary only; the dispatchable workflow is a
  committed repo asset (no runtime commit, no repo-write token) and the target OS
  tooling is pre-provisioned. `PIP_LINT-01` needs no bootstrap (reads files from
  the checkout).
- **Gate provisioning**: `PIP_LINT-01` is the in-gate proof -- it parses the two
  committed `examples/pipelines/*.yml` plus the `.github/workflows/deploy-lab.yml`
  asset from the checkout (deps: none, no docker, no runner) and runs in Forge's
  gate every time. The live PIP-01..03 skip when `LD_W1_HOST` / the self-hosted
  runner are unavailable (allowed for `lab-*`); provider binary behavior and spec
  substitution are additionally proven in Phases 1-3.

### Scenarios

**Feature: reference pipeline contract (gate-tier structural proof)**

  Scenario: PIP_LINT-01 Shipped pipelines are well-formed and drift-free (gate)
    Given the committed `examples/pipelines/github-deploy.yml`,
    `examples/pipelines/azure-pipelines.yml`, and the promoted
    `.github/workflows/deploy-lab.yml` asset
    When each is parsed with a YAML parser in `go test ./test/pipeline`
    Then all are well-formed; the GH files declare required `version`+`checksum`
    inputs, `concurrency`, an `always()` upload step, and an `if: failure()`
    rollback job reading `LAST_GOOD_VERSION`/`LAST_GOOD_CHECKSUM`; the ADO file
    declares a `condition: always()` publish and a `condition: failed()` Rollback
    stage; AND `deploy-lab.yml` is step-for-step identical to
    `github-deploy.yml` except for the added `environment: lab` binding (a
    mechanical equality assertion so the dispatchable asset cannot drift from the
    shipped reference). (`implementation-plan.md:438`.)

**Feature: reference pipeline integration (live lab)**

  Scenario: PIP-01 GitHub deploy workflow publishes results
    Given the committed `.github/workflows/deploy-lab.yml` asset (`name:
    deploy-lab`, bound to `environment: lab`), dispatched with `version=1.1.0` and
    `checksum=sha256:<good-hex>`
    When the workflow runs on a `[self-hosted, lab]` runner
    Then the `deploy` job is green and the `always()` step uploads the
    `labdeploy-results-1.1.0` artifact containing trx + summary + logs from
    `examples/labdeploy-results/**`.

  Scenario: PIP-02 GitHub deploy failure triggers auto-rollback
    Given pre-existing repo variables `LAST_GOOD_VERSION=1.1.0` /
    `LAST_GOOD_CHECKSUM=sha256:<good-hex>`, dispatched with `version=1.2.0-bad`
    and its `checksum`
    When the workflow runs
    Then the `deploy` job fails with `[ERR_SERVICE_START]`, the `rollback` job
    (`if: failure()`) applies `app_version=${{ vars.LAST_GOOD_VERSION }}` /
    `app_checksum=${{ vars.LAST_GOOD_CHECKSUM }}` and ends green, and the target
    serves `v=1.1.0`.

  Scenario: PIP-03 Azure DevOps pass case publishes to the Tests tab
    Given a run of `azure-pipelines.yml` (pass case) on pool `LabAgents` with
    parameters `version` + `checksum`
    When the pipeline runs
    Then the `Deploy` stage is green, `PublishTestResults@2` shows 3 VSTest
    results in the Tests tab, and `PublishPipelineArtifact@1` publishes
    `labdeploy-results-<version>`. (On a failing `version`, the `Rollback` stage
    `condition: failed()` applies `$(LAST_GOOD_VERSION)` / `$(LAST_GOOD_CHECKSUM)`,
    mirroring PIP-02's GitHub rollback contract.)

---

## Traceability

Test names carry their scenario ID (`go test` unit/golden for the gate tier;
`terraform-plugin-testing` / terratest for the lab tier), satisfying the
"Scenario IDs MUST appear in test names 1:1" requirement (`tech-spec.md:294`).
The two ID namespaces are kept separate: normative DESIGN section 18 IDs (lab
tier + gate-runnable VAL) versus supplemental T1-T9 gate-proof IDs. Each row
cites its `architecture.md` section 6 proof pin so phase Type never contradicts
the authoritative contract.

**Normative DESIGN section 18 acceptance IDs**

| Phase | Tier | Scenario IDs | Type | Proof pin |
|---|---|---|---|---|
| 1 Spec/Schema | gate | VAL-01..09 | inline | T1 |
| 4 Linux + Docker | lab | CON-06, CAP-03, IDP-01, DRF-02, LCK-01..02, DST-02, ART-06, DKR-01..03 | lab-bare-metal | L2 |
| 5 Windows single-target | lab | CON-01..03, CON-05, ART-01..05, CAP-01..02, WSV-01..08, NOD-01..03, NET-01..02, RBK-01..02, DRF-01/03, DST-01 | lab-bare-metal | L1 |
| 6 Failover cluster | lab | CLU-01..08 | lab-wsfc | L3 |
| 7 E2E test resource | lab | E2E-01..07 | lab-bare-metal | L1 |
| 8 Pipeline integration | lab | PIP-01..03 | lab-bare-metal | L4 |

**Supplemental gate-proof IDs (NOT DESIGN section 18; implement T1-T9)**

| Phase | Supplemental IDs | Type | Proof pin |
|---|---|---|---|
| 1 Spec/Schema | HSH-01, RRP-01..02, SRT-01 | inline | T2, T9 |
| 2 Transport/Artifact proofs | ENC-01..02, LCL-01, CON-04, ART_FETCH-01..02, NUG-01, TPS-01 | compose | T3, T4, T5 |
| 3 Pattern/Engine/Logs proofs | PAT-01..03, ENG-01..03, LOG-01 | inline | T6, T7, T8 |
| 8 Pipeline contract (in-gate) | PIP_LINT-01 | lab-bare-metal (gate-runnable) | golden (`implementation-plan.md:438`) |

Notes:
- CON-04 (host-key mismatch, in-process stub) and LCL-01 (local round-trip) are
  supplemental gate proofs (T4, per `implementation-plan.md:127` and
  `architecture.md` T4/T5). The normative CON-05 (local on real W1) is lab-tier
  (Phase 5); the live WinRM legs CON-01..03 are lab-tier (Phase 5).
- ART fetch/404/header (ART_FETCH-01..02, `service:httptest`, T5) are supplemental
  gate proofs; the normative persisted-state legs ART-01..05 and the docker leg
  ART-06 are lab-tier (Phases 4-5). ART-06 appears exactly once (Phase 4).
- Phase 2 is `compose` because T4/T5 are `service:*` pins that
  `architecture.md:366-383` forbids as inline; its env-var-unset path
  self-provisions in-process (httptest + local shell + stub key, no docker) so it
  never skips. Docker-backed acceptance (docker_container, ART-06) is lab-only
  (Phase 4); no phase relies on docker inside Forge's gate.
- NET-02 runs against `LD_W1_NOASPNET_HOST` (a W1-class box lacking
  `Microsoft.AspNetCore.App`), distinct from the primary `LD_W1_HOST` whose
  bootstrap requires that runtime.
- CON-06 (live SSH dial + SFTP round-trip) is lab-tier (Phase 4) per
  `implementation-plan.md:126`; its in-process host-key-mismatch counterpart is
  the supplemental CON-04 (Phase 2). No live sshd runs in Forge's gate.
- SRT-01 (provider-state round-trip) and RRP-01/RRP-02 are supplemental T9
  gate proofs, NOT DESIGN section 18 IDs: RRP-01 is table-driven over the full
  immutable set (forces replacement) and RRP-02 proves a mutable change is an
  in-place update (`architecture.md:386` T9; `implementation-plan.md:311`). They
  are gate-runnable in Phase 1 alongside the normative VAL-01..09.
- PAT-03 (docker_container run/rollback golden, `implementation-plan.md:421`) is
  the gate-tier script-generation proof for the docker engine; the live docker
  run/rollback legs are DKR-01..03 (Phase 4).
- Phase 8 PIP-01..03 assert against the SHIPPED assets
  `examples/pipelines/github-deploy.yml` and `azure-pipelines.yml` (promoted to
  the dispatchable `.github/workflows/deploy-lab.yml` by `implementation-plan.md`
  Stage 8.2), which use `version`+`checksum` inputs, `[self-hosted, lab]` /
  `LabAgents`, secrets `LABDEPLOY_PASSWORD`+`NUGET_PAT`, `LAST_GOOD_VERSION`/
  `LAST_GOOD_CHECKSUM` rollback variables, and artifact `labdeploy-results-<version>`.
  These asset names DIVERGE from the DESIGN section 18.10 matrix wording
  (`e2e-results`/`labdeploy-logs` artifacts, `auto_rollback`/`rollback_to` inputs).
  This e2e doc does not unilaterally pick a winner: the divergence is a genuine
  upstream conflict (DESIGN is treated as normative-and-resolved by
  `architecture.md` and `tech-spec.md`, yet the shipped assets differ), so it is
  escalated for an operator pin as open question `pip-design-vs-assets` below. The
  scenarios currently follow the shipped assets because those are what exists and
  runs; if the operator pins DESIGN as authoritative, the assets and PIP-01..03
  Then-clauses update together in the next iteration.
- PIP_LINT-01 is the gate-tier structural proof (`implementation-plan.md:438`,
  proof golden, deps none): it parses the two committed `examples/pipelines/*.yml`
  and the promoted `.github/workflows/deploy-lab.yml`, asserting well-formedness,
  the required input/step/rollback contract, and step-for-step equality between
  the asset and the example modulo the `environment: lab` binding. It runs in
  Forge's gate so Phase 8 always has non-skippable coverage even when the lab is
  offline.
