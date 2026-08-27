# labdeploy_windows_service (not a supported public resource)

> **This resource is not part of the supported public provider surface.**
>
> The v1 `terraform-provider-labdeploy` contract authorizes exactly two
> resources (DESIGN §5.2-5.3, architecture.md §1): [`labdeploy_deployment`](./deployment.md)
> and `labdeploy_e2e_test`. Stage 4.2 of the implementation plan is scoped to the
> `windows_service` **pattern-layer** work (`internal/pattern`) plus goldens — not a
> new public Terraform resource.
>
> The typed `WindowsServiceResource` code is retained in the codebase as an
> additive convenience projection over `labdeploy_deployment`, but it is **not
> registered** on the provider and is therefore not usable from Terraform
> configuration. It is intentionally undocumented as a public resource.

## Deploy a Windows service

Use the generic [`labdeploy_deployment`](./deployment.md) resource with a
`windows_service` pattern spec. It drives the same single-target deployment state
machine (rollout, automatic rollback-on-failure, drift detection, and destroy —
DESIGN §10.2) via the `windows_service` pattern implemented in `internal/pattern`.

```hcl
resource "labdeploy_deployment" "payments" {
  spec = file("${path.module}/payments.windows_service.yaml")
}
```

See [`labdeploy_deployment`](./deployment.md) for the full schema, the
`windows_service` pattern fields, and immutability / lifecycle semantics.
