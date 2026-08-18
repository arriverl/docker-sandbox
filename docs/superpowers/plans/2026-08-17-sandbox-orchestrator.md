# Sandbox Orchestrator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a pluggable `sandbox` orchestrator so the platform manages instances by stable `instance_id` (create/start/stop/destroy/restore/wait/list), with Docker as the default plugin implementation.

**Architecture:** `sandbox` holds types, plugin interfaces, and Orchestrator. It does not import `dockerx`. `dockerx` implements the five plugins and `NewOrchestrator`. CLI uses Orchestrator when `-instance` is present.

**Tech Stack:** Go 1.24.2, existing Docker SDK, `sandboxerr`.

## Global Constraints

- `GOTOOLCHAIN=local`, `go test/run -mod=mod`; do not run unconstrained `go mod tidy`; do not upgrade to Go 1.25.
- Chinese errors via `sandboxerr` with `实例=` / `镜像=` / `状态=` / `用户=`.
- Do not commit unless the user asks.
- Fake engine in `client_test.go` must keep implementing `EngineAPI` if new methods are added.

---

### Task 1: sandbox types, plugins, orchestrator + fake tests

**Files:**
- Create: `sandbox/types.go`, `sandbox/plugin.go`, `sandbox/name.go`, `sandbox/spec.go`, `sandbox/errors.go`, `sandbox/orchestrator.go`
- Test: `sandbox/orchestrator_test.go`

**Interfaces:**
- Consumes: `sandboxerr`
- Produces: `sandbox.Service` with Create/Start/Stop/Destroy/Restore/WaitReady/Get/List

- [ ] Write failing tests for: Create validation/conflict, Start/Stop idempotent, Destroy keeps volume, Restore keeps instance_id+name, WaitReady success/timeout
- [ ] Implement Orchestrator against injected plugins
- [ ] Run: `$env:GOTOOLCHAIN='local'; go test -mod=mod ./sandbox/`

### Task 2: dockerx plugin adapters + volume spec labels

**Files:**
- Modify: `dockerx/workspace.go` (EnsureLabels)
- Create: `dockerx/orchestrate.go` (adapters + NewOrchestrator)
- Test: `dockerx/orchestrate_test.go`

- [ ] Persist compact spec on workspace volume labels
- [ ] Runtime/Network/Storage/Registry/Health adapters
- [ ] Run: `$env:GOTOOLCHAIN='local'; go test -mod=mod ./dockerx/ ./sandbox/`

### Task 3: CLI instance-first commands

**Files:**
- Modify: `cmd/sandboxctl/main.go`

- [ ] `create -instance -user -image [-memory] [-cpus] [-expose]` → Orchestrator
- [ ] `start/stop/destroy/restore/inspect/ps/wait` by instance_id with container-ID fallback
- [ ] Run: `$env:GOTOOLCHAIN='local'; go test -mod=mod ./...`

---
