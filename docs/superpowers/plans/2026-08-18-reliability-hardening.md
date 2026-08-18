# Docker Sandbox Reliability Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development for every behavior change. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Raise the Windows Docker Desktop single-host orchestrator from functional to reliable internal operation for 50–300 instances.

**Architecture:** Keep `sandbox` as the Docker-independent orchestration layer and `dockerx` as the default plugin. Add per-instance serialization and explicit compensation inside `sandbox.Service`; preserve Docker labels as the restart-persistent registry while making partial states and retry decisions explicit. Add observability through an optional injected interface so existing callers remain source-compatible.

**Tech Stack:** Go 1.24.2, Docker Engine SDK 28.3.3, GitHub Actions on Windows.

## Global Constraints

- Keep Go at 1.24.2 and use `GOTOOLCHAIN=local`.
- Do not add a database, HTTP/gRPC control plane, Kubernetes, or multi-host scheduling.
- Do not start the full application or require a live Docker daemon in the default test suite.
- Preserve stable `instance_id`; Docker container IDs and host ports remain diagnostic and may change after restore.
- Preserve `sandboxerr.Kind` compatibility and `%w` error chains.
- New behavior must be covered by a failing test before production code changes.
- Do not commit feature changes unless the user explicitly asks.

---

### Task 1: Transactional provisioning and per-instance serialization

**Files:**
- Modify: `sandbox/orchestrator.go`
- Modify: `sandbox/plugin.go`
- Modify: `sandbox/mem_test.go`
- Test: `sandbox/orchestrator_test.go`

**Interfaces:**
- Add an internal keyed lock owned by `Service`; all mutating operations for one `instance_id` serialize.
- Extend `Volume` and the in-memory storage fake with enough information to distinguish a newly created volume from a reused volume.
- Preserve existing public lifecycle method signatures.

- [ ] Add a concurrent Create test proving only one request can provision a given `instance_id`.
- [ ] Run the focused test and confirm it fails because both calls enter provision.
- [ ] Add a failure-injection test where `Runtime.Create` fails after network/volume acquisition; assert newly created resources are compensated and a later Create can retry.
- [ ] Run the focused test and confirm it fails because the volume remains visible as destroyed.
- [ ] Implement keyed locking and a provision compensation stack. Compensate only resources created by this call; never purge a pre-existing workspace.
- [ ] Run `go test -mod=mod ./sandbox/` and confirm the focused and existing lifecycle tests pass.

### Task 2: Restore/reset and destructive lifecycle consistency

**Files:**
- Modify: `sandbox/orchestrator.go`
- Modify: `sandbox/types.go`
- Modify: `sandbox/mem_test.go`
- Test: `sandbox/orchestrator_test.go`

**Interfaces:**
- Keep `RestoreOptions.KeepVolume` for compatibility and add explicit `ResetVolume`; zero-value options preserve data.
- Make reset ordering: load spec → stop/remove old container → remove old volume → provision.
- Return joined cleanup errors while still attempting independent cleanup stages.

- [ ] Add a test proving reset removes the container before attempting volume deletion.
- [ ] Add tests for Stop failure plus successful forced remove, and storage failure plus attempted network release.
- [ ] Verify tests fail against the current ignored-error implementation.
- [ ] Reorder Restore and use `errors.Join` for independent cleanup failures without hiding the primary lifecycle error.
- [ ] Run `go test -mod=mod ./sandbox/`.

### Task 3: Probe validation and retryable health semantics

**Files:**
- Modify: `sandbox/health.go`
- Modify: `sandbox/orchestrator.go`
- Test: `sandbox/orchestrator_test.go`
- Create: `sandbox/health_test.go`

**Interfaces:**
- Add `ValidateProbe(inst Instance, probe Probe) error`.
- Add an internal not-ready marker that distinguishes transient probe results from invalid configuration.
- Invalid kind, missing port, or empty exec command fail immediately as `KindInvalid`.
- Connection failures, non-2xx HTTP responses, and non-zero exec exits remain retryable until context completion.

- [ ] Add tests for invalid kind, missing host port, and empty exec command returning immediately.
- [ ] Add tests proving connection refusal/non-2xx/non-zero exec continue until success or timeout.
- [ ] Verify the invalid cases currently time out or loop.
- [ ] Implement validation and retryability classification.
- [ ] Run `go test -mod=mod ./sandbox/ ./dockerx/`.

