# terraform-provider-labdeploy — Design Specification

Status: v1.0 (implementation spec — normative)
Audience: autonomous coding agents. Every "MUST/SHOULD/MAY" is RFC-2119. Anything not
specified here is decided by the Decision Table (§21); do not invent behavior.

---

## 1. Purpose & Scope

A Terraform provider (Go, terraform-plugin-framework, plugin protocol v6) that a CI
pipeline (GitHub Actions or Azure DevOps) invokes to:

1. Deploy build artifacts (nupkg / zip / docker image) to target lab machines
   (Windows or Linux) over SSH / WinRM / local execution.
2. Manage app lifecycle for five P0 deployment patterns (§9) + one P1 pattern.
3. Run E2E test binaries on the target, evaluate pass criteria, and collect
   logs + test results back to the CI runner for publishing.
4. Support rollout (create/upgrade), automatic rollback-on-failure, explicit
   rollback (downgrade apply), drift detection, and destroy.

Non-goals (v1): multi-tenant orchestration, Kubernetes, IIS hosting, Linux
systemd services (only console/docker patterns on Linux), CSV-hosted cluster
binaries, blue/green with load balancers, secret storage (secrets always come
from pipeline env vars).

---

## 2. Definitions

| Term | Meaning |
|---|---|
| Runner | Machine executing `terraform` + this provider (GH runner / ADO agent). May equal the target (`transport: local`). |
| Target | Lab machine receiving the deployment. Bootstrapped separately with a GH/ADO build agent (out of scope), reachable via SSH/WinRM. |
| Spec | YAML or JSON document (kind `Deployment` or `TestRun`) driving one resource. |
| Release | Immutable extracted artifact at `<root>/<app>/releases/<version>/`. |
| Current | Junction (Windows) / symlink (Linux) `<root>/<app>/current` → active release. |
| Manifest | Provider-owned JSON at `<root>/<app>/manifest.json`, source of truth for Read/drift. |
| Switchover | The stop → repoint junction → (re)configure service → start sequence. |
| Role | WSFC Generic Service cluster group. |

---

## 3. System Context

```
 GitHub Actions / ADO pipeline (secrets live here as env vars)
        │  terraform init/plan/apply  (-var version=...)
        ▼
 terraform CLI ──gRPC──► terraform-provider-labdeploy (this binary, runs on runner)
        │                        │
        │                internal engine
        │     ┌───────────┬──────┴──────┬────────────┐
        │   spec        transport     artifact     pattern
        │  parse/valid  ssh|winrm|    fetch+sha256  console|winsvc|node|
        │               local         (or target-   dotnet|cluster|docker
        │                             pull)
        ▼
   Target machine(s): Windows Server 2019/2022 (PS 5.1) or Linux (sh)
   Layout: <root>/<app>/{releases/<ver>, current→, shared/, staging/, manifest.json, .lock}
```

The provider binary always runs on the runner. "Deploying the provider to the
target" (user requirement) is satisfied by `transport: local` when the pipeline
job runs on the target's own build agent (Decision D1).

---

## 4. Repository Layout (this repo)

```
terraform-provider-labdeploy/
├── main.go
├── go.mod
├── Makefile
├── docs/DESIGN.md                  ← this file
├── internal/
│   ├── provider/                   # TF plugin surface
│   │   ├── provider.go
│   │   ├── deployment_resource.go
│   │   └── e2e_test_resource.go
│   ├── spec/                       # types.go, parse.go, validate.go
│   ├── transport/                  # transport.go, ssh.go, winrm.go, local.go
│   ├── artifact/                   # fetch.go (http|file|nuget|docker ref)
│   ├── pattern/                    # pattern.go, windows_service.go, console_app.go,
│   │                               # node_dotnet.go, cluster_generic.go, docker_container.go
│   ├── engine/                     # engine.go, cluster.go, manifest.go, health.go, paths.go, lock.go
│   └── logs/collect.go
└── examples/
    ├── main.tf
    ├── specs/*.yaml                # one per pattern + e2e
    └── pipelines/{github-deploy.yml, azure-pipelines.yml}
```

---

## 5. Terraform Surface

### 5.1 Provider block

```hcl
provider "labdeploy" {
  # optional; every field can be overridden by spec.target
  default_target {
    host            = "lab-node-01"
    transport       = "winrm"            # ssh | winrm | local
    os              = "windows"          # windows | linux
    port            = 5986               # 0 => transport default (ssh 22, winrm-https 5986, winrm-http 5985)
    username        = "labadmin"
    password_env    = "LABDEPLOY_PASSWORD"   # env var NAME on the runner
    private_key_env = ""                     # env var NAME holding PEM (ssh only)
    winrm_https     = true
    winrm_insecure  = true               # skip TLS verify (lab certs)
  }
}
```

### 5.2 `labdeploy_deployment`

| Attribute | Type | Req | Notes |
|---|---|---|---|
| `spec` | string | one-of | Inline YAML/JSON, kind `Deployment`. Exactly one of `spec`/`spec_file` (validated). |
| `spec_file` | string | one-of | Path relative to cwd. Content hashed into plan. |
| `variables` | map(string) | opt | Substituted into spec: `${var:NAME}` (§6.6). Unknown name ⇒ ERR_SPEC_INVALID. |
| `version_override` | string | opt | Overrides `artifact.version` (pipeline passes build number here). |
| `destroy_mode` | string | opt | `purge` (default) \| `unregister` \| `abandon` (§10.5). |
| **Computed** | | | |
| `id` | string | | `sha1(sorted(hosts)+"/"+name)[0:12] + ":" + name` |
| `name` | string | | from spec `metadata.name` |
| `deployed_version` | string | | after apply / refreshed by Read |
| `previous_version` | string | | "" when none |
| `hosts` | list(string) | | resolved target hosts |
| `release_path` | string | | remote path of current release (owner node for cluster) |
| `service_status` | string | | `running` \| `stopped` \| `not_installed` \| `n/a` (console) \| `online` \| `offline` (cluster role) |
| `spec_hash` | string | | sha256 of canonical JSON after substitution + override |

Plan semantics:
- Change to `spec_hash` ⇒ in-place Update, EXCEPT changes to immutable fields ⇒
  **RequiresReplace**: `pattern.type`, `pattern.*.service_name`, `pattern.*.role_name`,
  `pattern.install_root`, `metadata.name`, `target.hosts` (set inequality), `target.os`.
  Implemented as a plan modifier that parses old+new spec and compares these paths.
- `version_override`/`artifact.version` change ⇒ Update (rolling deploy §10.2/§10.3).
- Read (refresh) reconciles from target manifest (§10.4). Manifest missing ⇒
  `resp.State.RemoveResource` (next plan = create).
- Timeouts block supported: create=30m, update=30m, delete=15m defaults.
- Import: NOT supported v1 → error `"import is not supported; adopt via apply"`.

### 5.3 `labdeploy_e2e_test`

| Attribute | Type | Req | Notes |
|---|---|---|---|
| `spec` / `spec_file` | string | one-of | kind `TestRun`. |
| `variables` | map(string) | opt | same substitution. |
| `deployment_id` | string | opt | Set to `labdeploy_deployment.x.id` purely to create the dependency edge. |
| `triggers` | map(string) | opt | **RequiresReplace**. Pipelines set `{ run = var.build_id }` to force re-run per build. |
| `fail_on_test_failure` | bool | opt | default `true`. If true, unmet pass criteria ⇒ apply error ERR_TEST_FAILED (after collection). |
| **Computed** | | | |
| `passed` | bool | | pass criteria met |
| `exit_code` | number | | runner process exit code |
| `total_tests` / `passed_tests` / `failed_tests` / `skipped_tests` | number | | from TRX/JUnit; -1 when `results.format: none` |
| `results_dir` | string | | LOCAL dir on runner containing downloaded results+logs |
| `duration_seconds` | number | | |
| `summary` | string | | one-line, e.g. `passed 41/42 (1 failed) in 318s` |

