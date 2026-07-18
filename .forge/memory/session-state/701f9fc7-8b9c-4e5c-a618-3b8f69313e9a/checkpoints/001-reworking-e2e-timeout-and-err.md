<overview>
Generating godog (Cucumber-for-Go) E2E acceptance test artifacts for the `deployment-resource-crud-and-plan-modifiers` stage of a Terraform provider (`github.com/smartpcr/terraform-provider-labdeploy`). The suite covers 6 acceptance scenarios: immutable-path RequiresReplace, host-reorder no-op, coded ERR_CONNECT diagnostic, deterministic SHA-1 id, timeout defaults, and import-unsupported. Iteration 2 scored 82/iterate — the evaluator requires TWO fixes: (1) timeout defaults must be proven through the real framework schema/CRUD path, not an E2E-only constant wrapper; (2) ERR_CONNECT must originate from `transport.ErrConnect` through the real taxonomy path, not a fabricated `engine.CodedError`.
</overview>

<history>
1. Initial task: produce godog E2E artifacts for the workstream.
   - Located the Go module at repo root (`go.mod` already had `github.com/cucumber/godog v0.15.1` — no dependency changes needed).
   - Studied the impl under `internal/provider/deployment_resource.go` and its in-package tests, plus a sibling e2e test for conventions.
   - Created 3 files: the `.feature` file, the `_test.go` step-definitions, and `internal/provider/e2e_seam.go` (an `//go:build e2e` seam).
   - Fixed a console_app JSON round-trip issue by switching the prior-state to verbatim inline `spec` instead of a marshaled `resolved_spec`.
   - All 9 scenarios passed; committed as `[e2e] Deployment Resource CRUD and Plan Modifiers — E2E` (commit 7213257).
   - Also fixed CRLF→LF line endings and an `os.Setenv` errcheck violation to satisfy the repo's `.golangci.yml` lint gate.
   - Replied DONE.

2. Iteration 3 feedback (score 82, iterate): the evaluator flagged two specific weaknesses that "do not fully prove the required acceptance behavior":
   - Timeout defaults asserted through an E2E-only constant wrapper (`DefaultTimeoutsForE2E()`) rather than reading the framework schema.
   - ERR_CONNECT directly fabricated as `engine.CodedError` instead of originating from `transport.ErrConnect` through the real taxonomy path.
   - Began investigating the real error-taxonomy path and engine transport seam to implement genuine fixes. (This investigation was in progress when compaction occurred.)
</history>

<work_done>
Files created (committed in 7213257):
- `test/e2e/release-RELEASE-PROVIDER/terraform_provider_surface_deployment_resource_crud_and_plan_modifiers.feature` — Gherkin with all 6 scenarios, tagged `@story-release:RELEASE-PROVIDER @phase-terraform-provider-surface @stage-deployment-resource-crud-and-plan-modifiers @setup-inline`.
- `test/e2e/release-RELEASE-PROVIDER/terraform_provider_surface_deployment_resource_crud_and_plan_modifiers_test.go` — `//go:build e2e`, `package e2e`, initializer `InitializeScenario_terraform_provider_surface_deployment_resource_crud_and_plan_modifiers`, entrypoint `TestE2E_terraform_provider_surface_deployment_resource_crud_and_plan_modifiers`.
- `internal/provider/e2e_seam.go` — `//go:build e2e` seam exposing `DeploymentIDForE2E`, `DefaultTimeoutsForE2E`, and `NewDeploymentResourceWithConnectError` (currently uses a fabricated `engine.CodedError` — needs rework).

Work completed:
- [x] All 6 scenarios written and passing (9 scenario instances incl. Scenario Outline examples, 33 steps).
- [x] Lint-clean (LF endings, gofmt, errcheck) for my files.
- [x] Committed.

Work IN PROGRESS (iteration 3 — the two evaluator fixes, NOT yet applied):
- [ ] Fix #1: Rework timeout-defaults proof to exercise the real Create/Update/Delete CRUD path and observe the applied context deadline (~30m/30m/15m), instead of `DefaultTimeoutsForE2E()` echoing constants.
- [ ] Fix #2: Rework ERR_CONNECT to inject a fake `transport.Transport` whose `Connect` returns `transport.ErrConnect(...)`, drive the REAL `engine.Update`/`Deploy`, and let `wrapTransportErr` produce the `engine.CodedError{Code:"ERR_CONNECT"}` naturally.
</work_done>

