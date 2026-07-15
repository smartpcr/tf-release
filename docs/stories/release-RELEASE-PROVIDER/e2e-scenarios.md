# release provider -- E2E Scenarios (QA acceptance)

Gherkin-style feature scenarios for `terraform-provider-labdeploy` (Go,
terraform-plugin-framework, plugin protocol v6). These are the executable
acceptance gate QA runs against the provider. Scenario IDs map 1:1 to the
normative matrix in `.forge-attachments/DESIGN.md` section 18; test names MUST
carry the same ID (e.g. `TestAcc_WSV_03_FailStartRollback`).

Cross-references (do not duplicate -- see sibling docs):
- Component boundaries, `Transport`/`Pattern`/engine contracts:
  `docs/stories/release-RELEASE-PROVIDER/architecture.md`.
- Schema field tables, error taxonomy, decision table:
  `docs/stories/release-RELEASE-PROVIDER/tech-spec.md`.
- Phase/stage build order (Phase 1..9): `implementation-plan.md`. The phase
  numbers below intentionally mirror that plan so a phase's tests land in the
  same iteration that builds its code.

## Conventions (apply to every scenario)

- **Secrets are env-var NAMES, never values.** Specs and Terraform state carry
  only the *name* of a runner env var (e.g. `password_env: LABDEPLOY_PASSWORD`);
  the provider resolves the value with `os.Getenv` at runtime (DESIGN section 11,
  D6). No scenario embeds a literal hostname, password, token, or key.
- **Target/host indirection.** The target host itself is supplied through an
  env var (`LD_TARGET_HOST`, `LD_CN1_HOST`, `LD_CN2_HOST`, ...) so the same
  scenario body runs against a compose container, an embedded server, or a lab
  box without edits.
- **Connection env-var switch.** Each `inline`/`compose` phase reads a single
  "connection endpoint" env var (named per phase). When it is **set**, the suite
  talks to the CI-provided service (docker-compose or lab). When it is **unset**,
  the suite self-provisions an embedded/ephemeral dependency IN-PROCESS (no
  docker) so the scenario still runs in Forge's gate instead of skipping.
- **Terminology.** "apply => error CODE" means `terraform apply` exits non-zero
  and stderr contains `[CODE]`. "plan empty" means
  `terraform plan -detailed-exitcode` exits 0. Every scenario ends by asserting
  `.lock` is absent on all touched hosts (DESIGN section 13).
- **Test app.** `sample-svc` (tiny .NET worker) packaged as zip + nupkg at
  `1.0.0`, `1.1.0`, `1.2.0-bad` (fail-start). `sample-tests` (vstest) has 3 pass
  tests plus a fail-on-demand test via `FAIL_ONE=1`. Health endpoint returns
  `200` with body `v=<LD_VERSION>`; `--fail-start` exits immediately;
  `--health 500` serves 500.
- **`TF_ACC`.** Acceptance tests gate on `TF_ACC=1`
  (`terraform-plugin-testing`). Gate-provisioned inline scenarios set it
  automatically; lab-only scenarios additionally require the lab connection env
  var and skip (allowed only for `lab-*`) when it is absent.

---

# Phase 1: Spec Validation and Provider Schema

Covers DESIGN section 18.1 (VAL) plus the Terraform-schema one-of/RequiresReplace
rules from section 5. No target is contacted; these are the fastest gate tests
and must pass before any transport work.

### Setup
- **Type**: inline
- **Local**: `go test ./internal/provider/... ./internal/spec/... -run VAL`
  (unit + `terraform-plugin-testing` cases). No network, no VM.
- **CI runner**: GitHub-hosted `ubuntu-latest` (and a matrix leg on
  `windows-latest` to prove path/regex parity). No labels required.
- **Secrets**: none.
- **Pre-test bootstrap**: none (`go` toolchain from `go.mod`, Terraform CLI from
  `hashicorp/setup-terraform`).
- **Gate provisioning**: none needed -- validation short-circuits before any
  dial. VAL-01 asserts "no connection attempted" by pointing `target.hosts` at
  an unroutable value taken from `LD_UNROUTABLE_HOST` (default `192.0.2.1`,
  TEST-NET-1) and asserting the error is `ERR_SPEC_INVALID`, never
  `ERR_CONNECT`.

### Scenarios

**Feature: Deployment spec schema validation**

  Scenario: VAL-01 Malformed YAML is rejected with a line number
    Given a `labdeploy_deployment` whose `spec` has a tab-broken YAML block
    And `target.hosts = [ env(LD_UNROUTABLE_HOST) ]`
    When I run `terraform plan`
    Then it errors `[ERR_SPEC_INVALID]`
    And the message contains the offending line number
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
    Given `${var:missing}` appears in the spec and `variables` omits `missing`
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

  Scenario: VAL-10 Immutable field forces replace (schema plan modifier)
    Given an applied deployment with `pattern.install_root = C:\deploy`
    When I change `install_root` and run `terraform plan`
    Then the plan shows `# forces replacement`
    And no in-place update is proposed (DESIGN section 5.2 immutable paths).

---

# Phase 2: Connectivity and Transport