Lifecycle: Create runs the tests. Update is never in-place (all arguments are
RequiresReplace). Delete removes the remote test dir (`best effort`, never fails
destroy). **Collection always happens before returning a test-failure error** so
pipelines can publish artifacts from an `always()` step.

---

## 6. Deployment Spec (kind: Deployment) — normative schema

Accepted as YAML or JSON. Provider MUST convert YAML→JSON, apply `${var:*}`
substitution, apply `version_override`, then validate. Canonical JSON
(sorted keys, no insignificant whitespace) feeds `spec_hash`.

### 6.1 Envelope

| Path | Type | Req | Default | Validation |
|---|---|---|---|---|
| `apiVersion` | string | Y | — | == `labdeploy/v1` |
| `kind` | string | Y | — | == `Deployment` |
| `metadata.name` | string | Y | — | `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` |
| `metadata.labels` | map | N | {} | informational |

### 6.2 `target`

| Path | Type | Req | Default | Validation |
|---|---|---|---|---|
| `target.transport` | enum | N* | provider default | `ssh\|winrm\|local` |
| `target.hosts` | []string | N* | provider default host | len==1 for all patterns except `cluster_generic_service` (len 2..16). With `local`, MUST be `["localhost"]`. |
| `target.os` | enum | Y* | provider default | `windows\|linux`; must satisfy §9 support matrix |
| `target.port` | int | N | transport default | 1..65535 |
| `target.credentials.username` | string | cond | provider default | required for ssh/winrm |
| `target.credentials.password_env` | string | cond | provider default | Name of env var on runner. Secret VALUE never appears in spec/state/logs. |
| `target.credentials.private_key_env` | string | cond | — | ssh alt-auth. One of password_env/private_key_env required for ssh. |
| `target.winrm.use_https` | bool | N | true | |
| `target.winrm.insecure_skip_verify` | bool | N | false | |
| `target.winrm.timeout_seconds` | int | N | 60 | per-operation |
| `target.ssh.host_key` | string | N | "" | ""=accept-any (lab). Else base64 pubkey pinned. |
| `target.ssh.timeout_seconds` | int | N | 30 | dial |
| `target.connect_retries` | int | N | 3 | dial retries, 5s backoff |

\* Each field individually falls back to `provider.default_target`; after merge,
missing required fields ⇒ ERR_SPEC_INVALID naming the JSON path.

### 6.3 `artifact`

| Path | Type | Req | Default | Validation |
|---|---|---|---|---|
| `artifact.type` | enum | Y | — | `zip\|nupkg\|docker_image` |
| `artifact.version` | string | Y | — | `^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`; becomes release dir name |
| `artifact.checksum` | string | cond | — | `^sha256:[0-9a-f]{64}$`. REQUIRED for zip/nupkg. Forbidden for docker_image (use digest). |
| `artifact.fetch_mode` | enum | N | `target_pull` when source=http & os supports it, else `runner_push` | `target_pull\|runner_push` (§8.3) |
| `artifact.source.type` | enum | Y | — | `http\|file\|nuget_feed\|docker_registry` |
| — http: `url` | string | Y | — | https?:// |
| — http: `auth.header` | string | N | — | e.g. `Authorization`; value comes from `auth.token_env` |
| — http: `auth.token_env` | string | N | — | env var NAME on runner |
| — file: `path` | string | Y | — | path on RUNNER |
| — nuget_feed: `feed_url`,`package_id` | string | Y | — | version = artifact.version; download URL = `<feed>/flatcontainer/<idLower>/<verLower>/<idLower>.<verLower>.nupkg` (NuGet v3 flat container); `auth.token_env` sent as `Authorization: Bearer` or Basic per `auth.scheme: bearer\|basic(default basic,user=pat)` |
| — docker_registry: `image` | string | Y | — | repo without tag |
| — docker_registry: `tag` | string | N | =version | |
| — docker_registry: `digest` | string | N | — | `sha256:...`; when set, pull by digest and verify |
| — docker_registry: `auth.username`,`auth.password_env` | | N | — | `docker login` on target, `docker logout` after |

nupkg handling (D3): treated as zip; extract EVERYTHING preserving paths
(`[content_types].xml`, `_rels`, nuspec included — harmless). No nuget.exe on target.

### 6.4 `pattern` (discriminated union on `pattern.type`)

Common:

| Path | Type | Req | Default |
|---|---|---|---|
| `pattern.type` | enum | Y | — |
| `pattern.install_root` | string | N | `C:\deploy` (win) / `/opt/deploy` (linux) |
| `pattern.post_install` | string | N | command run in release dir after extract, before switchover; non-zero exit ⇒ ERR_SERVICE_INSTALL |

`console_app` (os: windows|linux):

| Field | Req | Notes |
|---|---|---|
| `exe` | Y | path RELATIVE to release dir |
| `args` | N | []string |
| `verify_command` | N | run in release dir after switchover; exit≠0 ⇒ failure ⇒ rollback |

No service is registered. `service_status` reports `n/a`. "Start" is a no-op;
health/verify define success.

`windows_service` (os: windows):

| Field | Req | Default | Notes |
|---|---|---|---|
| `service_name` | Y | — | `^[A-Za-z0-9_-]{1,80}$` |
| `display_name` | N | =service_name | |
| `description` | N | "" | |
| `exe` | Y | — | relative; the SCM-aware binary when wrapper=none |
| `args` | N | [] | |
| `wrapper` | N | `none` | `none\|winsw`. `none` ⇒ exe MUST implement SCM (e.g. .NET `UseWindowsService()`). |
| `winsw_exe` | cond | — | REQUIRED iff wrapper=winsw; relative path of WinSW binary INSIDE the artifact (D2). |
| `start_type` | N | `auto` | `auto\|manual\|delayed` |
| `account.username` | N | `LocalSystem` | or `NT AUTHORITY\NetworkService`, `.\user` |
| `account.password_env` | cond | — | required for non-builtin accounts |
| `recovery.restart_on_failure` | N | true | maps to `sc.exe failure ... actions= restart/5000/restart/5000/restart/5000 reset= 86400` |
| `stop_timeout_seconds` | N | 30 | then force-kill process tree (§9.2 step S3) |

`node_web_app` (os: windows) — installed as a Windows service via WinSW (D2):

| Field | Req | Default | Notes |
|---|---|---|---|
| `service_name` | Y | — | |
| `entry` | Y | — | e.g. `server.js`, relative |
| `node_exe` | N | `node` | must resolve on target PATH (preflight check `node --version`) |
| `port` | Y | — | exported as env `PORT` |
| `install_deps` | N | false | true ⇒ run `npm ci --omit=dev` in release dir (preflight `npm --version`); requires `package-lock.json` in artifact else ERR_SERVICE_INSTALL |
| `winsw_exe` | Y | — | relative path inside artifact |
| `stop_timeout_seconds` | N | 30 | |

`dotnet_api` (os: windows):

| Field | Req | Default | Notes |
|---|---|---|---|
| `service_name` | Y | — | |
| `launcher` | N | `exe` | `exe\|dotnet_dll` |
| `exe` | cond | — | required when launcher=exe (self-contained or apphost) |
| `dll` | cond | — | required when launcher=dotnet_dll; binPath becomes `"<dotnet_exe>" "<release>\<dll>" <args>`; preflight `dotnet --list-runtimes` must contain `Microsoft.AspNetCore.App` |
| `dotnet_exe` | N | `dotnet` | |
| `urls` | N | — | exported as `ASPNETCORE_URLS` |
| `hosting` | N | `windows_service_native` | `windows_service_native` (app uses UseWindowsService) \| `winsw` (+`winsw_exe`) |
| `args`,`account`,`recovery`,`stop_timeout_seconds` | | | same as windows_service |

`cluster_generic_service` (os: windows, hosts=all cluster node names, 2..16):