<technical_details>
- **Real ERR_CONNECT taxonomy path** (verified): `engine.deploySingle` (engine.go:175) calls `e.NewTransport(...)`, then `t.Connect(ctx)` (line 190). On failure → `wrapTransportErr(err, host, "CONNECT")` (line 192). `wrapTransportErr` (engine.go:1230): `if ce, ok := err.(*transport.CodedError); ok { return coded(ce.Code, host, step, ce.Err) }` else `coded("ERR_CONNECT", ...)`. So `transport.ErrConnect(dialErr)` (a `*transport.CodedError{Code:"ERR_CONNECT"}`, transport.go:63) becomes `engine.CodedError{Code:"ERR_CONNECT", Step:"CONNECT"}`. Note: scenario says "PREFLIGHT" but the real dial failure classifies at CONNECT step — the CODE is still ERR_CONNECT, which is what the assertion checks (`begins "[ERR_CONNECT] "`).
- **Engine transport seam**: `engine.Engine` has a public swappable field `NewTransport func(t *spec.Target, host string) (transport.Transport, error)` (engine.go:37-38). `engine.New()` sets it to `transport.NewTransport` (engine.go:42). In-package/engine tests swap it to inject fakes. This is the honest injection point.
- **Resource engine seam**: `DeploymentResource` has unexported `newEngine func() deployEngine` (deployment_resource.go:55). When set, `r.engine()` returns it; else `realEngine{engine.New()}`. `realEngine` wraps `*engine.Engine` (deployment_resource.go:68) with `Warns()` returning `Engine.Warnings`. The `deployEngine` interface: `Update`, `ReadStatus`, `Destroy`, `Warns`.
- **Planned fix #2 approach**: In `e2e_seam.go`, build `eng := engine.New(); eng.NewTransport = func(...) { return &fakeConnectFailTransport{}, nil }`, wrap in `realEngine{eng}`, inject via `newEngine`. The fake transport's `Connect` returns `transport.ErrConnect(fmt.Errorf("dial tcp ...: connection refused"))`. `deploySingle` order: NewTransport → `pattern.For(windows_service)` (ok) → `t.Connect` fails first. So the fake only critically needs `Connect` (+ satisfy the full `transport.Transport` interface).
- **`transport.Transport` interface** (transport.go:44-52): `Connect(ctx) error`, `Close() error`, `OS() spec.OSKind`, `Host() string`, `Exec(ctx, Cmd) (Result, error)`, `Upload(ctx, io.Reader, int64, string) error`, `Download(ctx, remote, local string) error`. Must implement all; only `Connect` needs meaningful behavior (others can return zero/nil).
- **Planned fix #1 approach**: A fake `deployEngine` whose `Update`/`Destroy` capture `ctx.Deadline()`; drive real `Create`/`Update`/`Delete` (built from the real schema, with null timeouts) and assert deadline−now ≈ 30m/30m/15m within tolerance. This exercises `plan.Timeouts.Create(ctx, defaultCreateTimeout)` (deployment_resource.go:469) etc. applying the constant through the real CRUD + `context.WithTimeout`. Requires a fake engine returning a valid `engine.Status` (fields: DeployedVersion, PreviousVersion, ReleasePath, ServiceStatus, Hosts — a `&engine.Status{}` with nil Hosts works via `fillStatus`).
- **Timeout defaults** (deployment_resource.go:33-35): `defaultCreateTimeout = 30*time.Minute`, `defaultUpdateTimeout = 30*time.Minute`, `defaultDeleteTimeout = 15*time.Minute`. Schema declares a `timeouts` block (Create/Update/Delete: true) at deployment_resource.go:146; defaults are NOT stored in the schema — applied at call time by CRUD methods. Hence a pure schema read cannot yield 30m/15m; behavioral proof (observed deadline) is the honest route.
- **Deterministic id** (deployment_resource.go:763 `deploymentID`): `sha1(strings.Join(sorted(lower(hosts)),",") + "/" + name)`, hex[:12] + ":" + name. Already proven correctly via `DeploymentIDForE2E` seam — evaluator did NOT flag this; keep it.
- **immutableKey** (deployment_resource.go:300): joins pattern.Type, ServiceName, RoleName, EffectiveInstallRoot(OS), Metadata.Name, sorted-lower hosts, OS with "|". ModifyPlan compares old vs new immutableKey (line 423) → RequiresReplace. Already proven correctly; not flagged.
- **ModifyPlan prior reconstruction**: `priorFromState` (deployment_resource.go:528) reads `resolved_spec` JSON, else falls back to inline `spec` (verbatim, "verified"), else returns nil for legacy spec_file. My test uses verbatim inline `spec` in prior state to avoid a console_app JSON strict-decode issue (`json: unknown field "account"` — the flat Pattern struct marshals empty `ServiceAccount{}` as `"account":{}` which the strict decoder rejects on re-parse of console_app specs).
- **Lint gate** (`.golangci.yml`): `version: "2"`, `build-tags: [e2e]` (so e2e files ARE linted), enables errcheck/govet/ineffassign/misspell/staticcheck/unconvert/unused, formatters gofmt+goimports. NO `os.Setenv` exclusion (so must check its error). The `Lint clean` sibling e2e scenario runs `golangci-lint run` from module root. My local golangci-lint 2.12.2/go1.26 flags many PRE-EXISTING sibling files (toolchain-version artifact) — those are NOT my concern; the gate uses a matching toolchain. My files must be clean under standard gofmt/errcheck (they are).
- **Environment**: Windows, PowerShell. `create` tool writes CRLF — must convert to LF via `[System.IO.File]::WriteAllText` with `-replace "\r\n","\n"` then `gofmt -w`. go version go1.26.4.
- **Spec fixtures**: mirror in-package helpers (wsSpecYAMLHost, wsSpecServiceYAML, dotnetSpecYAML, consoleSpecYAML, clusterSpecYAMLHosts). All reference `password_env: LABDEPLOY_PASSWORD` — must `os.Setenv("LABDEPLOY_PASSWORD","pw")` in the scenario Before hook (with error check).
</technical_details>