Covers DESIGN section 18.2 (CON) across `ssh`, `winrm`, and `local` transports.
The `winrm-https` legs are genuinely Windows-Server behaviors validated in the
lab phases; here we prove the transport state machine (retries, auth-no-retry,
host-key pinning, local no-socket) with an in-process SSH server so the gate
never skips.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/connectivity/docker-compose.yml up -d`
  then `go test ./test/e2e/connectivity/... -run CON`. Set
  `LD_SSH_ENDPOINT=env(host:port)` to target the compose sshd.
- **CI runner**: GitHub-hosted `ubuntu-latest` for ssh/local legs. The winrm
  leg (CON-01) is deferred to Phase 5 (`lab-bare-metal`).
- **Secrets**: none for gate provisioning (ephemeral keypair generated in-test).
  The compose path reads `LABDEPLOY_PASSWORD` from a throwaway container
  credential injected by the compose file's `.env` (non-secret lab value).
- **Pre-test bootstrap**:
  `docker compose -f tests/e2e/connectivity/docker-compose.yml up -d --wait`.
- **Gate provisioning**: when `LD_SSH_ENDPOINT` is **unset**, the suite starts an
  embedded SSH server in-process (`gliderlabs/ssh`) on `127.0.0.1:0`, generates
  an ephemeral host + client keypair, exports the private-key PEM into the env
  var named by `private_key_env`, and points `LD_TARGET_HOST` at the loopback
  listener. No docker, no pre-provisioned service. The `local` transport legs
  (CON-05) always run in-process. The compose file's sshd remains the
  `LD_SSH_ENDPOINT`-set path in CI.

**docker-compose.yml** (`tests/e2e/connectivity/docker-compose.yml`) services:
- `sshd` -- OpenSSH server (linux target for CON-03/CON-04), exposes 22.
- `blackhole` -- an iptables-drop sidecar used to simulate a firewall-dropped
  port for CON-03 (connect stalls, no RST).

### Scenarios

**Feature: Transport connect, auth, and retry semantics**

  Scenario: CON-01 WinRM HTTPS with self-signed cert (lab)
    Given a Windows target reachable over `winrm` HTTPS with
    `insecure_skip_verify = true`
    When I apply a minimal `console_app` spec
    Then apply succeeds
    And exactly one WARN diagnostic is emitted about insecure TLS.
    (Runs in Phase 5 lab; skips in the inline gate -- winrm has no embedded
    server.)

  Scenario: CON-02 Wrong password is not retried
    Given credentials whose `password_env` resolves to a wrong value
    When I apply
    Then it errors `[ERR_AUTH]`
    And exactly one auth attempt is made (no retry)
    And wall time < 15s.

  Scenario: CON-03 Firewall-dropped port exhausts retries
    Given `target.hosts = [ env(LD_TARGET_HOST) ]` pointing at the blackhole
    And `connect_retries = 2`
    When I apply
    Then it errors `[ERR_CONNECT]`
    And the log shows `attempts=3` (initial + 2 retries, 5s backoff).

  Scenario: CON-04 SSH host-key mismatch
    Given `ssh.host_key` pinned to a key that does not match the server
    When I apply
    Then it errors `[ERR_CONNECT]` with detail `host key mismatch`.

  Scenario: CON-05 Local transport opens no socket
    Given `transport = local`, `os` matching the runner, `hosts = [localhost]`
    When I apply a `console_app` spec
    Then apply succeeds
    And no network socket is opened (asserted via a loopback packet counter /
    `netstat` delta of zero for the target port).

---

# Phase 3: Artifact Acquisition

Covers DESIGN section 18.3 (ART): http zip, checksum mismatch, 404, nupkg
unzip-in-place, `target_pull` vs `runner_push`, and docker-registry auth
failure. Fetch/verify logic is transport-independent, so the gate stands up an
in-process HTTP file server rather than requiring the CI artifact host.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/artifact/docker-compose.yml up -d`
  then `go test ./test/e2e/artifact/... -run ART`. Point
  `LD_ARTIFACT_URL` at the compose nginx and `LD_NUGET_FEED` at the compose
  registry.
- **CI runner**: GitHub-hosted `ubuntu-latest`. Deployment/execution uses the
  embedded SSH target from Phase 2 gate provisioning or `local`.
- **Secrets**: none for the gate path. The compose path reads
  `LD_ARTIFACT_TOKEN`, `LD_NUGET_TOKEN`, and `LD_REGISTRY_PASSWORD` for the
  authenticated-fetch legs; in CI these are non-secret throwaway container
  credentials from the compose `.env`.
- **Pre-test bootstrap**:
  `docker compose -f tests/e2e/artifact/docker-compose.yml up -d --wait`.
- **Gate provisioning**: when `LD_ARTIFACT_URL` is **unset**, the suite serves
  the fixture packages from a `net/http/httptest.Server` on `127.0.0.1:0`
  (with a route returning 404 for ART-03 and an auth-gated route for ART-05),
  and stands up an in-process NuGet v3 flat-container responder for ART-04.
  `docker_registry` (ART-06) cannot be embedded without a docker daemon, so it
  runs only on the compose/lab path (Phase 8) and is skipped in the pure-inline
  gate. The compose nginx/registry remain the `LD_ARTIFACT_URL`-set path.

