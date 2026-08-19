# docker-sandbox

单机 Docker Engine 上的 **容器沙箱编排内核**（Go 库）和命令行工具 `sandboxctl`。

它给在线实训、代码沙箱、隔离执行环境提供可嵌入的基础能力：用稳定的 `instance_id` 管理容器生命周期，而不是把 Docker 容器 ID 当作平台主键。

当前基线：Windows Docker Desktop 或 Linux Docker Engine；设计峰值 **300** 个活跃实例。它不是 Kubernetes，也不是多主机调度器。鉴权、租户配额、作业队列、HTTP/gRPC API 应由上层平台提供。

| 项 | 值 |
|---|---|
| 模块 | `github.com/gitproduct/docker-sandbox` |
| Go | 1.24.2（`GOTOOLCHAIN=local`） |
| Docker SDK | `github.com/docker/docker` v28.3.3 |
| 默认前缀 | `sandbox` |
| 默认配额 | 256 MiB 内存、0.5 CPU |
| 端口池 | TCP `20000–20499`，共 500 个 |
| Workspace 挂载 | `/workspace` |

- [系统定位](#系统定位)
- [系统框架](#系统框架)
- [身份与资源命名](#身份与资源命名)
- [代码结构](#代码结构)
- [组成方式](#组成方式)
- [生命周期](#生命周期)
- [利用点](#利用点)
- [命令行](#命令行)
- [构建、测试与 CI](#构建测试与-ci)
- [生产边界](#生产边界)

---

## 系统定位

### 解决什么问题

实训平台、OJ、在线 IDE 需要在一台 Docker 主机上为每个学员开隔离环境。平台侧必须能：

1. 用自己的业务 ID（课程实例、作业会话）引用沙箱，重启 Docker 后 ID 不变。
2. 按用户隔离网络，按实例保留工作区文件。
3. 限制 CPU/内存，发布宿主机端口，等待服务就绪。
4. 创建失败可重试，销毁后可还原，进程崩溃后能从 Docker 标签重建视图。

本仓库提供 **编排内核**，不提供完整产品。上层应调用 `sandbox.Service`，不要直接拼容器、网络、卷。

### 明确不做

- 多主机调度、Kubernetes、服务网格
- HTTP/gRPC 控制面、鉴权、计费、作业队列
- 跨用户的全局防火墙策略（默认同用户容器在同一 bridge 上可互通；互不信任任务用 per-instance 网络）
- UDP 自动端口分配
- 把 Docker 容器 ID 当作稳定主键

---

## 系统框架

三层结构：平台只依赖最上面一层。

```
┌─────────────────────────────────────────────────────────┐
│  上层平台（实训 / OJ / IDE）                              │
│  持有 instance_id，带 deadline 调用 sandbox.Service       │
└────────────────────────────┬────────────────────────────┘
                             │
┌────────────────────────────▼────────────────────────────┐
│  sandbox.Service                                         │
│  身份、串行化、配额、补偿、EnsureRunning、Restore/Destroy  │
│  WaitReady、Observer（含 pull.progress / wait.retry）     │
│  插件：Runtime / Network / Storage / Registry / Health   │
└────────────────────────────┬────────────────────────────┘
                             │
┌────────────────────────────▼────────────────────────────┐
│  dockerx（默认插件实现）                                  │
│  Client → Engine API                                     │
│  UserSandbox（用户网络 + 端口池）                         │
│  Workspace（命名卷）                                      │
│  ContainerManager / ImageBuilder                         │
└────────────────────────────┬────────────────────────────┘
                             │
                    Docker Engine
              （npipe / unix / tcp）
```

### 编排器职责

`sandbox.Service` 只通过 `instance_id` 说话。它负责：

- 校验并压缩 `Spec`（写入 Docker label 的上限 4096 字节）
- 填入默认内存/CPU；可选 `MaxInstances`（只统计非 `destroyed`）
- 同一 `instance_id` 的变更互斥；同一 `user_id` 的 provision 互斥
- 事务化创建：网络 → 卷 → 容器；失败只回滚 **本次新建** 的资源
- Destroy 聚合清理错误；后续清理使用独立超时 context
- Restore 默认保卷；`ResetVolume` 先删容器再删卷
- WaitReady：非法探针立即失败；未就绪可重试直到 context 截止，并发送 `wait.retry`
- 向 `Observer` 发送阶段事件（含补偿、拉取进度和端口水位）
- 可选补偿 journal：补偿失败写入本地 JSONL，进程重启后下次 Create 先重放

### 插件职责

| 插件 | 接口 | 默认实现 | 作用 |
|---|---|---|---|
| Runtime | 容器进程 | `dockerx` `runtimeAdapter` | Create/Start/Stop/Remove/Inspect/Exec |
| Network | 每用户网络 | `UserSandbox` | Ensure/Release；可选 EnsureTracked 供补偿 |
| Storage | 工作区卷 | `Workspace` | Ensure/Inspect/Destroy/List |
| Registry | 实例视图 | 扫容器 + 卷 | Get/List；无容器则 `destroyed` |
| Health | 就绪探测 | `StdHealth` | HTTP / TCP / Exec |

没有数据库。重启后的“注册表”就是 Docker labels。

---

## 身份与资源命名

| 字段 | 是否稳定 | 规则 | 用途 |
|---|---|---|---|
| `instance_id` | 稳定 | 平台主键 | 所有编排 API 的参数 |
| 容器名 | 稳定 | `{prefix}-{sanitize(instance_id)}` | 如 `sandbox-box1` |
| Docker 容器 ID | **可变** | Engine 分配 | 仅诊断；Restore 后会变 |
| 卷名 | 稳定 | `{prefix}-ws-{sanitize(instance_id)}` | 如 `sandbox-ws-box1` |
| 用户网络 | 按用户稳定 | `{prefix}-net-{sanitize(user_id)}` | 如 `sandbox-net-alice` |
| HostPort | **可变** | 端口池动态分配 | Restore 后必须重新 `Get` |

`sanitize` 会把非法字符换成 `-`。清洗后可能碰撞的 user id（如 `user@1` 与 `user-1`）会追加短哈希。

### Docker labels（持久化元数据）

| Label | 含义 |
|---|---|
| `sandbox.role` | `instance` 或 `workspace` |
| `sandbox.instance` | instance_id |
| `sandbox.userid` | 用户 |
| `sandbox.network` | 用户网络名 |
| `sandbox.volume` | 工作区卷名 |
| `sandbox.hostports` | 已分配宿主机端口 |
| `sandbox.spec` | 压缩后的 Spec JSON（含 `SkipPull`） |

平台不得缓存容器 ID 和 Restore 前的 HostPort。

---

## 代码结构

```
docker-sandbox/
├── sandbox/                 对外编排 API（平台应只依赖这一包）
│   ├── types.go             Spec / Instance / Probe / Config / 常量
│   ├── plugin.go            Runtime/Network/Storage/Registry/Health
│   ├── orchestrator.go      Service：生命周期 + 补偿 + 配额 + 上限
│   ├── spec.go              Spec 编解码，4096 字节上限
│   ├── health.go            StdHealth、ValidateProbe、ProbeNotReady
│   ├── observer.go          LogObserver（一行 JSON）
│   ├── keylock.go           按 key 串行的引用计数锁
│   ├── name.go              容器名清洗
│   └── errors.go            编排错误中文包装
├── dockerx/                 Docker Engine 客户端 + 默认插件
│   ├── client.go            EngineAPI 最小子集、Ping/List
│   ├── config.go / host.go  npipe/unix/tcp、代理策略
│   ├── container.go         创建/启停/删除/Exec/拉镜像
│   ├── pullgate.go          同镜像拉取 singleflight，等待者可取消
│   ├── usernet.go           每用户网络、端口租约、水位事件
│   ├── portpool.go          500 端口窗口、70%/85% 水位
│   ├── portlock*.go         跨进程文件锁
│   ├── workspace.go         命名卷，默认挂 /workspace
│   ├── orchestrate.go       NewOrchestrator / BindOrchestrator
│   └── imagebuild.go        动态 Dockerfile 构建
├── sandboxerr/              Kind 分类与 %w 错误链
├── cmd/sandboxctl/          CLI：编排主路径 + 旧容器命令
├── docs/operations.md       运维、容量、覆盖率、演练
└── .github/workflows/       Windows 质量门、Ubuntu race、live smoke
```

测试：`*_test.go` 默认不连真实 Docker。`dockerx/live_test.go` 在 `DOCKER_SANDBOX_LIVE=1` 时做连通/创建销毁。

---

## 组成方式

推荐装配顺序：Docker 客户端 → UserSandbox → 编排器。

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func main() {
	client, err := dockerx.New(dockerx.Config{
		Timeout:      2 * time.Minute,
		DisableProxy: true, // Windows npipe 不要走 HTTP 代理
	})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	sb := dockerx.NewUserSandbox(client, dockerx.UserSandboxConfig{
		Prefix:          "sandbox",
		DefaultMemory:   sandbox.RecommendedDefaultMemory,   // 256 MiB
		DefaultNanoCPUs: sandbox.RecommendedDefaultNanoCPUs, // 0.5 CPU
		MaxInstances:    sandbox.DesignPeakInstances,        // 300
		Observer:        sandbox.LogObserver{},              // JSON 打到 stderr
	})
	orch, err := dockerx.BindOrchestrator(client, sb, "sandbox")
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	inst, err := orch.EnsureRunning(ctx, sandbox.Spec{
		InstanceID: "lab-42",
		UserID:     "alice",
		Image:      "alpine:3.20",
		Cmd:        []string{"sleep", "3600"},
		Expose:     []string{"80"},
	}, sandbox.Probe{Kind: sandbox.ProbeHTTP, Path: "/ready"})
	if err != nil {
		log.Printf("kind=%s next=%s err=%v", sandboxerr.Classify(err), sandboxerr.ActionOf(err), err)
		return
	}
	// HostPort 以本次返回值为准，不要缓存 Restore 前的端口
	_ = inst.HostPorts
}
```

最短装配（Windows 建议）：

```go
client, orch, err := dockerx.OpenDefault()
```

`OpenDefault` 关闭 HTTP 代理、填入 256MiB / 0.5 CPU / 300 上限，并挂上 `LogObserver`。

要点：

- `Create` **不**自动 Start。平台开通请用 `EnsureRunning`（已存在则跳过 Create → Start → WaitReady）。
- 已 Destroy 未 Purge 时 Create/EnsureRunning 返回 `conflict`，`sandboxerr.ActionOf(err) == "use_restore"`。
- 达 `MaxInstances` 时 `ActionOf` 为 `retry_after`。
- `Spec.NetworkIsolation = sandbox.NetworkPerInstance` 时每实例一张 bridge。
- `Config.JournalPath` 非空时补偿失败写入 JSONL；新进程 Create 会先重放。
- `NewOrchestrator(client, cfg)` 等价于内部 `NewUserSandbox` + `BindOrchestrator`。CLI 用后者，避免端口池建两次。
- `Config.DefaultMemory` / `DefaultNanoCPUs` 为 0 时，`sandbox.New` 会填入推荐值，避免无限制实例。
- `MaxInstances <= 0` 表示不限额；CLI 默认 300。
- 每次调用必须带 timeout。`Create` / `EnsureRunning` 可能拉镜像。
- 自定义 Runtime/Storage 时，实现 `trackedStorage` / `trackedNetwork` 才能让补偿区分“新建 vs 复用”。

`sandboxctl` 的组成与上面相同，并固定挂上带 wait 心跳的 Observer。

---

## 生命周期

```
          Create（不自动 Start）
                │
                ▼
            created ──Start──► running ──Stop──► exited
                │                 │                 │
                │                 └── Restart ──────┘
                │
             Destroy
                │
                ├── 默认：删容器、保留卷 → destroyed（须 Restore，不能再 Create）
                └── -purge：删容器和卷 → 记录消失，可重新 Create
                │
             Restore
                ├── 默认：保留卷数据，重建容器（ID/端口可变）
                └── -reset：先删容器，再删卷，空工作区
```

其它约束：

- 开通捷径：`EnsureRunning(spec, probe)` = Create（已存在则跳过）→ Start → WaitReady；空 `probe.Kind` 时跳过等待。内核 `Create` 仍不自动 Start。
- 同一 `instance_id` 已存在容器再 Create → `conflict`；EnsureRunning 会接着 Start。
- 已 Destroy 未 Purge 再 Create → `conflict` + `next=use_restore`，必须 Restore。
- Create 中途失败：新建卷/新建网络会补偿；已有卷不会删。补偿失败写入进程内 pending，若配置了 `JournalPath` 还会落盘，下次同 ID Create 先补再创建。
- 取消/超时 **不会** 取消补偿；补偿用 `context.WithoutCancel` + 10s。

状态字符串：`created` / `running` / `paused` / `exited` / `destroyed`。

---

## 利用点

### 1. 用 Kind 做控制流，不要解析中文

```go
switch {
case sandboxerr.IsConflict(err):
    switch sandboxerr.ActionOf(err) {
    case sandboxerr.ActionUseRestore:
        // Destroy 后应 Restore，不要再 Create
    case sandboxerr.ActionRetryAfter:
        // 达 MaxInstances，稍后重试或换 ID
    default:
        // 已存在；EnsureRunning 会复用
    }
case sandboxerr.IsNotFound(err):
    // 实例或镜像不存在
case sandboxerr.IsUnavailable(err):
    // Docker daemon 未就绪
case sandboxerr.IsInvalid(err):
    // 参数/探针配置错误，不要重试
case sandboxerr.IsCanceled(err), sandboxerr.IsTimeout(err):
    // 调用方 deadline
}
```

`sandboxctl` 失败时打印 `[kind] next=... message`。本地缺失 Dockerfile 是 `not_found`，不会误判成 daemon 不可用。

### 2. 观测事件

`Observer.Observe` 不得阻塞。CLI 默认把 JSON 打到 stderr，人类模式的 `wait` 还会打心跳：

`operation`、`stage`、`instance_id`、`user_id`、`duration_ms`、`kind`、`error`、`attempt`、`detail`、`current`、`total`

典型 `stage`：`operation.start` / `operation.end`、`provision.network|storage|runtime`、`compensation.storage|network`、`pull.progress`、`wait.retry`、`portpool.warn` / `portpool.restrict`。

### 3. 端口池

- 窗口 `20000–20499`，跨进程文件锁 + Listen 占用检查。
- 进程启动时从已有容器的 `sandbox.hostports` Seed。
- **70%（350）**：`Stats().Warn`，成功分配后发 `portpool.warn`。
- **85%（425）**：拒绝 **多端口** 新实例（`ErrMultiPortRestricted`），单端口仍可分配直到耗尽。
- 绑定冲突会换端口重试一次，且不泄漏租约。
- 采集：`UserSandbox.PortPoolStats()` 或 `PortPool.Stats()`。

上层仍应自己做并发启动限流（建议 20）和日常活跃 200。

### 4. 健康检查

```go
inst, err := orch.WaitReady(ctx, "lab-42", sandbox.Probe{
    Kind: sandbox.ProbeHTTP, // 或 tcp / exec
    Path: "/ready",
    Port: 0, // 0 则用第一个 HostPort
})
```

- HTTP/TCP 必须能解析出 HostPort。
- Exec 必须有容器 ID 和非空命令。
- 未知类型、缺端口 → 立即 `invalid`。
- 连接拒绝、非 2xx、Exec 非零退出 → `ProbeNotReady`，轮询到成功或 context 截止。
- 建议 WaitReady 不超过 2 分钟。

### 5. 工作区与网络

- 卷默认挂到容器 `/workspace`，Destroy 不 Purge 则文件还在。
- 默认每用户一张 bridge；用户 inactive、无 hold 卷、无占用容器时 `Reap` 可回收网络。
- 互不信任的实训任务设置 `NetworkIsolation: sandbox.NetworkPerInstance` 或 CLI `-net-instance`，每实例独立 bridge 且关闭 ICC。

### 6. 镜像与构建

- Create 默认会拉缺失镜像；`SkipPull` 会写入 spec，Restore 后仍生效。
- 同 ref 拉取 singleflight；**等待者**可单独取消，不取消正在拉的那一个。
- `sandboxctl build` 走 `ImageBuilder`：`-base`、`-dockerfile`、`-inject host:dest`、`-step`，给实训镜像打标签。这不是编排器的一部分。

### 7. CLI 主路径

`create` **必须**带 `-instance`。旧容器路径请用显式的 `legacy-create`。默认人类可读输出；脚本加 `-json`。

```powershell
$env:GOTOOLCHAIN = "local"

# 全局标志
#   -json  -journal PATH  -host -proxy -no-proxy -timeout -prefix
#   -default-memory 256m  -default-cpus 0.5  -max-instances 300

go run -mod=mod ./cmd/sandboxctl ping
go run -mod=mod ./cmd/sandboxctl create -instance box1 -user alice -image alpine:3.20 -expose 80
go run -mod=mod ./cmd/sandboxctl ensure -instance box1 -user alice -image alpine:3.20 -expose 80 -http /ready
go run -mod=mod ./cmd/sandboxctl start box1
go run -mod=mod ./cmd/sandboxctl wait box1 -http /ready
go run -mod=mod ./cmd/sandboxctl inspect box1
go run -mod=mod ./cmd/sandboxctl stop box1
go run -mod=mod ./cmd/sandboxctl destroy box1            # 下课保数据
go run -mod=mod ./cmd/sandboxctl restore box1            # 保数据重建
go run -mod=mod ./cmd/sandboxctl restore box1 -reset     # 清空作业
go run -mod=mod ./cmd/sandboxctl destroy box1 -purge     # 清空作业
go run -mod=mod ./cmd/sandboxctl ps
```

`inspect` 在有 HostPort 时打印 `http://127.0.0.1:<port>`。`wait` 每轮探测在 stderr 打 `elapsed` / `attempt` / `last`。`ps` 附带端口池水位。

其它：`restart`、`volumes`、`volume INSTANCE`、`active USER` / `inactive USER` / `reap [USER]`、`legacy-create`、`remove`（删容器）、`build -tag ... -base ...`。

Windows 建议加 `-no-proxy`，或直接用 `OpenDefault` 的同等配置。

---

## 构建、测试与 CI

```powershell
$env:GOTOOLCHAIN = "local"
$env:DOCKER_SANDBOX_LIVE = "0"

gofmt -l (Get-ChildItem -Recurse -Filter *.go).FullName   # 必须无输出
go vet -mod=mod ./...
go test -count=1 -mod=mod ./...
go test -count=1 -mod=mod ./... "-coverprofile=coverage.out"
```

- 覆盖率是 **语句覆盖**（不是行覆盖）。包百分比 = 已执行语句 / 该包全部语句。`total` 按语句数加权。PowerShell 必须给 `-coverprofile=...` 加引号。
- `-race` 需要 CGO。仓库 CI：Windows 做格式/vet/单测/覆盖率；Ubuntu 做 race。
- Live smoke：`.github/workflows/smoke.yml`，`DOCKER_SANDBOX_LIVE=1`，手动或周一定时；默认 PR 不阻塞。

运维容量、水位、演练清单见 [`docs/operations.md`](docs/operations.md)。

---

## 生产边界

适合：单 Docker 主机、内部实训/沙箱、约 50–300 活跃实例。

上线时至少做到：

1. 每次调用带 deadline。
2. 用 `instance_id`，Restore 后重新 `Get` HostPort。
3. 订阅 Observer；70% 端口告警、85% 停止多端口新实例。
4. 上层限流并发 Start（建议 20），日常活跃建议 ≤200。
5. 不要把环境变量、构建密钥打进日志。
6. 发布周期做 Desktop 重启、拉取取消、创建补偿、端口冲突、Destroy 聚合错误演练。

已知限制：

- 跨进程同一 `instance_id` 的最终互斥靠 Docker 名称冲突，不是分布式锁。
- Registry 靠扫描容器和卷。内存插件 300 实例：Create p95 ≈ 0.75ms，List p95 ≈ 2.9ms（见 `docs/operations.md`）。Live Docker 300 仍属发布演练。
- 未配置 `JournalPath` 时，创建补偿 pending 只在进程内。
- 默认同用户网络内容器可互通；互不信任任务请开 per-instance 网络。

设计说明：[`docs/superpowers/specs/2026-08-17-sandbox-orchestrator-design.md`](docs/superpowers/specs/2026-08-17-sandbox-orchestrator-design.md)。