<important_files>
- `internal/provider/e2e_seam.go` (`//go:build e2e`)
  - The seam bridging the external `e2e` package to unexported provider internals.
  - NEEDS REWORK: currently `NewDeploymentResourceWithConnectError()` injects a fabricated `engine.CodedError` (via `e2eConnectErrEngine`). Must instead inject a real engine with a fake transport returning `transport.ErrConnect(...)`. Also `DefaultTimeoutsForE2E()` (returns raw constants) should be replaced/supplemented by a deadline-capturing fake engine to prove timeouts behaviorally.
  - Keep `DeploymentIDForE2E` (working, not flagged).
- `test/e2e/release-RELEASE-PROVIDER/terraform_provider_surface_deployment_resource_crud_and_plan_modifiers_test.go` (`//go:build e2e`, `package e2e`)
  - Step definitions. Contains fixture helpers (e2eWSSpecYAMLHost etc.), `runPlan` (drives ModifyPlan via real schema), `crudState`, and the 6 scenarios' steps.
  - Timeout step (`whenTimeoutDefaultsRead`/`thenTimeoutDefaults`) and ERR_CONNECT step (`whenCreateSurfacesFailure`) need updating to use the reworked seam.
  - Mirrors `provider.deploymentModel` as `e2eCrudDeploymentModel` (incl. `Timeouts timeouts.Value` field). `e2eNullTimeouts()` helper builds a null timeouts value.
- `test/e2e/release-RELEASE-PROVIDER/terraform_provider_surface_deployment_resource_crud_and_plan_modifiers.feature`
  - The 6 scenarios. The timeout scenario currently reads "When the resource timeout defaults are read" — may need rewording to match a behavioral (CRUD-driven) proof, and the ERR_CONNECT scenario wording is fine.