**docker-compose.yml** (`tests/e2e/artifact/docker-compose.yml`) services:
- `artifacts` -- nginx serving `sample-svc` zip/nupkg with a bearer-auth
  location for ART-05.
- `nuget` -- BaGetter (NuGet v3 flat container) hosting `sample-svc` nupkg.
- `registry` -- CNCF `registry:2` (private, htpasswd) for ART-06 docker pulls.

### Scenarios

**Feature: Artifact fetch, checksum, and pull mode**

  Scenario: ART-01 HTTP zip with correct sha, runner_push
    Given an http zip source with matching `artifact.checksum` and
    `fetch_mode = runner_push`
    When I apply
    Then apply succeeds
    And `releases/<ver>/.labdeploy-release.json` records a sha matching the spec.

  Scenario: ART-02 Checksum mismatch leaves target untouched
    Given `artifact.checksum` off by one hex digit
    When I apply
    Then it errors `[ERR_CHECKSUM_MISMATCH]`
    And there is no `releases/<ver>` dir, no manifest change, staging is empty
    And a pre-existing service (if any) is still Running.

  Scenario: ART-03 HTTP 404
    Given an http url that returns 404
    When I apply
    Then it errors `[ERR_ARTIFACT_FETCH]` including `404`.

  Scenario: ART-04 nupkg is unzipped in place
    Given a `nupkg` source for `sample-svc`
    When I apply
    Then files land under `releases/<ver>/lib/...` (package layout preserved)
    And the app runs (no nuget client on target).

  Scenario: ART-05 target_pull with auth header does not proxy payload
    Given `fetch_mode = target_pull` and an auth header env var
    When I apply (optionally with runner egress to the package host blocked)
    Then apply succeeds
    And runner network counters confirm the payload was not proxied.

  Scenario: ART-06 docker_registry bad credentials (compose/lab only)
    Given a `docker_registry` source whose `password_env` value is wrong
    When I apply
    Then it errors `[ERR_ARTIFACT_FETCH]` containing a `docker login` stderr
    snippet
    And the container list is unchanged.
    (Skipped in the pure-inline gate: no docker daemon.)

---

# Phase 4: Console App Deployment and Lifecycle

Covers DESIGN section 18.4 (CAP) plus the pattern-agnostic lifecycle rows of
section 18.8 that do NOT need a Windows service: idempotency (IDP), locking
(LCK), manifest-delete drift (DRF-02), and the `abandon` destroy mode (DST-02).
The `console_app` pattern registers no OS service, so the entire release
machinery (stage/extract/junction-or-symlink/manifest/prune) is exercised over
`local` transport on the runner itself -- fully in-process, no docker.

### Setup
- **Type**: inline
- **Local**: `go test ./test/e2e/lifecycle/... -run 'CAP|IDP|LCK|DRF_02|DST_02'`.
  Uses `transport = local`, `hosts = [localhost]`, `os` matching the runner.
- **CI runner**: matrix of GitHub-hosted `ubuntu-latest` (symlink `current`) and
  `windows-latest` (junction `current`), so CAP-01/CAP-03 parity is proven.
- **Secrets**: none (local transport needs no credentials).
- **Pre-test bootstrap**: none. `install_root` is redirected to a per-test temp
  dir via `LD_INSTALL_ROOT` so the runner filesystem is never polluted.
- **Gate provisioning**: dependency-free by construction -- `local` transport
  plus an `httptest` artifact server (Phase 3 helper) self-provision everything.
  The optional `LD_SSH_ENDPOINT`-set path re-runs CAP-03 against the compose
  linux sshd to prove SFTP symlink handling; unset => embedded SSH (Phase 2).
  For LCK-01 the "slow artifact" is simulated by an httptest handler that
  sleeps, so no external timing dependency is needed.

### Scenarios

