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