- `internal/provider/deployment_resource.go` (impl under test — do NOT modify)
  - Key refs: timeouts constants (33-35), schema/timeouts block (116-152), `deployEngine` interface (61-66), `newEngine` seam (55, 72-77), `ModifyPlan` (363-426), `immutableKey` (300), `Create` (463-479) with `plan.Timeouts.Create(ctx, defaultCreateTimeout)` (469), `Delete` timeout (661), `ImportState` (698-701, detail = "import is not supported; adopt via apply"), `diagSummary` (721-733), `deploymentID` (763-771), `apply` (586-600, calls `eng.Update` then `diagSummary(ctx,"ERR_CONNECT",...)`).
- `internal/engine/engine.go` (do NOT modify)
  - `NewTransport` seam (37-42), `Update` (114), `deploySingle` (175, Connect at 190, wrapTransportErr at 192), `wrapTransportErr` (1230-1235), `Status` struct (28), `ReadStatus` (1259).
- `internal/transport/transport.go` (do NOT modify)
  - `Transport` interface (44-52), `CodedError` (54-64), `ErrConnect(err)` (63).
- `internal/engine/manifest.go`: `engine.CodedError{Code,Host,Step,Err}` (21-26).
</important_files>

<next_steps>
Immediate next steps (apply the two evaluator fixes, then verify):

1. **Fix #2 (ERR_CONNECT via real taxonomy)** in `internal/provider/e2e_seam.go`:
   - Add a `type e2eFailConnectTransport struct{}` implementing all 7 `transport.Transport` methods; `Connect(ctx)` returns `transport.ErrConnect(fmt.Errorf("dial tcp 10.0.0.1:5985: connect: connection refused"))`; `OS()` returns `spec.OSWindows`; `Host()` returns "lab-01"; `Exec/Upload/Download/Close` return zero/nil.
   - Change `NewDeploymentResourceWithConnectError()` to build `eng := engine.New(); eng.NewTransport = func(*spec.Target, string) (transport.Transport, error) { return &e2eFailConnectTransport{}, nil }` and inject `newEngine: func() deployEngine { return realEngine{eng} }`. (Requires importing `internal/transport`.)
   - Remove the fabricated `e2eConnectErrEngine`.
   - Verify: the resource's `apply` → `eng.Update` → real `deploySingle` → `Connect` fails → `wrapTransportErr` → `engine.CodedError{Code:ERR_CONNECT}` → `diagSummary` → Summary begins "[ERR_CONNECT] ".

2. **Fix #1 (timeout defaults behaviorally)** in `e2e_seam.go` + test:
   - Add a deadline-capturing fake `deployEngine` (or a seam function `NewDeploymentResourceWithDeadlineCapture()` returning the resource + a pointer to captured durations). Its `Update` records `d, ok := ctx.Deadline(); captured = time.Until(d)` and returns `&engine.Status{}`; `Destroy` records likewise.
   - In the test, drive real `Create` (→ capture ≈30m), `Update` (≈30m), `Delete` (≈15m) with plans/states built from the REAL schema and null timeouts; assert each captured duration is within a tolerance (e.g. 29m–30m for 30m, 14m–15m for 15m) since some time elapses.
   - Remove `DefaultTimeoutsForE2E()` if fully replaced, and update the feature wording if needed.

3. **Verify**:
   - Convert any edited/new files to LF, `gofmt -w`, ensure errcheck-clean.
   - Run `go build ./...` (production, no e2e tag) to confirm the seam doesn't leak.
   - Run `go test -tags e2e ./test/e2e/release-RELEASE-PROVIDER/ -run TestE2E_terraform_provider_surface_deployment_resource_crud_and_plan_modifiers -v` — all 9 scenarios must pass.
   - Commit with the `Co-authored-by: Copilot <223556219+Copilot@users.noreply.github.com>` trailer.

4. **In the response**, include a `### Prior feedback resolution` block marking BOTH numbered evaluator items as ADDRESSED with file/section refs (mandatory per the iteration-3 process). Do NOT emit `DONE` or `[Iteration N/20]` markers (Forge runs the outer loop; one turn per iteration).

Note: Delete's engine call path — verify `Delete` uses `eng.Destroy` with the timeout-wrapped ctx (deployment_resource.go:661 `state.Timeouts.Delete(ctx, defaultDeleteTimeout)`); confirm the Delete flow reaches `eng.Destroy` so the deadline capture works (may need a valid prior state/resolved_spec so Delete doesn't short-circuit).
</next_steps>