**Feature: console_app releases, pruning, idempotency, and locking**

  Scenario: CAP-01 Fresh console apply with verify_command
    Given a fresh apply of `sample-svc` v1.0.0 with
    `verify_command = sample-svc.exe --version`
    When I apply
    Then apply succeeds
    And the `current` junction/symlink resolves to `releases/1.0.0`
    And outputs `deployed_version = 1.0.0` and `service_status = n/a`.

  Scenario: CAP-02 Prune honors keep_releases and never the previous version
    Given `keep_releases = 2`
    When I roll v1.1.0 -> v1.0.0 -> v1.1.0 forward three times
    Then exactly two dirs remain under `releases` (newest + previous)
    And the pruned dir is gone.

  Scenario: CAP-03 Linux console over ssh uses a symlink
    Given the same fresh apply over `ssh` on linux
    When I apply
    Then apply succeeds
    And `current` is a symlink verified via `readlink`.

  Scenario: IDP-01 Re-apply of identical spec is a no-op
    Given an applied deployment (service/app running)
    When I re-apply the identical spec
    Then plan is empty
    And `-refresh-only` reports no changes
    And target file mtimes are unchanged (idempotency short-circuit, section 10.1).

  Scenario: DRF-02 Deleted manifest forces clean recreate
    Given I delete `manifest.json` on the target
    When I run `terraform plan`
    Then the plan is CREATE (state removed on refresh)
    And apply reinstalls cleanly over the leftovers (idempotent create handles
    the existing release as an update-config).

  Scenario: LCK-01 Second concurrent apply fails fast
    Given apply A is mid-flight holding `.lock` (slow artifact handler)
    When apply B runs in parallel against the same target
    Then B fails in < 5s with `[ERR_LOCKED]` naming A's owner
    And A completes normally.

  Scenario: LCK-02 Stale lock is overridden
    Given a planted `.lock` aged beyond `lock_timeout_seconds`
    When I apply
    Then apply succeeds
    And a WARN diagnostic `stale lock ... overridden` is emitted.

  Scenario: DST-02 Destroy mode abandon makes no connection
    Given `destroy_mode = abandon` and an unroutable `LD_TARGET_HOST`
    When I run `terraform destroy`
    Then destroy succeeds without contacting the target
    And Terraform state is empty
    And the machine is untouched.

---

# Phase 5: Windows Service Patterns and Service Drift

Covers DESIGN section 18.5 (WSV), 18.6 (NOD/NET), and the service-dependent
lifecycle rows of section 18.8 (RBK, DRF-01, DRF-03, DST-01). These validate
Windows-only behaviors -- SCM registration, `sc.exe` recovery actions,
`HKLM\...\Services\<svc>\Environment` (REG_MULTI_SZ) delivery, WinSW wrapping,
force-kill of a stuck service, event-log 7000/7009 capture, update downtime
windows, and rollback-on-failure -- that cannot be faithfully embedded. This is
a dedicated bare-metal Windows Server box (env **W1** in DESIGN section 18).

### Setup
- **Type**: lab-bare-metal
- **Local**: developer points `LD_TARGET_HOST` at a personal Windows Server 2022
  VM (WinRM HTTPS enabled), sets `LABDEPLOY_PASSWORD`, then
  `go test ./test/e2e/winservice/... -run 'WSV|NOD|NET|RBK|DRF_01|DRF_03|DST_01'
  -tags acc`.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, windows, forge-lab-hw]`. Windows Server 2022, PowerShell 5.1,
  Node 20+npm, .NET 8 runtime + `vstest.console.exe`, optional docker.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-hw-vg` maps it to `LABDEPLOY_PASSWORD`) **and**
  GitHub environment `lab-windows` secret `LAB_PASSWORD` (mapped to
  `env: LABDEPLOY_PASSWORD`). The target host comes from KeyVault entry
  `labdeploy-w1-host` / GH environment variable `LD_TARGET_HOST`.
- **Pre-test bootstrap**: `pwsh tests/e2e/winservice/bootstrap-w1.ps1` -- enables
  WinRM HTTPS with a self-signed cert, opens 5986, installs the .NET runtime and
  vstest, and confirms `node`/`npm` on PATH. Idempotent; safe to re-run.
- **Gate provisioning**: n/a (lab). Scenarios that require the W1 box skip (the
  only place a dependency-missing skip is allowed) when `LD_TARGET_HOST` is
  unset. The pattern-agnostic shapes of these behaviors are also covered
  in-process in Phase 4 (console_app) so the Forge gate is never empty.

### Scenarios

**Feature: windows_service lifecycle**

  Scenario: WSV-01 Fresh install with health check and env delivery
    Given a fresh apply of v1.0.0 with an http `/health` check
    When I apply
    Then `sc query` reports RUNNING with `start_type = AUTO_START`
    And recovery actions are set
    And `HKLM` Environment contains `LD_VERSION=1.0.0` and the spec env
    And the health check passes
    And `app.log` contains an env dump proving delivery.

  Scenario: WSV-02 In-place upgrade keeps downtime bounded
    Given v1.0.0 running
    When I apply v1.1.0
    Then apply is error-free
    And `/health` body is `v=1.1.0`
    And the downtime window (1s-poll curl) <= `stop_timeout + 15s`
    And `releases` holds 1.0.0 and 1.1.0
    And output `previous_version = 1.0.0`.

  Scenario: WSV-03 Failed start rolls back to previous version
    Given v1.1.0 running
    When I apply v1.2.0-bad (fail-start)
    Then apply errors `[ERR_SERVICE_START]` including an event-log 7000/7009 line
    And the service is RUNNING on v1.1.0 with `/health` `v=1.1.0`
    And the junction points to 1.1.0
    And TF state `deployed_version = 1.1.0`
    And the subsequent plan is empty.

  Scenario: WSV-04 Failing health check rolls back
    Given v1.0.0 running and `rollback_on_failure = true`
    When I apply v1.1.0 built with `--health 500`
    Then apply errors `[ERR_HEALTH_CHECK]`
    And the prior version is restored and healthy (WSV-03 shape).

  Scenario: WSV-05 Rollback disabled leaves drift for the next apply
    Given the WSV-03 setup but `rollback_on_failure = false`
    When I apply v1.2.0-bad
    Then apply errors
    And the service is stopped/crashed on 1.2.0-bad
    And `manifest.last_operation.result = failed`
    And the next plan is non-empty (drift, section 10.4)
    And a follow-up apply of 1.1.0 repairs it.

  Scenario: WSV-06 WinSW wrapper for a console build
    Given `wrapper = winsw` wrapping the console build
    When I apply fresh then upgrade
    Then both succeed
    And the WinSW xml is regenerated on upgrade (slow-stop honored and measured).

  Scenario: WSV-07 Stuck service is force-killed within budget
    Given a slow-stop build and `stop_timeout_seconds = 10`
    When I deploy
    Then deploy succeeds
    And logs contain the `FORCE_KILL` step
    And the total STOP phase is < 25s.

  Scenario: WSV-08 Non-builtin service account, secret never logged
    Given account `.\svcuser` with `password_env`
    When I apply
    Then the service `ObjectName = .\svcuser`
    And the secret is absent from any `sc qc` capture in TF logs at TRACE.