| Field | Req | Default | Notes |
|---|---|---|---|
| `service_name` | Y | — | Windows service registered on EVERY node, identical binPath |
| `role_name` | Y | — | cluster group name |
| `exe`,`args` | Y/N | | as windows_service; wrapper NOT supported (SCM-aware exe required, D5b) |
| `static_address` | N | — | passed to `Add-ClusterGenericServiceRole -StaticAddress` (creates CAP) |
| `preferred_owner` | N | — | if set, `Set-ClusterOwnerNode` after create; role moved here at end of successful update |
| `account`,`recovery`,`stop_timeout_seconds` | | | as windows_service |

Binaries live on EACH node's local disk at identical `install_root` paths (D5).
CSV-hosted binaries: out of scope v1.

`docker_container` (os: linux or windows w/ docker; P1):

| Field | Req | Default |
|---|---|---|
| `container_name` | Y | — |
| `ports` | N | [] `"host:container"` |
| `env_passthrough` | N | uses `environment` map below |
| `volumes` | N | [] |
| `restart_policy` | N | `unless-stopped` |
| `run_args` | N | [] extra `docker run` args |

### 6.5 `environment`, `files`, `health_check`, `strategy`, `logs`

| Path | Type | Req | Default | Notes |
|---|---|---|---|---|
| `environment` | map[string]string | N | {} | For services: written to `HKLM\SYSTEM\CurrentControlSet\Services\<svc>\Environment` (REG_MULTI_SZ) BEFORE start (§9.2 step S5). For docker: `-e`. For console verify/post_install: process env. Values MAY use `${var:*}`. |
| `files[].path` | string | | | relative to release dir; rendered content uploaded after extract |
| `files[].content` | string | | | UTF-8, `${var:*}` substituted |
| `health_check.type` | enum | N | `none` | `http\|tcp\|exec\|none` |
| `health_check.http.url` | string | cond | | Executed ON TARGET (D6): win `Invoke-WebRequest -UseBasicParsing`, linux `curl -fsS -o /dev/null -w %{http_code}` |
| `health_check.http.expect_status` | int | N | 200 | |
| `health_check.http.expect_body_regex` | string | N | "" | applied to body when non-empty |
| `health_check.tcp.port` | int | cond | | `Test-NetConnection -ComputerName localhost -Port` / `nc -z` |
| `health_check.exec.command` | string | cond | | run in release dir; exit 0 = healthy |
| `health_check.initial_delay_seconds` | int | N | 5 | |
| `health_check.interval_seconds` | int | N | 5 | |
| `health_check.timeout_seconds` | int | N | 60 | TOTAL budget incl. initial delay |
| `strategy.keep_releases` | int | N | 3 | ≥1; prune oldest by mtime after success, never the previous version |
| `strategy.rollback_on_failure` | bool | N | true | §10.2 |
| `strategy.cluster.drain_timeout_seconds` | int | N | 300 | Move-ClusterGroup wait |
| `strategy.cluster.health_settle_seconds` | int | N | 10 | sleep after group Online before health |
| `strategy.lock_timeout_seconds` | int | N | 900 | stale-lock threshold (§13) |
| `logs.paths` | []string | N | [] | globs; relative paths resolve under `<app>/shared/` |
| `logs.windows_event_logs[]` | obj | N | [] | `{log: Application, provider: "", level: <=Error?}`; collected `since` operation start |

### 6.6 Variable substitution

Token: `${var:NAME}` where `NAME ~ ^[A-Za-z_][A-Za-z0-9_]*$`. Source: the
resource's `variables` map only (never process env — prevents secret leakage
into state). Unresolved token ⇒ ERR_SPEC_INVALID `unresolved variable NAME at
<json-path>`. Escape: `$${var:...}` emits literal `${var:...}`.

---

## 7. TestRun Spec (kind: TestRun) — normative schema

`apiVersion: labdeploy/v1`, `kind: TestRun`, `metadata.name` as §6.1.
`target`: identical to §6.2 (hosts len==1).
`artifact`: identical to §6.3 (the E2E test binaries package; docker_image not
allowed here v1 ⇒ ERR_SPEC_INVALID).

