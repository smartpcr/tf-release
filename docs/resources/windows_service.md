# labdeploy_windows_service

Deploys and manages **one Windows service on a single lab host** using the
`windows_service` deployment pattern (DESIGN §9.2).

`labdeploy_windows_service` is a strongly-typed alternative to the generic
[`labdeploy_deployment`](./deployment.md) resource: instead of an opaque
YAML/JSON `spec` string, every field of the Windows service pattern is a
first-class Terraform attribute. The resource projects those attributes onto the
canonical `Deployment` document and drives the same single-target deployment
state machine (rollout, automatic rollback-on-failure, drift detection, and
destroy — DESIGN §10.2).

Use `labdeploy_deployment` instead when you need a multi-host WSFC cluster, a
non-Windows pattern, or you already keep specs as YAML files.

## Example Usage

```hcl
provider "labdeploy" {
  default_target {
    transport                  = "winrm"
    username                   = "labadmin"
    password_env               = "LABDEPLOY_PASSWORD" # env var NAME on the runner
    winrm_use_https            = true
    winrm_insecure_skip_verify = true
  }
}

resource "labdeploy_windows_service" "payments" {
  name         = "payments-svc"
  host         = "lab-node-01"
  service_name = "PaymentsSvc"
  display_name = "Contoso Payments Service"
  description  = "Handles payment intents"

  exe        = "bin\\Payments.Host.exe"
  args       = ["--config", "appsettings.Production.json"]
  start_type = "auto"

  install_root         = "C:\\deploy"
  stop_timeout_seconds = 30

  account {
    username     = "CONTOSO\\svc-payments"
    password_env = "PAYMENTS_SVC_PASSWORD"
  }

  recovery {
    restart_on_failure = true
  }

  environment = {
    ASPNETCORE_ENVIRONMENT = "Production"
  }

  artifact {
    type     = "zip"
    version  = "1.4.2"
    checksum = "sha256:<64-hex>"
    source {
      type = "http"
      url  = "https://artifacts.example/payments-1.4.2.zip"
    }
  }

  # The pipeline's rollout / rollback lever.
  version_override = var.build_version
}
```

## WinSW wrapper

Set `wrapper = "winsw"` and point `winsw_exe` at the WinSW binary inside the
release to register the service through a WinSW `.winsw.xml` instead of native
`sc.exe`:

```hcl
resource "labdeploy_windows_service" "worker" {
  name         = "worker-svc"
  host         = "lab-node-01"
  service_name = "WorkerSvc"
  exe          = "bin\\Worker.exe"
  wrapper      = "winsw"
  winsw_exe    = "tools\\WinSW.exe"

  artifact {
    type     = "zip"
    version  = "2.0.0"
    checksum = "sha256:<64-hex>"
    source {
      type = "http"
      url  = "https://artifacts.example/worker-2.0.0.zip"
    }
  }
}
```

## Argument Reference

### Identity & connection

- `name` (String, **Required**, **ForceNew**) — Logical deployment name
  (`metadata.name`).
- `host` (String, **Required**, **ForceNew**) — The single target host.
- `transport` (String, Optional) — `winrm` | `ssh` | `local`. **Precedence:** this
  attribute > provider `default_target.transport` > `winrm` fallback. Leaving it
  unset lets `default_target.transport` supply `ssh`/`local`; the `winrm` fallback
  only applies when neither is set.
- `port` (Number, Optional) — `0` ⇒ transport default (winrm-https `5986`,
  winrm-http `5985`, ssh `22`).
- `username` (String, Optional) — Connection username.
- `password_env` (String, Optional) — **Env var NAME** on the runner holding the
  connection password (never the secret itself). Required for `winrm`.
- `winrm_use_https` (Bool, Optional) — Use WinRM over HTTPS (default `true`).
- `winrm_insecure_skip_verify` (Bool, Optional) — Skip TLS verification for lab
  certificates.

All connection fields fall back to the provider `default_target` block when
unset (DESIGN §6.2 merge-then-validate).

### Service pattern

- `service_name` (String, **Required**, **ForceNew**) — Windows service name.
- `display_name` (String, Optional) — Defaults to `service_name`.
- `description` (String, Optional).
- `exe` (String, **Required**) — Service executable, relative to the current
  release (or absolute).
- `args` (List of String, Optional) — Executable arguments.
- `start_type` (String, Optional) — `auto` (default) | `manual` | `delayed`.
- `wrapper` (String, Optional) — `none` (native `sc.exe`, default) | `winsw`.
- `winsw_exe` (String, Optional) — WinSW wrapper exe (relative to the release),
  required when `wrapper = "winsw"`.
- `install_root` (String, **ForceNew**, Optional) — Root install dir on the
  target (default `C:\deploy`).
- `stop_timeout_seconds` (Number, Optional) — Graceful stop budget before
  force-kill (default `30`).
- `post_install` (String, Optional) — PowerShell run in the release dir after
  extract, before configure.
- `account` (Block, Optional) — Non-builtin service account (defaults to
  `LocalSystem`):
  - `username` (String, Optional).
  - `password_env` (String, Optional) — **Env var NAME** holding the account
    password.
- `recovery` (Block, Optional):
  - `restart_on_failure` (Bool, Optional) — Register SCM restart actions
    (default `true`).

### Artifact & lifecycle

- `artifact` (Block, **Required**) — DESIGN §6.3:
  - `type` (String, **Required**) — `zip` | `nupkg`.
  - `version` (String, **Required**).
  - `checksum` (String, Optional) — `sha256:<hex>`; required for `zip`.
  - `fetch_mode` (String, Optional) — `target_pull` | `runner_push`.
  - `source` (Block, **Required**):
    - `type` (String, **Required**) — `http` | `file` | `nuget_feed`.
    - `url` (String, Optional) — required for `http`.
    - `path` (String, Optional) — required for `file`.
    - `feed_url` / `package_id` (String, Optional) — required for `nuget_feed`.
- `environment` (Map of String, Optional) — Service environment written to
  `HKLM\SYSTEM\CurrentControlSet\Services\<svc>\Environment` (`REG_MULTI_SZ`).
- `version_override` (String, Optional) — Overrides `artifact.version` — the
  pipeline's rollout/rollback lever.
- `destroy_mode` (String, Optional) — `purge` (default) | `unregister` |
  `abandon` (DESIGN §10.5).

## Attribute Reference

- `id` (String) — `<name>@<host>`.
- `os` (String) — Always `windows`.
- `spec_hash` (String) — sha256 of the canonical projected `Deployment` (after
  `version_override`). Comparable to the `labdeploy_deployment` hash for the same
  effective spec.
- `deployed_version` (String) — Active release version after apply / refresh.
- `previous_version` (String) — Prior version (`""` when none).
- `release_path` (String) — Remote path of the current release.
- `service_status` (String) — `running` | `stopped` | `not_installed`.

## Immutability

Changing `name`, `host`, `service_name`, or `install_root` forces resource
replacement (destroy + create) — these define the service's identity and layout
on the box and cannot be an in-place upgrade (DESIGN §5.2). All other attribute
changes are applied in place as an upgrade of the running service.

## Import

Not supported in v1: the typed attributes cannot be reconstructed from the
target manifest alone. Re-apply the configuration instead (DESIGN §5.2).