**Feature: node_web_app and dotnet_api patterns**

  Scenario: NOD-01 Bundled node modules
    Given a node app with bundled `node_modules`, `install_deps = false`,
    port from `LD_NODE_PORT`
    When I apply
    Then the service runs and `GET /health` returns 200
    And env `PORT` is visible in `app.log`.

  Scenario: NOD-02 install_deps without a lockfile
    Given `install_deps = true` and an artifact lacking `package-lock.json`
    When I apply
    Then it errors `[ERR_SERVICE_INSTALL]` mentioning the lockfile
    And no service is created (fresh install).

  Scenario: NOD-03 install_deps with a lockfile
    Given `install_deps = true` with a lockfile present
    When I apply
    Then `node_modules` exists in the release dir and the app is healthy.

  Scenario: NET-01 dotnet self-contained, native hosting
    Given a self-contained dotnet app, `hosting = windows_service_native`,
    urls from `LD_ASPNET_URLS`
    When I apply
    Then it runs, health passes, and `ASPNETCORE_URLS` is present in HKLM env.

  Scenario: NET-02 dotnet_dll without the ASP.NET runtime
    Given `launcher = dotnet_dll` on a box lacking `Microsoft.AspNetCore.App`
    When I apply
    Then it errors `[ERR_PREFLIGHT]` naming `Microsoft.AspNetCore.App`
    And the error occurs before any file is copied.

**Feature: service drift, explicit rollback, and destroy**

  Scenario: RBK-01 Explicit downgrade reuses the release cache
    Given the post-WSV-02 state (1.1.0 running, 1.0.0 cached)
    When I apply `-var version=1.0.0` with egress to the artifact host blocked
    Then apply succeeds with FETCH log `cache_hit=true`
    And `/health` `v=1.0.0` and output `previous_version = 1.1.0`.

  Scenario: RBK-02 Downgrade re-fetches when cache pruned
    Given `keep_releases = 1` so 1.0.0 was pruned
    When I apply 1.0.0
    Then apply succeeds WITH a re-fetch (`cache_hit=false`).

  Scenario: DRF-01 Manual Stop-Service drives a Start-only converge
    Given a running service that I stop manually
    When I run `terraform plan`
    Then the plan shows a `service_status running -> stopped` update
    And apply performs Start ONLY (no FETCH/SWITCH steps) and converges.

  Scenario: DRF-03 Manually repointed junction converges without fetch
    Given state says 1.1.0 but I repoint the junction to 1.0.0
    When I run `terraform plan`
    Then the plan shows `deployed_version` drift
    And apply converges to 1.1.0 without an artifact fetch.

  Scenario: DST-01 Destroy purge removes service, files, and state
    Given a purge-mode destroy of a WSV deployment
    When I run `terraform destroy`
    Then `sc query` returns 1060 (absent)
    And the app dir is gone and Terraform state is empty.

---

# Phase 6: Failover Cluster Deployment

Covers DESIGN section 18.7 (CLU). Validates `cluster_generic_service`: a Windows
service registered identically on every node, a WSFC Generic Service role,
rolling update with a single `Move-ClusterGroup`, rollback-on-failure that
returns the role to its original owner, mid-test node isolation, role/service
conflict detection, and `preferred_owner` placement. Requires a real Windows
Server Failover Cluster; nothing here can be embedded.

### Setup
- **Type**: lab-wsfc
- **Local**: developer points `LD_CN1_HOST`/`LD_CN2_HOST` at a personal two-node
  WSFC (nodes `cn1`,`cn2`, no CSV), sets `LABDEPLOY_PASSWORD`, then
  `go test ./test/e2e/cluster/... -run CLU -tags acc`.