| Path | Type | Req | Default | Notes |
|---|---|---|---|---|
| `runner.type` | enum | Y | — | `exec\|vstest\|dotnet_test\|npm` |
| `runner.command` | cond | — | | required for exec; run in test release dir |
| `runner.args` | []string | N | [] | |
| — vstest expands to | | | | `vstest.console.exe <args> /Logger:trx /ResultsDirectory:<results>` (vstest.console on PATH: preflight) |
| — dotnet_test | | | | `dotnet test <args> --logger trx --results-directory <results> --no-build` |
| — npm | | | | `npm <args or "test">` cwd=test release dir |
| `runner.working_dir` | string | N | test release dir | relative allowed |
| `runner.timeout_seconds` | int | N | 1800 | on expiry: kill process tree on target (win: `taskkill /PID <pid> /T /F`; linux: `kill -9 -<pgid>`) ⇒ ERR_TIMEOUT |
| `runner.env` | map | N | {} | |
| `results.format` | enum | N | `none` | `trx\|junit\|none` |
| `results.paths` | []string | cond | — | globs relative to test release dir; required unless format=none |
| `pass_criteria.exit_codes` | []int | N | [0] | |
| `pass_criteria.min_pass_rate` | float | N | 1.0 | pass_rate = passed/(total-skipped); only enforced when format≠none and total>0 |
| `collect.logs` | []string | N | [] | globs on target (absolute, or relative to a deployment's shared dir via `collect.app_shared_of: <appname>`) |
| `collect.windows_event_logs[]` | | N | [] | as §6.5, `since` = test start |
| `collect.destination_dir` | string | N | `./labdeploy-results/<name>-<unix>` | LOCAL runner dir; created; becomes `results_dir` output |

Test install layout on target: `<install_root>/_tests/<name>/releases/<version>/…`
(same staging/extract machinery; no service, no junction required — release dir
used directly). Results written on target to `<test-release>/_results/`, zipped,
downloaded, unzipped into `destination_dir/results/`; collected logs into
`destination_dir/logs/`; provider writes `destination_dir/summary.json`:

```json
{"name":"smoke","passed":true,"exit_code":0,"total":42,"passed_tests":41,
 "failed":1,"skipped":0,"pass_rate":0.976,"duration_seconds":318,
 "criteria":{"exit_codes":[0],"min_pass_rate":0.95},"error_code":""}
```

TRX parsing: read `<ResultSummary><Counters total executed passed failed ... />`
(namespace-insensitive). JUnit: sum over `<testsuite tests failures errors skipped>`.
Multiple result files: sum counters.

---

## 8. Component Contracts

### 8.1 transport

```go
type Shell int // ShellPowerShell, ShellSh, ShellCmd
type Cmd struct{ Shell Shell; Script string; TimeoutSec int; Env map[string]string }
type Result struct{ ExitCode int; Stdout, Stderr string }

type Transport interface {
    Connect(ctx context.Context) error
    Close() error
    OS() spec.OSKind
    Host() string
    Exec(ctx context.Context, c Cmd) (Result, error) // err = transport failure ONLY; app exit codes go in Result
    Upload(ctx context.Context, local io.Reader, size int64, remote string) error
    Download(ctx context.Context, remote, local string) error
}
```

Rules:
- Windows targets: ALL scripts are PowerShell 5.1 (`ShellPowerShell`), executed
  as `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass
  -EncodedCommand <base64(UTF-16LE(script))>` — identical over WinRM and SSH
  (works from cmd default shell). `Env` is injected by prepending
  `$env:K='V';` lines (single-quote escaped) — never on the command line in
  clear (values may be secrets).
- Linux targets: `ShellSh` ⇒ `sh -c '<script>'` with env prepended `K='V' `.
- WinRM upload: chunked base64 append. Chunk = 48,000 raw bytes. First chunk:
  `[IO.File]::WriteAllBytes($p,[Convert]::FromBase64String($b))`; subsequent:
  open FileStream Append + Write. Progress logged every 5 MB at TRACE.
- SSH upload/download: SFTP subsystem (`github.com/pkg/sftp`).
- Local: `os/exec` + file copy; `Connect` verifies OS matches `target.os`.
- `Connect` retries `target.connect_retries` times, 5s fixed backoff.
  Auth rejection (winrm 401, ssh auth error) is NOT retried ⇒ ERR_AUTH.
  Dial/timeout ⇒ ERR_CONNECT.
- Preflight (engine, after connect): win ⇒ `$PSVersionTable.PSVersion.Major`
  must be ≥5; free disk on install_root drive ≥ 2× artifact size (skip when
  size unknown); pattern-specific tool checks (node/npm/dotnet/docker/
  FailoverClusters module) per §6.4 ⇒ failures are ERR_PREFLIGHT.

### 8.2 artifact

```go
type Fetched struct{ LocalPath string; Sha256 string; Size int64 } // runner temp file
Fetch(ctx, s spec.Artifact, vars Secrets) (Fetched, error)          // http|file|nuget → temp zip
TargetPullScript(s spec.Artifact) (script string, ok bool)          // §8.3
```
- http/nuget: stream to `os.CreateTemp`, compute sha256 while streaming; compare
  to `artifact.checksum` ⇒ mismatch = ERR_CHECKSUM_MISMATCH (temp deleted).
  4xx/5xx/conn error ⇒ ERR_ARTIFACT_FETCH (include status + first 256B body).
- docker_image: no fetch on runner; returns ref string; pull happens on target
  inside pattern (§9.6) — bad creds/pull failure ⇒ ERR_ARTIFACT_FETCH.

### 8.3 target_pull (default for http/nuget sources)

Runner never proxies large payloads. Generated PS (win):
```powershell
$ProgressPreference='SilentlyContinue'
$h=@{}; if($env:LD_AUTH_VALUE){$h['<auth.header|Authorization>']=$env:LD_AUTH_VALUE}
Invoke-WebRequest -UseBasicParsing -Uri '<url>' -Headers $h -OutFile '<staging>\pkg.zip'
$sha=(Get-FileHash '<staging>\pkg.zip' -Algorithm SHA256).Hash.ToLower()
if($sha -ne '<hex>'){ Write-Error "checksum mismatch: $sha"; exit 41 }
```
`LD_AUTH_VALUE` is passed via Cmd.Env (encoded script, not persisted). Exit 41
maps to ERR_CHECKSUM_MISMATCH. Linux analog uses curl + `sha256sum`.
`fetch_mode: runner_push` forces §8.2 + Upload (used when target has no
egress). file:// sources always runner_push.

### 8.4 pattern

```go
type ReleaseCtx struct{ App, Version string; P Paths; Spec *spec.Deployment; PrevVersion string }
type Pattern interface {
    Validate(s *spec.Deployment) error                       // static, pattern-specific
    Preflight(ctx, t Transport, rc ReleaseCtx) error
    Configure(ctx, t Transport, rc ReleaseCtx) error         // idempotent: create/update service reg, env, wrapper cfg. Files already switched.
    Stop(ctx, t, rc) error                                   // no-op if absent/stopped
    Start(ctx, t, rc) error                                  // wait until Running/exit ok
    Status(ctx, t, rc) (string, error)                       // §5.2 service_status values
    Uninstall(ctx, t, rc, purge bool) error
}
```
Engine owns: fetch, staging, extraction, junction switch, files/env rendering,
health, manifest, prune, lock. Patterns own service/SCM/docker/cluster verbs.

### 8.5 engine

`Deploy(ctx, s) (*Status, error)`, `Destroy(ctx, s, mode)`, `ReadStatus(ctx, s)`.
Cluster spec routes to `engine/cluster.go`. All remote steps logged via tflog
(`step=<NAME> host=<h>`), names exactly: VALIDATE CONNECT PREFLIGHT LOCK FETCH
CHECKSUM STAGE EXTRACT RENDER CONFIGURE STOP SWITCH START HEALTH FINALIZE PRUNE
UNLOCK ROLLBACK FORCE_KILL MOVE_GROUP.

---

## 9. Target Layout & Pattern Algorithms

### 9.1 Layout (identical shape both OSes; `\` ⇢ `/` on linux)

```
<install_root>\<app>\
  releases\<version>\        # immutable payload
  current                    # junction/symlink → releases\<version>
  shared\logs\               # survives releases; app should log here (env LD_SHARED_DIR provided)
  staging\                   # downloads/extract temp; wiped at start & end of every op
  manifest.json
  .lock
```
Creation (win): dirs via `New-Item -ItemType Directory -Force`.
Junction switch (win) — EXACT commands:
```powershell
if (Test-Path '<cur>') { cmd /c rmdir '<cur>' ; if($LASTEXITCODE -ne 0){exit 42} }
cmd /c mklink /J '<cur>' '<rel>' ; if($LASTEXITCODE -ne 0){exit 42}
```
(`rmdir` on a junction removes only the link.) Linux: `ln -sfn '<rel>' '<cur>'`.
Exit 42 ⇒ ERR_SWITCH.
Extraction (win): `Expand-Archive -Path pkg.zip -DestinationPath '<rel>' -Force`
(nupkg renamed to .zip first). Linux: `unzip -o` (preflight: unzip present).
Every release dir gets `.labdeploy-release.json` = `{version, sha256, extracted_at}`.

Provided env to every pattern (merged under spec `environment`, spec wins):
`LD_APP, LD_VERSION, LD_RELEASE_DIR, LD_SHARED_DIR, PORT (node only)`.

### 9.2 windows_service — switchover S-steps (normative)

```
S0 CONFIG-PRECHECK: sc.exe query "<svc>" → exit 1060 ⇒ not installed (fresh path)
S1 (update only) STOP:
     Stop-Service -Name <svc> -ErrorAction SilentlyContinue
     wait Stopped, poll 1s, max stop_timeout_seconds
S2 (timeout) FORCE_KILL:
     $pid=(Get-CimInstance Win32_Service -Filter "Name='<svc>'").ProcessId
     if($pid -gt 0){ taskkill /PID $pid /T /F }
     re-wait 10s; still not Stopped ⇒ exit 43 (ERR_SERVICE_STOP)
S3 SWITCH junction (§9.1)
S4 CONFIGURE:
   wrapper=none:
     fresh:  sc.exe create "<svc>" binPath= "\"<cur>\<exe>\" <args>" start= <auto|demand|delayed-auto> DisplayName= "<display>"
             (delayed: start= delayed-auto)   obj= "<account>" password= via env-injected variable when non-builtin
     update: sc.exe config "<svc>" binPath= "..."  (binPath uses CURRENT junction ⇒ normally unchanged)
     sc.exe description "<svc>" "<description>"
     recovery: sc.exe failure "<svc>" reset= 86400 actions= restart/5000/restart/5000/restart/5000
   wrapper=winsw:
     write <cur>\<svc>.winsw.xml:
       <service><id><svc></id><name><display></name><executable><abs exe|node_exe></executable>
       <arguments>…</arguments><logpath><shared>\logs</logpath><stopwait><stop_timeout>sec</stopwait>
       <env name=… value=…/>…</service>
     fresh: & '<cur>\<winsw_exe>' install '<cur>\<svc>.winsw.xml'
     update: & … refresh  (fallback: uninstall+install if refresh exit≠0)
S5 ENV: Set-ItemProperty HKLM:\SYSTEM\CurrentControlSet\Services\<svc>
        -Name Environment -Type MultiString -Value @('K=V',…)   # full merged map §9.1
S6 START: Start-Service <svc>; poll Get-Service status==Running, 1s, max 60s
        not Running ⇒ capture last 20 System-log 7000/7009/7031/7034 events for <svc> into error detail ⇒ exit 44 (ERR_SERVICE_START)
S7 HEALTH per §6.5 ⇒ fail exit 45 (ERR_HEALTH_CHECK)
```
Fresh install runs S3(create junction) S4 S5 S6 S7 (no S1/S2).

### 9.3 console_app

Fresh/update: STAGE → SWITCH → post_install → verify_command (in `<cur>`, with
env) → health(optional). Nothing persistent runs; Stop/Start are no-ops;
Status = `n/a` if `.labdeploy-release.json` version matches manifest else drift.

### 9.4 node_web_app / dotnet_api

node: engine extraction → when install_deps: `npm ci --omit=dev` in release dir
(BEFORE switch; exit≠0 ⇒ ERR_SERVICE_INSTALL w/ last 50 lines) → then §9.2 with
wrapper=winsw and executable=node_exe, arguments=`"<cur>\<entry>"`, env includes
PORT.
dotnet: §9.2 where binPath = exe (launcher=exe) or
`"\"<dotnet_exe>\" \"<cur>\<dll>\" <args>"` (launcher=dotnet_dll);
hosting=winsw uses wrapper path instead. Env includes ASPNETCORE_URLS when set.

### 9.5 cluster_generic_service — algorithms (normative)

Connectivity: transport connects to EACH host in `target.hosts` (these MUST be
the cluster node names as `Get-ClusterNode` reports, case-insensitive).
Cluster cmdlets run on any connected node ("coordinator" = hosts[0]).
Preflight (coordinator): `Import-Module FailoverClusters` ok;
`(Get-ClusterNode | ? State -eq 'Up').Name` ⊇ hosts (any spec host not Up ⇒
ERR_PREFLIGHT); role existence + binding check:
`Get-ClusterResource | ? {$_.ResourceType -eq 'Generic Service'}` — if role
`role_name` exists, its `ServiceName` private property MUST equal
`service_name` else ERR_SERVICE_INSTALL (conflict).

CREATE (role absent):
```
C1 for each node (sequential, hosts order): LOCK, STAGE+EXTRACT release, SWITCH,
   S4 CONFIGURE (sc create, start= demand ← cluster controls start), S5 ENV
C2 coordinator: Add-ClusterGenericServiceRole -ServiceName <svc> -Name <role>
                [-StaticAddress <ip>]
   ⇒ error exit 46 ERR_SERVICE_INSTALL
C3 if preferred_owner: Set-ClusterOwnerNode -Group <role> -Owners <po>,<rest…>
   ; Move-ClusterGroup -Name <role> -Node <po> -Wait <drain>
C4 Start-ClusterGroup -Name <role> -Wait <drain>  (idempotent if Online)
   state≠Online ⇒ ERR_CLUSTER_MOVE
C5 sleep health_settle_seconds; HEALTH on OWNER node ⇒ fail: Remove role?
   NO — leave role Offline (Stop-ClusterGroup), report ERR_HEALTH_CHECK,
   manifest.last_operation=failed on all nodes (fresh create has no rollback target)
C6 FINALIZE manifests all nodes, UNLOCK all
```

UPDATE (role exists) — rolling, passive-first (D-order):
```
U0 LOCK all nodes (all-or-nothing; any node ERR_LOCKED ⇒ abort, unlock acquired)
U1 owner := (Get-ClusterGroup -Name <role>).OwnerNode.Name ; passives := hosts − owner
U2 for each passive p (hosts order): STAGE+EXTRACT new release on p (NO switch yet)
   — any failure here ⇒ abort; nothing switched; cleanup staging; ERR_*
U3 for each passive p: SWITCH junction, S4 CONFIGURE(update), S5 ENV
U4 firstNew := passives[0]
   MOVE_GROUP: Move-ClusterGroup -Name <role> -Node <firstNew> -Wait <drain_timeout>
   verify (Get-ClusterGroup).{OwnerNode==firstNew, State==Online} else exit 47 ERR_CLUSTER_MOVE
U5 sleep health_settle; HEALTH on firstNew
   ── FAIL ⇒ CLUSTER ROLLBACK:
      R1 Move-ClusterGroup -Node <owner> -Wait <drain>   (old owner still on OLD release)
      R2 verify Online on owner; HEALTH on owner (old version)
      R3 for each passive p: SWITCH junction back to PrevVersion; S4/S5 restore
      R4 manifests: current=old on all; last_operation=rolled_back
      R5 return ERR_HEALTH_CHECK (R1/R2 failure ⇒ ERR_ROLLBACK_FAILED, see §10.6)
U6 old owner o: STAGE+EXTRACT(already possible in U2? NO — U2 covers passives only;
   stage now), SWITCH, S4, S5   (service on o is stopped — cluster moved away)
U7 if preferred_owner set and ≠ current owner:
   Move-ClusterGroup -Node <preferred_owner> -Wait <drain>; HEALTH again
U8 FINALIZE all manifests (current=new, previous=old), PRUNE per node, UNLOCK all
```
Downtime budget: one failover (U4). No step stops the role other than moves.

DESTROY purge: `Stop-ClusterGroup -Name <role>` →
`Remove-ClusterGroup -Name <role> -RemoveResources -Force` → per node:
`sc.exe delete <svc>` → remove `<root>\<app>` dir. unregister: role+service
removed, files kept. abandon: nothing touched.

### 9.6 docker_container

```
D1 (auth) docker login <registry> -u <user> --password-stdin   (value via env/stdin)
D2 docker pull <image>:<tag>   or  <image>@<digest>
D3 old := docker inspect -f '{{.Image}}' <container_name>  (record for rollback)
D4 docker rm -f <container_name>   (ignore not-found)
D5 docker run -d --name <container_name> --restart <policy> [-p …] [-e …] [-v …] <ref>
D6 HEALTH ⇒ fail & rollback_on_failure ⇒ rm -f; docker run … <old image id>; health; ERR_HEALTH_CHECK
D7 docker logout (if D1)
```
manifest.current_version = spec version; image id recorded in manifest.extra.

---

## 10. Rollout / Rollback / Read / Destroy Semantics

### 10.1 Idempotency short-circuit
If manifest.current_version == spec version AND manifest.artifact_checksum ==
spec checksum AND pattern Status is healthy-equivalent (`running`/`online`/`n/a`)
⇒ Deploy returns success NO-OP (no fetch, no restart). This plus Read makes
re-apply produce an empty plan (E2E IDP-01).

### 10.2 Single-target failure & rollback matrix

| Failure at step | Machine mutated? | rollback_on_failure=true action | Result state |
|---|---|---|---|
| VALIDATE/CONNECT/PREFLIGHT/LOCK | no | none | old intact |
| FETCH/CHECKSUM/STAGE/EXTRACT/RENDER | staging only | wipe staging | old intact |
| SWITCH/CONFIGURE/START/HEALTH (fresh install) | yes | Stop; junction removed; service deleted; manifest absent | machine clean, apply error, no TF state |
| SWITCH/CONFIGURE/START/HEALTH (update) | yes | Stop → junction→prev → CONFIGURE(prev) → ENV(prev) → Start → HEALTH(prev) | old version RUNNING; TF state unchanged (Update error keeps prior state) ⇒ consistent |
| any, rollback_on_failure=false | yes | none | manifest.last_operation=failed; Read reports drift (§10.4) |

Rollback itself failing ⇒ §10.6.

### 10.3 Explicit rollback (pipeline-driven downgrade)
`terraform apply -var version=<old>` ⇒ Update. Engine treats downgrade
identically to upgrade EXCEPT: if `releases/<old>` exists AND its
`.labdeploy-release.json` sha256 matches spec checksum ⇒ SKIP fetch/stage
(FETCH step logs `cache_hit=true`). Cluster path identical (U-steps).

### 10.4 Read (refresh)
Connect (coordinator for cluster) → read manifest.
- manifest absent ⇒ RemoveResource.
- else set deployed_version/previous_version/release_path from manifest;
  service_status from pattern.Status (cluster: role State lowercase).
- manifest.last_operation.result==failed ⇒ append warning diag AND set
  `deployed_version = manifest.current_version + "!failed"` so plan shows a
  change (forces converging apply).
Read network errors ⇒ Read returns error (refresh fails loudly; no silent drop).

### 10.5 Destroy modes
purge: Stop → Uninstall(service/role/container) → remove `<root>/<app>` tree.
unregister: Stop → Uninstall → keep files+manifest deleted? manifest DELETED
(else Read after re-create confuses), releases kept.
abandon: no connection at all; state dropped.

### 10.6 ERR_ROLLBACK_FAILED
Both new version failed AND restore failed. Provider MUST: leave `.lock`
REMOVED, write manifest.last_operation={type:deploy,result:failed}, emit error
listing both underlying errors, and the diag detail MUST start with
`MACHINE IN UNKNOWN STATE host=<h>`. Next plan (via §10.4) shows a repairing
change; apply retries full deploy.

---

## 11. Secrets & State Hygiene (MUSTs)

- Specs/state carry env-var NAMES only; provider resolves values at runtime
  from the runner's environment (`os.Getenv`). Missing env var ⇒ ERR_SPEC_INVALID
  `env var <NAME> referenced by <path> is not set`.
- Secret values MUST never appear in: TF state, plan diffs, tflog at any level,
  error messages, or remote command lines (env-injection only §8.1).
- `spec` attribute is NOT marked sensitive (it contains no values); computed
  attributes never echo credentials.
- WinRM insecure_skip_verify=true and ssh host_key="" are permitted (lab) but
  emit a WARN diag once per apply.

---

## 12. Error Taxonomy (stable contract for pipelines & tests)

Every diagnostic Summary is exactly `[<CODE>] <short>`; Detail includes
`host=<h> step=<STEP>` plus remediation hint.

| Code | Meaning | Exit-code marker (remote) |
|---|---|---|
| ERR_SPEC_INVALID | schema/substitution/env-name failure | — |
| ERR_CONNECT | dial/timeout/host-key | — |
| ERR_AUTH | credential rejected | — |
| ERR_PREFLIGHT | OS/tool/disk precondition | — |
| ERR_LOCKED | lock held (§13) | — |
| ERR_ARTIFACT_FETCH | download/pull failure | — |
| ERR_CHECKSUM_MISMATCH | sha256 mismatch | 41 |
| ERR_EXTRACT | expand/unzip failure | — |
| ERR_SWITCH | junction/symlink | 42 |
| ERR_SERVICE_STOP | stop+force-kill failed | 43 |
| ERR_SERVICE_INSTALL | sc create/config, winsw, npm ci, role conflict | 46 |
| ERR_SERVICE_START | not Running in 60s | 44 |
| ERR_HEALTH_CHECK | §6.5 budget exhausted | 45 |
| ERR_CLUSTER_MOVE | Move/Start-ClusterGroup failed/not Online | 47 |
| ERR_ROLLBACK_FAILED | §10.6 | — |
| ERR_TIMEOUT | runner.timeout_seconds / TF timeouts | — |
| ERR_TEST_FAILED | pass_criteria unmet | — |
| ERR_UNSUPPORTED | pattern×os or feature gap | — |

## 13. Concurrency & Locking

`.lock` JSON `{owner:"<runner-host>/<pid>", op, started_utc}` created with
exclusive semantics: win `New-Item -ItemType File` after `Test-Path` guard inside
one encoded script using `[IO.File]::Open(path,'CreateNew')` (atomic; exists ⇒
exit 48); linux `set -C; > .lock`. Exists & age < lock_timeout_seconds ⇒
ERR_LOCKED (fail fast, <5s, no wait). Age ≥ timeout ⇒ overwrite + WARN
`stale lock from <owner> overridden`. Lock removed in FINALIZE and in ALL error
paths after LOCK (defer). Cluster: acquire on every node hosts-order,
release reverse.

## 14. Pattern × OS Support Matrix (validation table)

| pattern \ os | windows | linux |
|---|---|---|
| console_app | ✔ | ✔ |
| windows_service | ✔ | ✖ ERR_SPEC_INVALID |
| node_web_app | ✔ | ✖ |
| dotnet_api | ✔ | ✖ |
| cluster_generic_service | ✔ | ✖ |
| docker_container | ✔ (docker preflight) | ✔ |

Artifact×pattern: docker_image only with docker_container (else ERR_SPEC_INVALID);
zip/nupkg with everything except docker_container (else ERR_SPEC_INVALID).

## 15. Observability

- tflog structured fields on every step: `app, host, step, version, duration_ms`.
- `TF_LOG=DEBUG` shows scripts with env values REDACTED (`$env:K='***'`).
- Deployment resource emits a progress diag WARN only for stale-lock override
  and insecure-transport notices; everything else is log-level.

## 16. CI/CD Integration (reference, shipped in examples/)

### 16.1 Provider distribution to runners
Publish `terraform-provider-labdeploy_v0.1.0_<os>_<arch>.zip` as a release/
pipeline artifact. Runner install layout (filesystem mirror):
```
$HOME/.terraform.d/plugins/registry.local/smartpcr/labdeploy/0.1.0/linux_amd64/terraform-provider-labdeploy_v0.1.0
%APPDATA%\terraform.d\plugins\registry.local\smartpcr\labdeploy\0.1.0\windows_amd64\terraform-provider-labdeploy_v0.1.0.exe
```
`~/.terraformrc`:
```hcl
provider_installation {
  filesystem_mirror { path = "/home/runner/.terraform.d/plugins" include = ["registry.local/*/*"] }
  direct { exclude = ["registry.local/*/*"] }
}
```
TF config: `required_providers { labdeploy = { source = "registry.local/smartpcr/labdeploy", version = "0.1.0" } }`.

### 16.2 GitHub Actions (examples/pipelines/github-deploy.yml)
Jobs: `deploy` (init/apply with `-var version=${{ inputs.version }}`,
env `LABDEPLOY_PASSWORD: ${{ secrets.LAB_PASSWORD }}`) → e2e is the SAME apply
(depends_on edge) → `always():` upload `labdeploy-results/**` via
actions/upload-artifact → `rollback` job `if: failure() && inputs.auto_rollback`
re-applies `-var version=${{ inputs.rollback_to }}`.

### 16.3 Azure DevOps (examples/pipelines/azure-pipelines.yml)
Stages Deploy → (implicit tests in apply) → publish: `PublishTestResults@2`
(testResultsFormat VSTest, files `**/labdeploy-results/**/*.trx`,
condition always()) + `PublishPipelineArtifact@1` for logs; Rollback stage
`condition: failed()` gated by variable, re-apply pinned previous version.
Secrets via variable group, mapped to env in the step (`env: LABDEPLOY_PASSWORD: $(LabPassword)`).

## 17. Unit-Test Requirements (per package, go test, no target needed)

| Package | Must cover |
|---|---|
| spec | yaml+json parse equivalence; every validation rule in §6/§7 (table-driven, one case per rule id); `${var}` substitution incl. escape & unresolved; canonical hash stability (key order) |
| transport | encodedCommand exact bytes for known script; chunking math (0, 1, 48000, 48001 bytes); env single-quote escaping (`O'Brien`); local transport round-trip |
| artifact | sha stream verify; 404; header auth injected; nuget flat-container URL construction |
| pattern | generated PS for S4 fresh vs update golden-file tests (all 5 patterns); winsw xml golden; cluster scripts golden (create/move/rollback) |
| engine | state machine unit tests with fake Transport (scripted Result queue): every row of §10.2 matrix; prune keeps previous; lock stale vs fresh; idempotent short-circuit |
| logs | trx & junit counters parse (fixtures); glob→zip script golden |
| provider | schema validation (both spec+spec_file, neither); RequiresReplace triggers on each immutable path; state round-trip |

Framework acceptance: `terraform-plugin-testing` acceptance tests behind
`TF_ACC=1` + env-provided lab targets run the E2E matrix below.

---

## 18. E2E Test Scenario Matrix (normative acceptance)

Environments: **L1** linux vm (ssh, docker installed) · **W1** Windows Server 2022
(winrm https self-signed, node 20+npm, .NET 8 runtime+vstest, docker optional)
· **C2** two-node WSFC (nodes `cn1`,`cn2`, Windows 2022, no CSV needed) ·
**C3** three-node WSFC (P1). Test app: `sample-svc` — tiny .NET worker that (a)
runs as console or windows service, (b) serves `GET /health` returning 200 and
body `v=<LD_VERSION>` on `PORT|ASPNETCORE_URLS`, (c) `--fail-start` flag exits
immediately, (d) `--health 500` serves 500, (e) writes env dump + heartbeat to
`%LD_SHARED_DIR%\logs\app.log`. Packaged as zip and nupkg at versions
`1.0.0`,`1.1.0`,`1.2.0-bad` (fail-start build). E2E test package `sample-tests`
(vstest) contains 3 pass + configurable-fail test via env `FAIL_ONE=1`.

Conventions: "apply ⇒ error CODE" means `terraform apply` exit≠0 and stderr
contains `[CODE]`. "plan empty" means `terraform plan -detailed-exitcode` exits 0.
Every scenario ends with: assert `.lock` absent on all touched hosts.

### 18.1 VAL — spec validation (no target needed)

| ID | Given | When | Expected |
|---|---|---|---|
| VAL-01 | spec with tab-broken YAML | plan | error ERR_SPEC_INVALID, message contains line number; no connection attempted (assert via unroutable host) |
| VAL-02 | missing `artifact.version` | plan | ERR_SPEC_INVALID naming `artifact.version` |
| VAL-03 | pattern windows_service + os linux | plan | ERR_SPEC_INVALID citing §14 matrix |
| VAL-04 | cluster pattern, hosts=[one] | plan | ERR_SPEC_INVALID `hosts: need 2..16` |
| VAL-05 | checksum `md5:…` | plan | ERR_SPEC_INVALID regex msg |
| VAL-06 | both `spec` and `spec_file` set | validate | TF schema error `exactly one of` |
| VAL-07 | `${var:missing}` in spec | plan | ERR_SPEC_INVALID `unresolved variable missing at <path>` |
| VAL-08 | `password_env: NOT_SET_VAR` | apply | ERR_SPEC_INVALID `env var NOT_SET_VAR … is not set` (before any dial) |
| VAL-09 | docker_image + windows_service | plan | ERR_SPEC_INVALID (artifact×pattern) |

### 18.2 CON — connectivity

| ID | Env | When | Expected |
|---|---|---|---|
| CON-01 | W1 | apply minimal console spec, winrm https insecure | success; exactly one WARN diag about insecure TLS |
| CON-02 | W1 wrong password | apply | ERR_AUTH; exactly 1 auth attempt (no retry); <15s wall |
| CON-03 | firewall-dropped port | apply, connect_retries=2 | ERR_CONNECT after 2 retries; log shows attempts=3 |
| CON-04 | L1 with pinned wrong `ssh.host_key` | apply | ERR_CONNECT detail `host key mismatch` |
| CON-05 | runner==W1, transport local, os windows | apply console spec | success without opening any socket |

### 18.3 ART — artifacts

| ID | Env | Given/When | Expected |
|---|---|---|---|
| ART-01 | W1 | http zip, correct sha, fetch_mode runner_push | success; release dir contains `.labdeploy-release.json` sha matching |
| ART-02 | W1 | sha in spec off by one hex | ERR_CHECKSUM_MISMATCH; target: no `releases/<ver>` dir, no manifest change, staging empty; service (if pre-existing) untouched & Running |
| ART-03 | W1 | url → 404 | ERR_ARTIFACT_FETCH incl. `404` |
| ART-04 | W1 | nupkg of sample-svc | success; files under `releases/<ver>/lib/...` per package layout; app runs |
| ART-05 | W1 | fetch_mode target_pull with auth header env | success; runner network counters confirm payload not proxied (or: run with runner egress to package host blocked ⇒ still success) |
| ART-06 | L1 | docker_registry with bad password_env value | ERR_ARTIFACT_FETCH containing `docker login` stderr snippet, container list unchanged |

### 18.4 CAP — console_app

| ID | Env | When | Expected |
|---|---|---|---|
| CAP-01 | W1 | fresh apply v1.0.0, verify_command `sample-svc.exe --version` | apply ok; `current` junction target ends `releases\1.0.0`; outputs deployed_version=1.0.0, service_status=`n/a` |
| CAP-02 | W1 | apply v1.1.0 then v1.0.0-cached rolled forward again ×3 with keep_releases=2 | after last apply exactly 2 dirs under releases (newest+previous); pruned dir gone |
| CAP-03 | L1 | same as CAP-01 over ssh, linux build | ok; `current` is symlink; verify via `readlink` |

### 18.5 WSV — windows_service

| ID | When | Expected |
|---|---|---|
| WSV-01 | fresh apply v1.0.0, health http `/health` | service exists (`sc query` RUNNING), start_type AUTO_START, recovery actions set, HKLM Environment contains `LD_VERSION=1.0.0` and spec env; health passed; app.log contains env dump proving env delivery |
| WSV-02 | apply v1.1.0 | zero-error apply; `/health` body `v=1.1.0`; downtime window (1s-poll curl from W1) ≤ stop_timeout+15s; releases has 1.0.0 & 1.1.0; previous_version output =1.0.0 |
| WSV-03 | apply v1.2.0-bad (fail-start) | apply exit≠0 with ERR_SERVICE_START incl. an event-log 7000/7009 line; POST-STATE: service RUNNING on v1.0.0? No — previous was 1.1.0 ⇒ RUNNING `/health` v=1.1.0; junction→1.1.0; TF state deployed_version=1.1.0; subsequent plan EMPTY |
| WSV-04 | apply v1.1.0 with `--health 500` variant + rollback on | ERR_HEALTH_CHECK; restored to prior version healthy (as WSV-03 shape) |
| WSV-05 | same as WSV-03 but rollback_on_failure=false | apply error; service state = stopped-or-crashed on 1.2.0-bad; manifest.last_operation.result=failed; NEXT `plan` non-empty (drift via §10.4); next apply of 1.1.0 repairs |
| WSV-06 | wrapper=winsw wrapping console build | fresh + upgrade both ok; winsw xml regenerated on upgrade (stopwait honored: inject slow-stop, measure) |
| WSV-07 | app ignores stop (slow-stop build), stop_timeout=10 | deploy succeeds; logs contain step FORCE_KILL; total STOP phase <25s |
| WSV-08 | non-builtin account `.\svcuser` password_env | service ObjectName=.\svcuser; secret absent from `sc qc` capture in TF logs at TRACE |

### 18.6 NOD / NET

| ID | When | Expected |
|---|---|---|
| NOD-01 | node app (bundled node_modules, install_deps=false), port 8087 | service running; `GET :8087/health` 200; env PORT visible in app.log |
| NOD-02 | install_deps=true, artifact w/o package-lock.json | ERR_SERVICE_INSTALL mentioning lockfile; no service created (fresh) |
| NOD-03 | install_deps=true with lockfile | `node_modules` exists in release dir; healthy |
| NET-01 | dotnet self-contained, hosting native, urls :8088 | running; health ok; ASPNETCORE_URLS present in HKLM env |
| NET-02 | launcher=dotnet_dll on box WITHOUT aspnet runtime | ERR_PREFLIGHT naming `Microsoft.AspNetCore.App` BEFORE any file copied |

### 18.7 CLU — failover cluster (C2 unless noted)

| ID | When | Expected |
|---|---|---|
| CLU-01 | fresh apply v1.0.0 role `sample-role` | service exists on cn1+cn2 (start_type DEMAND); `Get-ClusterGroup sample-role` Online; owner ∈ {cn1,cn2}; health on owner ok; both manifests current=1.0.0; TF service_status=`online` |
| CLU-02 | apply v1.1.0 while probe loop (1 Hz GET health via CAP) runs 120s | apply ok; probe outage ≤ drain-induced single gap ≤30s and only ONE gap; final owner = pre-update PASSIVE node; both nodes junction→1.1.0; role Online |
| CLU-03 (C3) | apply v1.1.0 on 3 nodes | passives updated in hosts order (log order asserts); former owner updated last; exactly one MOVE_GROUP (plus optional preferred move) |
| CLU-04 | apply v1.1.0 built `--health 500` | ERR_HEALTH_CHECK; role Online back on ORIGINAL owner; `/health` serves v=1.0.0; BOTH nodes junction→1.0.0; manifests last_operation=rolled_back; next plan shows pending change to 1.1.0 (desired ≠ actual) |
| CLU-05 | block WinRM to cn2 mid-test (before apply) | apply ⇒ ERR_CONNECT host=cn2 during CONNECT/U0; cn1 untouched: junction & role owner unchanged, no partial switch |
| CLU-06 | pre-create role `sample-role` bound to service `other-svc` | ERR_SERVICE_INSTALL conflict message includes both names; nothing modified |
| CLU-07 | destroy purge | role absent, services deleted both nodes, `<root>\sample-svc` gone both nodes |
| CLU-08 | preferred_owner=cn2, fresh+update | after each apply OwnerNode==cn2 |

### 18.8 RBK / DRF / DST / IDP / LCK

| ID | When | Expected |
|---|---|---|
| RBK-01 | after WSV-02, apply `-var version=1.0.0` | success; FETCH log `cache_hit=true` (no network to artifact host — block it to prove); health v=1.0.0; previous_version=1.1.0 |
| RBK-02 | keep_releases=1 (1.0.0 pruned), apply 1.0.0 | success WITH re-fetch (cache_hit=false) |
| DRF-01 | Stop-Service manually; `terraform plan` | plan shows `service_status running→stopped`-driven update; apply performs Start ONLY (log: no FETCH/SWITCH steps) and converges |
| DRF-02 | delete manifest.json; plan | plan = CREATE (state removed on refresh); apply reinstalls cleanly over leftovers (idempotent create must handle existing service ⇒ treats as update-config) |
| DRF-03 | manually repoint junction to 1.0.0 while state says 1.1.0; plan | plan shows deployed_version drift; apply converges to 1.1.0 without artifact fetch |
| DST-01 | destroy (purge) WSV deployment | service absent (`sc query`⇒1060), dir gone, state empty |
| DST-02 | destroy_mode=abandon | no connection made (unroutable host would still succeed); state empty; machine untouched |
| IDP-01 | re-apply identical spec (service running) | plan empty; with `-refresh-only` no changes; target file mtimes unchanged |
| LCK-01 | start apply A (slow artifact); apply B parallel | B fails <5s ERR_LOCKED naming owner of A; A completes normally |
| LCK-02 | plant `.lock` aged > lock_timeout; apply | success; WARN diag `stale lock … overridden` |

### 18.9 E2E — labdeploy_e2e_test resource

| ID | When | Expected |
|---|---|---|
| E2E-01 | deploy 1.1.0 + TestRun vstest (all pass), triggers.run=1 | apply ok; outputs total=3 passed=3 failed=0 passed=true; `results_dir/results/*.trx` exists; `summary.json` matches; logs dir contains app.log copied from shared |
| E2E-02 | FAIL_ONE=1, fail_on_test_failure=true | apply exit≠0 ERR_TEST_FAILED; **results_dir fully populated anyway**; no TF state for test resource (Create failed) — documented; deployment resource unaffected |
| E2E-03 | same, fail_on_test_failure=false | apply ok; passed=false failed=1; pipeline gate demo uses output |
| E2E-04 | runner.timeout_seconds=10 against sleep-forever test | ERR_TIMEOUT; on target the test process tree is DEAD (assert via process list); partial logs collected |
| E2E-05 | collect.windows_event_logs provider filter | events json in logs dir; ONLY events ≥ test start time |
| E2E-06 | triggers.run bumped 1→2, same everything | plan = replace; tests re-run |
| E2E-07 | results.format none, exit-code-only runner exec | passed reflects exit code list; counters = -1 |

### 18.10 PIP — pipeline integration (smoke, uses W1)

| ID | When | Expected |
|---|---|---|
| PIP-01 | GH workflow dispatch version=1.1.0 | green; uploaded artifact `e2e-results` contains trx+summary+logs; job summary echoes `summary` output |
| PIP-02 | GH dispatch version=1.2.0-bad, auto_rollback=true rollback_to=1.1.0 | deploy job fails with ERR_SERVICE_START; rollback job runs, ends green; final target serves v=1.1.0 |
| PIP-03 | ADO run of azure-pipelines.yml (pass case) | Tests tab shows 3 VSTest results; pipeline artifact `labdeploy-logs` present |

Harness notes for the agent: drive these with terratest or shell+`terraform`
against real VMs; W1/C2 provisioning scripts are out of scope of the provider
repo but the matrix above is the acceptance gate. Scenario IDs MUST appear in
test names 1:1.

---

## 19. Implementation Milestones (agent execution order)

1. spec (types/parse/validate) + unit tests → 2. transport (local, ssh, winrm)
→ 3. artifact → 4. engine paths/manifest/lock/health with fake transport tests
→ 5. pattern windows_service + console → 6. deployment resource CRUD → run
VAL/CON/ART/CAP/WSV → 7. node/dotnet → 8. cluster engine+pattern → CLU → 9.
e2e_test resource + logs → E2E → 10. docker → 11. examples/pipelines → PIP →
12. goreleaser packaging.

## 20. Build & Toolchain Constraints

Go ≥1.22; deps pinned in go.mod: terraform-plugin-framework v1.x,
terraform-plugin-log, masterzen/winrm, golang.org/x/crypto, pkg/sftp,
gopkg.in/yaml.v3. `golangci-lint run` clean; `make build test`. Binary name
`terraform-provider-labdeploy_v<version>`. No cgo. No external runner deps
beyond terraform itself.

## 21. Decision Table (ambiguity killers — binding)

| # | Question | Decision |
|---|---|---|
| D1 | "provider deployed to target machine"? | Provider always runs on runner; runner==target supported via `transport: local`. |
| D2 | How are non-SCM exes (node, plain console) run as services? | WinSW; wrapper binary MUST ship inside the artifact (`winsw_exe`). Provider never downloads third-party binaries. |
| D3 | nupkg semantics | Unzip-in-place; no NuGet client on target. Version/dir name from spec, not nuspec. |
| D4 | Health check origin | Always executed ON the target (localhost reachable, firewall-independent). |
| D5 | Cluster binary placement | Local disk per node, identical paths. CSV = v2. |
| D5b | Cluster + wrapper | Not supported v1; exe must be SCM-aware. |
| D6 | Secrets | Env-var-name indirection everywhere; values never persisted (§11). |
| D7 | YAML vs JSON precedence | Both accepted; canonical JSON drives hashing/plan. |
| D8 | Downgrade | Same code path as upgrade + release cache reuse (§10.3). |
| D9 | Import | Unsupported v1 (clear error). |
| D10 | PowerShell baseline | Windows PowerShell 5.1 (no pwsh7 dependency). |
| D11 | Update failing mid-way, rollback ok | TF state keeps old version; machine restored to old ⇒ consistent by construction. |
| D12 | Who prunes releases | Engine, after successful FINALIZE only; never deletes `previous_version`. |
| D13 | E2E on failure leaves no state | Accepted; `triggers` replace-model makes re-runs cheap; reporting mode via `fail_on_test_failure=false`. |
| D14 | Docker on Windows | Supported when `docker version` preflight passes; otherwise ERR_PREFLIGHT. |
| D15 | Where do E2E artifacts land in pipelines | Always under `collect.destination_dir`; pipelines publish with always()/condition:always. |