### Task 4: Error fidelity, spec persistence, and adapter contract

**Files:**
- Modify: `sandboxerr/errors.go`
- Modify: `sandbox/spec.go`
- Modify: `sandbox/orchestrator.go`
- Modify: `dockerx/orchestrate.go`
- Modify: `dockerx/workspace.go`
- Modify: `cmd/sandboxctl/main.go`
- Test: `sandboxerr/errors_test.go`
- Test: `sandbox/orchestrator_test.go`
- Test: `dockerx/orchestrate_test.go`
- Test: `dockerx/workspace_test.go`
- Create: `cmd/sandboxctl/main_test.go`

**Interfaces:**
- `os.ErrNotExist` is not globally equivalent to Docker unavailable.
- Add an error-returning spec encoder while retaining `EncodeSpec` as a compatibility wrapper if required.
- Enforce a documented compact-spec size ceiling before Docker API calls.
- Ensure `runtimeAdapter` either forwards `RuntimeCreate.Binds/Network` or explicitly rejects contradictory values; no silent loss.
- CLI local validation returns `KindInvalid`; missing local files return `KindNotFound`.

- [ ] Add classification tests for local missing files.
- [ ] Add oversized-spec tests that fail before runtime creation.
- [ ] Add adapter tests proving bind/network intent reaches `CreateOptions`.
- [ ] Add CLI parsing/error-kind tests without invoking `os.Exit`.
- [ ] Implement the minimal compatible changes and preserve `%w`.
- [ ] Run `go test -mod=mod ./...`.

### Task 5: Port conflict retry, pull cancellation, observability, and quotas

**Files:**
- Modify: `dockerx/pullgate.go`
- Modify: `dockerx/container.go`
- Modify: `dockerx/usernet.go`
- Modify: `dockerx/portpool.go`
- Modify: `sandbox/types.go`
- Modify: `sandbox/orchestrator.go`
- Test: `dockerx/lifecycle_test.go`
- Test: `dockerx/usernet_test.go`
- Test: `sandbox/orchestrator_test.go`

**Interfaces:**
- `pullGate.Do` accepts caller context; waiters can cancel independently.
- Port binding conflicts trigger bounded lease release/reallocation with no leaked port.
- Add optional `Observer` callbacks for operation start/end, stage, duration, error kind, and compensation result.
- Add configurable non-zero default Memory/NanoCPUs at the orchestrator boundary; explicit values still win.

- [ ] Add a test proving a pull waiter exits on its own canceled context.
- [ ] Add a test proving one synthetic Docker port-bind conflict releases the first lease and retries with a different port.
- [ ] Add observer event-order and default-quota tests.
- [ ] Verify each test fails for the intended missing behavior.
- [ ] Implement bounded retry, context-aware pull waiting, observer no-op default, and configurable defaults.
- [ ] Run `go test -race -mod=mod ./...`.

### Task 6: CI, documentation, and operational gates

**Files:**
- Create: `.github/workflows/ci.yml`
- Modify: `README.md`
- Create: `docs/operations.md`
- Modify: `docs/superpowers/specs/2026-08-17-sandbox-orchestrator-design.md`

**Interfaces:**
- CI runs on `windows-latest` with Go 1.24.2.
- Required checks: `gofmt`, `go vet`, unit tests, race tests, and coverage collection.
- Live Docker smoke remains opt-in and is not a default pull-request blocker.
- Document capacity guardrails: design peak 300 instances, daily active target 200, port-pool alerts at 70% and admission control at 85%.

- [ ] Add CI workflow with least permissions (`contents: read`) and dependency caching.
- [ ] Document state transitions, retryability, reset data loss, Docker Desktop prerequisites, capacity limits, metrics, and recovery drills.
- [ ] Run local equivalents of every non-live CI command.
- [ ] Confirm workflow YAML, docs links, and commands are internally consistent.

### Task 7: Final verification and review

**Files:**
- Review all files changed by Tasks 1–6.

- [ ] Run `gofmt -l` and require no output.
- [ ] Run `go vet -mod=mod ./...`.
- [ ] Run `go test -count=1 -mod=mod ./...`.
- [ ] Run `go test -race -count=1 -mod=mod ./...`.
- [ ] Collect package coverage and compare with the pre-change baseline.
- [ ] Inspect `git diff --check` and the full branch diff.
- [ ] Perform an independent code review and fix all critical/important findings.