- **CI runner**: ADO agent pool `forge-lab-wsfc`; GitHub self-hosted runner
  labels `[self-hosted, windows, wsfc, forge-lab]`. Runner sits outside the
  cluster and reaches both nodes over WinRM HTTPS. C3 scenarios (CLU-03) need a
  three-node cluster labeled additionally `wsfc-3node`.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-wsfc-vg` maps it to `LABDEPLOY_PASSWORD`)
  **and** GitHub environment `lab-wsfc` secret `LAB_PASSWORD` (mapped to
  `env: LABDEPLOY_PASSWORD`). Node hostnames come from KeyVault entries
  `labdeploy-cn1-host`/`labdeploy-cn2-host` and GH environment variables
  `LD_CN1_HOST`/`LD_CN2_HOST`.
- **Pre-test bootstrap**: `pwsh tests/e2e/cluster/bootstrap-wsfc.ps1` -- verifies
  the `FailoverClusters` PowerShell module, the cluster is Up, both nodes are Up,
  and removes any leftover `sample-role`/`sample-svc` from a prior run.
- **Gate provisioning**: n/a (lab). CLU scenarios skip when
  `LD_CN1_HOST`/`LD_CN2_HOST` are unset (allowed for `lab-*`). The engine's
  cluster state machine (hosts-order updates, single MOVE_GROUP, rollback to
  original owner) is additionally covered by fake-transport unit tests
  (DESIGN section 17) so cluster logic still has gate coverage.

### Scenarios

**Feature: cluster_generic_service rollout and rollback**

  Scenario: CLU-01 Fresh cluster role install
    Given a fresh apply of v1.0.0 for role `sample-role`
    When I apply
    Then the service exists on cn1 and cn2 (`start_type = DEMAND`)
    And `Get-ClusterGroup sample-role` is Online with owner in {cn1, cn2}
    And health on the owner passes
    And both manifests read `current = 1.0.0`
    And TF `service_status = online`.

  Scenario: CLU-02 Rolling update keeps a single short outage
    Given v1.0.0 Online and a 1 Hz `/health` probe loop for 120s
    When I apply v1.1.0
    Then apply succeeds
    And the probe sees at most ONE gap, drain-induced, <= 30s
    And the final owner is the pre-update PASSIVE node
    And both nodes' junctions point to 1.1.0 and the role is Online.

  Scenario: CLU-03 Three-node update order (C3)
    Given a three-node cluster on v1.0.0
    When I apply v1.1.0
    Then passives update in hosts order (asserted via log order)
    And the former owner updates last
    And there is exactly one MOVE_GROUP (plus an optional preferred move).

  Scenario: CLU-04 Failed health rolls the role back to the original owner
    Given v1.0.0 Online
    When I apply v1.1.0 built with `--health 500`
    Then apply errors `[ERR_HEALTH_CHECK]`
    And the role is Online back on the ORIGINAL owner serving `v=1.0.0`
    And both nodes' junctions point to 1.0.0
    And manifests read `last_operation = rolled_back`
    And the next plan shows a pending change to 1.1.0.

  Scenario: CLU-05 Node isolation mid-apply causes no partial switch
    Given WinRM to cn2 is blocked before apply
    When I apply v1.1.0
    Then it errors `[ERR_CONNECT]` `host=cn2` during CONNECT/U0
    And cn1 is untouched (junction and role owner unchanged, no partial switch).

  Scenario: CLU-06 Role bound to a foreign service is rejected
    Given a pre-created `sample-role` bound to service `other-svc`
    When I apply
    Then it errors `[ERR_SERVICE_INSTALL]` naming both `sample-role` and
    `other-svc`
    And nothing is modified.

  Scenario: CLU-07 Destroy purge removes the role and services on all nodes
    Given a purge-mode destroy
    When I run `terraform destroy`
    Then the role is absent, services are deleted on both nodes
    And `<root>\sample-svc` is gone on both nodes.

  Scenario: CLU-08 preferred_owner placement
    Given `preferred_owner = cn2`
    When I apply fresh then update
    Then `OwnerNode == cn2` after each apply.

---

# Phase 7: E2E Test Resource and Result Collection

Covers DESIGN section 18.9 (E2E) for the `labdeploy_e2e_test` resource: run a
test binary on the target, parse TRX/JUnit counters, evaluate `pass_criteria`,
enforce `fail_on_test_failure`, honor `triggers` replace semantics, kill a
runaway test tree on timeout, filter Windows event logs by test-start time, and
ALWAYS collect results before returning a failure. Runner and vstest logic is
transport-independent, so results parsing runs in the gate; a real target
executes the binary.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/testrun/docker-compose.yml up -d` then
  `go test ./test/e2e/testrun/... -run E2E`. Point `LD_TEST_ENDPOINT` at the
  compose target.
- **CI runner**: GitHub-hosted `ubuntu-latest` for the `exec`/`npm`/`dotnet_test`
  legs against a linux target; the `vstest` + Windows-event-log legs (E2E-05)
  run on the Phase 5 `lab-bare-metal` W1 box.
- **Secrets**: none for the gate path. The compose path reads
  `LABDEPLOY_PASSWORD` from the compose `.env` (throwaway container credential).
- **Pre-test bootstrap**:
  `docker compose -f tests/e2e/testrun/docker-compose.yml up -d --wait`.
