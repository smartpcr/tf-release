# terraform-provider-labdeploy

Custom Terraform provider that deploys build artifacts (nupkg / zip / docker
images) onto Windows and Linux lab machines over WinRM / SSH, runs E2E test
packages against them, and collects logs + test results back to the CI runner.
Driven entirely from GitHub Actions or Azure DevOps pipelines.

**[`docs/DESIGN.md`](docs/DESIGN.md) is the normative spec** — schemas, state
machines (windows service S0–S7, cluster C/U/R steps, docker D-steps), error
codes, locking, rollback semantics, the decision table (D1–D15), and the
~50-scenario E2E acceptance matrix. Code comments cite it as `DESIGN §x.y`.

## Resources

| Resource | Purpose |
|---|---|
| `labdeploy_deployment` | One app on one host or one WSFC cluster. Create/Update = deploy with health-gated switchover + automatic rollback. Delete honors `destroy_mode` (purge / unregister / abandon). |
| `labdeploy_e2e_test` | Runs a test package (vstest / dotnet test / npm / exec) on the target once per `triggers` change; always collects results + logs to the runner before pass/fail gating. |

Supported patterns: `console_app`, `windows_service`, `node_web_app`,
`dotnet_api`, `cluster_generic_service` (rolling passive-first update via
`Move-ClusterGroup`), `docker_container`.

## Build & install (local mirror)

```sh
go mod tidy        # regenerates go.sum
make install       # builds and copies into ~/.terraform.d/plugins/registry.local/smartpcr/labdeploy/0.1.0/<os_arch>/
```

Terraform resolves `registry.local/smartpcr/labdeploy` from the implied local
mirror automatically; no `.terraformrc` needed when using
`~/.terraform.d/plugins`. For a shared mirror path use:

```hcl
# ~/.terraformrc
provider_installation {
  filesystem_mirror { path = "/opt/tf-mirror" }
  direct { exclude = ["registry.local/*/*"] }
}
```

## Release & distribution to runners

Tagged builds (`git push --tags`, `v*`) run [`.goreleaser.yml`](.goreleaser.yml)
via the [`release` workflow](.github/workflows/release.yml). GoReleaser produces
statically linked (`CGO_ENABLED=0`) provider binaries and packages them as:

```
terraform-provider-labdeploy_v<version>_linux_amd64.zip
terraform-provider-labdeploy_v<version>_windows_amd64.zip
```

Each zip contains the versioned binary (`terraform-provider-labdeploy_v<version>`
on linux, `terraform-provider-labdeploy_v<version>.exe` on windows) plus a
`terraform-provider-labdeploy_v<version>_SHA256SUMS` checksum file. Reproduce
the matrix locally with `goreleaser build --snapshot --clean`.

To install on a CI runner, download and unzip the matching artifact into the
filesystem-mirror layout Terraform expects (DESIGN §16.1):

```
# Linux runner
$HOME/.terraform.d/plugins/registry.local/smartpcr/labdeploy/<version>/linux_amd64/terraform-provider-labdeploy_v<version>

# Windows runner
%APPDATA%\terraform.d\plugins\registry.local\smartpcr\labdeploy\<version>\windows_amd64\terraform-provider-labdeploy_v<version>.exe
```

Point Terraform at the mirror via `~/.terraformrc` and pin the source in the
config's `required_providers`:

```hcl
# ~/.terraformrc  (Linux runner shown; use %APPDATA%\terraform.rc on Windows)
provider_installation {
  filesystem_mirror { path = "/home/runner/.terraform.d/plugins" include = ["registry.local/*/*"] }
  direct { exclude = ["registry.local/*/*"] }
}
```

```hcl
terraform {
  required_providers {
    labdeploy = { source = "registry.local/smartpcr/labdeploy", version = "0.1.0" }
  }
}
```

## Quick start

```sh
export LABDEPLOY_PASSWORD='...'   # secrets are env vars; specs reference NAMES only
export NUGET_PAT='...'
cd examples
terraform init
terraform apply -var app_version=1.2.3 -var app_checksum=sha256:...
```

`examples/` contains a root module wiring deployment + e2e test, one spec per
pattern under `specs/`, and ready-to-adapt pipelines under `pipelines/`
(GitHub Actions + ADO, both with rollback-to-last-known-good stages).

Rollout / rollback from a pipeline is just `terraform apply` with a different
`-var app_version=...` — the provider stops the service, repoints the
`current` junction/symlink, reconfigures, restarts, health-checks, and rolls
back automatically on failure (DESIGN §10.3).

## Layout on the target

```
<install_root>\<app>\
  releases\<version>\   # immutable, cached by checksum marker
  current               # junction (Windows) / symlink (Linux) -> active release
  shared\logs\          # survives releases
  staging\              # transient fetch area
  manifest.json         # target-side source of truth
  .lock                 # concurrency guard (exit 48 on contention)
```

## Development

```sh
make build   # bin/terraform-provider-labdeploy_v0.1.0
make test    # unit tests incl. fake-transport engine state-machine suite
go vet ./...
```

`internal/engine/engine_test.go` contains a script-dispatching fake Windows
host used to verify step ordering, idempotency, lock contention, health-failure
rollback, fresh-install cleanup, and purge semantics without real targets.