- **Gate provisioning**: when `LD_TEST_ENDPOINT` is **unset**, the suite runs the
  test binary over `local` transport on the runner (or the Phase 2 embedded SSH
  target) and serves `sample-tests` from the `httptest` artifact server. The
  TRX/JUnit fixtures in `internal/logs/testdata` are parsed in-process, so
  counter math (E2E-07) needs no target at all. `vstest.console.exe` and Windows
  event logs are Windows-only; E2E-05 defers to the lab and skips inline.

**docker-compose.yml** (`tests/e2e/testrun/docker-compose.yml`) services:
- `target` -- OpenSSH linux container with the .NET SDK (`dotnet test`) and Node
  (`npm test`) toolchains for running `sample-tests`.
- `artifacts` -- nginx serving the `sample-tests` package (reused from Phase 3).

### Scenarios

**Feature: labdeploy_e2e_test execution and collection**

  Scenario: E2E-01 All tests pass and results are collected
    Given a deployed 1.1.0 and a vstest TestRun (all pass), `triggers.run = 1`
    When I apply
    Then outputs `total = 3`, `passed_tests = 3`, `failed_tests = 0`,
    `passed = true`
    And `results_dir/results/*.trx` exists and `summary.json` matches
    And the logs dir contains `app.log` copied from the shared dir.

  Scenario: E2E-02 Test failure still collects, leaves no test state
    Given `FAIL_ONE=1` and `fail_on_test_failure = true`
    When I apply
    Then it errors `[ERR_TEST_FAILED]`
    And `results_dir` is fully populated anyway
    And no TF state exists for the test resource (Create failed)
    And the deployment resource is unaffected.

  Scenario: E2E-03 Reporting mode surfaces failure without failing apply
    Given `FAIL_ONE=1` and `fail_on_test_failure = false`
    When I apply
    Then apply succeeds with `passed = false`, `failed_tests = 1`
    And a pipeline gate can consume the output.

  Scenario: E2E-04 Timeout kills the test process tree
    Given `runner.timeout_seconds = 10` against a sleep-forever test
    When I apply
    Then it errors `[ERR_TIMEOUT]`
    And the test process tree on the target is dead (asserted via process list)
    And partial logs are collected.

  Scenario: E2E-05 Event-log collection is time-filtered (lab)
    Given `collect.windows_event_logs` with a provider filter
    When I apply
    Then the events json in the logs dir contains ONLY events at/after test start.
    (Runs on the W1 lab; skips inline -- Windows event logs.)

  Scenario: E2E-06 Bumping triggers.run re-runs the tests
    Given a passing TestRun with `triggers.run = 1`
    When I change `triggers.run` to 2 and plan
    Then the plan is a replace and the tests re-run.

  Scenario: E2E-07 Exit-code-only runner with results.format none
    Given `results.format = none` and an `exec` runner
    When I apply
    Then `passed` reflects the `pass_criteria.exit_codes` list
    And the counters are `-1`.

---

# Phase 8: Docker Container Pattern

Covers the P1 `docker_container` pattern (DESIGN section 6.4, section 14 docker
preflight, D14) and the docker-registry artifact leg (ART-06). A docker daemon
is mandatory, so this phase is compose-based; there is no embedded substitute for
a container runtime.

### Setup
- **Type**: compose
- **Local**: `docker compose -f tests/e2e/docker/docker-compose.yml up -d` then
  `go test ./test/e2e/docker/... -run 'DKR|ART_06'`. Point
  `LD_DOCKER_HOST` at the compose Docker-in-Docker endpoint and `LD_NUGET_FEED`
  /`LD_REGISTRY` at the compose registry.
- **CI runner**: GitHub-hosted `ubuntu-latest` (docker preinstalled). A
  `windows-latest` leg validates the Windows docker preflight (D14) when a
  Windows docker daemon is available; otherwise that single case skips.
- **Secrets**: none for the gate path; the compose registry uses a throwaway
  htpasswd credential from the compose `.env` mapped to `LD_REGISTRY_PASSWORD`.
- **Pre-test bootstrap**:
  `docker compose -f tests/e2e/docker/docker-compose.yml up -d --wait`.
- **Gate provisioning**: this phase's Type is `compose` precisely because the
  dependency (a container runtime) cannot be self-provisioned in-process. The
  `LD_DOCKER_HOST`-set path uses the compose DinD service. When unset on a runner
  that itself has a usable docker socket, the suite falls back to that local
  socket (still no pre-provisioned application container -- it `docker run`s
  `sample-svc` itself). On a runner with no docker at all, the docker scenarios
  skip (documented P1 limitation; the pattern's script generation is covered by
  golden-file unit tests per DESIGN section 17).

**docker-compose.yml** (`tests/e2e/docker/docker-compose.yml`) services:
- `dind` -- Docker-in-Docker daemon exposed as `LD_DOCKER_HOST`.
- `registry` -- private `registry:2` (htpasswd) hosting the `sample-svc` image.

### Scenarios

**Feature: docker_container lifecycle**

  Scenario: DKR-01 Fresh container deploy by tag
    Given a `docker_container` spec for `sample-svc` with `ports` mapping and a
    `restart_policy`
    When I apply
    Then the container runs, `GET /health` returns 200
    And `manifest.json` records the running image reference.

  Scenario: DKR-02 Upgrade replaces the container by digest
    Given a running container at v1.0.0
    When I apply v1.1.0 pinned by `digest`
    Then the old container is replaced, `/health` serves `v=1.1.0`
    And the pull verified the digest.

  Scenario: DKR-03 Destroy purge removes the container
    Given a purge-mode destroy
    When I run `terraform destroy`
    Then the container is absent from `docker ps -a` and state is empty.

  Scenario: ART-06 docker_registry bad credentials
    Given a `docker_registry` source whose `password_env` value is wrong
    When I apply
    Then it errors `[ERR_ARTIFACT_FETCH]` with a `docker login` stderr snippet
    And the container list is unchanged.

---

# Phase 9: Pipeline Integration Acceptance Gate

Covers DESIGN section 18.10 (PIP): the shipped reference pipelines in
`examples/pipelines/` driving the provider end to end -- GitHub Actions and Azure
DevOps -- including artifact publishing on `always()`/`condition: always`, and
the auto-rollback job/stage on a failed deploy. This is the final acceptance
gate and exercises real filesystem-mirror provider distribution (section 16.1)
against the W1 lab box.

### Setup
- **Type**: lab-bare-metal
- **Local**: not typically run locally; a developer can trigger
  `gh workflow run github-deploy.yml -f version=1.1.0` against a personal W1 and
  self-hosted runner. The ADO leg runs via a manual pipeline queue.
- **CI runner**: ADO agent pool `forge-lab-hw`; GitHub self-hosted runner labels
  `[self-hosted, windows, forge-lab-hw]` (same W1 class as Phase 5). The runner
  installs the provider into its filesystem mirror per section 16.1 before the
  workflow runs.
- **Secrets**: Azure Key Vault `kv-forge-lab` entry `labdeploy-lab-password`
  (ADO variable group `forge-lab-hw-vg`, mapped to
  `env: LABDEPLOY_PASSWORD: $(LabPassword)`) **and** GitHub environment
  `lab-windows` secret `LAB_PASSWORD` (mapped to `env: LABDEPLOY_PASSWORD`).
  Target host via KeyVault `labdeploy-w1-host` / GH environment variable
  `LD_TARGET_HOST`.
- **Pre-test bootstrap**: `pwsh tests/e2e/pipeline/install-provider.ps1` -- builds
  or downloads `terraform-provider-labdeploy_v<version>` and lays it into
  `%APPDATA%\terraform.d\plugins\registry.local\smartpcr\labdeploy\...` with the
  `~/.terraformrc` filesystem mirror, then reuses `bootstrap-w1.ps1`.
- **Gate provisioning**: n/a (lab). PIP scenarios skip when `LD_TARGET_HOST` /
  the self-hosted runner are unavailable (allowed for `lab-*`). The provider
  binary and spec substitution are covered in earlier inline phases, so the gate
  still validates provider behavior without the pipeline harness.

### Scenarios

**Feature: reference pipeline integration**

  Scenario: PIP-01 GitHub deploy workflow publishes results
    Given the `github-deploy.yml` workflow dispatched with `version=1.1.0`
    When the workflow runs
    Then it is green
    And the uploaded `e2e-results` artifact contains trx + summary + logs
    And the job summary echoes the `summary` output.

  Scenario: PIP-02 GitHub deploy failure triggers auto-rollback
    Given dispatch with `version=1.2.0-bad`, `auto_rollback=true`,
    `rollback_to=1.1.0`
    When the workflow runs
    Then the deploy job fails with `[ERR_SERVICE_START]`
    And the rollback job runs and ends green
    And the final target serves `v=1.1.0`.

  Scenario: PIP-03 Azure DevOps pass case publishes to the Tests tab
    Given a run of `azure-pipelines.yml` (pass case)
    When the pipeline runs
    Then the Tests tab shows 3 VSTest results
    And the pipeline artifact `labdeploy-logs` is present.

---

## Traceability

Every scenario ID above is drawn from DESIGN section 18 and MUST appear verbatim
in the corresponding acceptance test name (`terraform-plugin-testing` /
terratest), satisfying the "Scenario IDs MUST appear in test names 1:1"
requirement. Phase numbers align with `implementation-plan.md` so each phase's
tests are authored in the same iteration that builds the code under test.

| Phase | Scenario IDs | Setup type |
|---|---|---|
| 1 Spec Validation | VAL-01..10 | inline |
| 2 Connectivity | CON-01..05 | compose |
| 3 Artifact | ART-01..06 | compose |
| 4 Console + Lifecycle | CAP-01..03, IDP-01, DRF-02, LCK-01..02, DST-02 | inline |
| 5 Windows Services | WSV-01..08, NOD-01..03, NET-01..02, RBK-01..02, DRF-01/03, DST-01 | lab-bare-metal |
| 6 Failover Cluster | CLU-01..08 | lab-wsfc |
| 7 E2E Test Resource | E2E-01..07 | compose |
| 8 Docker | DKR-01..03, ART-06 | compose |
| 9 Pipeline | PIP-01..03 | lab-bare-metal |
