# docker-sandbox 运维与可靠性指南

## 适用范围

本项目提供单 Docker Engine 的生产级沙箱编排内核，面向在线实训、代码沙箱和隔离执行环境。当前基线是 Windows + Docker Desktop，设计峰值 300 个实例、日常活跃目标 200 个实例。

它不是多主机调度器，也不替代 Kubernetes。鉴权、租户配额、作业队列和外部 API 应由上层平台提供。

## 生命周期与恢复

- `Create` 只创建实例，不自动启动。平台开通用 `EnsureRunning`。
- 同一 `instance_id` 的变更操作在单进程内串行执行。
- 创建失败会补偿本次新建的 workspace；复用的 workspace 不会被误删。
- 补偿首次失败时，同进程后续 `Create` 会先重试补偿。配置 `JournalPath` 后该记录落盘，进程重启仍可重放。
- `Destroy` 默认保留 workspace；`Purge=true` 才删除数据。
- `Restore` 保持 `instance_id`，但 Docker 容器 ID 和 HostPort 可变化。
- `RestoreOptions{ResetVolume:true}` 会先删除旧容器，再删除 workspace；该操作会永久丢弃工作区数据。零值选项默认保留 workspace。
- 已销毁再 Create：`conflict`，`sandboxerr.ActionOf(err) == use_restore`。
- 达 `MaxInstances`：`conflict`，`ActionOf == retry_after`。

平台不得缓存 Docker 容器 ID 或 Restore 前的 HostPort，应在恢复后重新调用 `Get`。

## 默认资源配额

通过 `sandbox.Config.DefaultMemory` 和 `DefaultNanoCPUs` 设置平台默认值。调用方显式指定的非零值优先。未配置时内核会填入推荐值 **256 MiB** 和 **0.5 CPU**，避免单个沙箱吃满 Docker Desktop 虚拟机。`sandboxctl` 额外提供 `-default-memory`、`-default-cpus` 和 `-max-instances`（默认 300）。

建议初始值：

- 内存：每实例 256 MiB（`RecommendedDefaultMemory`）
- CPU：每实例 0.5 CPU（`RecommendedDefaultNanoCPUs`）
- 活跃实例上限：300（`MaxInstances`，只统计非 `destroyed`）
- 上层平台并发启动限流：20
- 日常活跃实例：不超过 200

## 端口池

默认范围为 20000–20499，共 500 个 TCP 端口。内核按占用数强制执行水位，而不是把判断留给调用方：

- 70% 水位（`PortPoolWarnMark` = 350）时 `PortPool.Stats().Warn == true`。成功分配后，`UserSandbox` 会发出 `portpool.warn` 观测事件。单端口实例仍可创建。
- 85% 水位（`PortPoolRestrictMark` = 425）时 `Stats().RestrictMultiPort == true`。`Admit(n>1)`、`AllocateN(n>1)` 以及 `Expose` 多于一个端口的 `Create` 返回 `conflict`（`ErrMultiPortRestricted`），并发出 `portpool.restrict`。单端口实例仍可创建，直到池耗尽。
- 端口绑定冲突会释放当前租约并用另一个端口重试一次。
- UDP 自动端口分配不在当前支持范围。

运维侧应订阅 `portpool.warn` / `portpool.restrict`，并用 `UserSandbox.PortPoolStats()` 或 `PortPool.Stats()` 采集使用率。

上层平台仍应自行限制并发启动（建议 20）和日常活跃实例（建议不超过 200，设计峰值 300）。这两项不是端口池内核硬上限。

## 健康检查

`WaitReady` 在轮询前验证探针：

- HTTP/TCP 必须能解析出 HostPort。
- Exec 必须有容器 ID和非空命令。
- 未知探针类型立即返回 `invalid`，不会伪装为超时。
- 连接拒绝、非 2xx 和 Exec 非零退出码属于“尚未就绪”，会持续到成功或调用方 context 截止。

所有调用必须带有限 deadline。建议启动就绪等待不超过 2 分钟。

## 可观测性

可通过 `sandbox.Config.Observer` 接收 `OperationEvent`。CLI 默认使用带 wait 心跳的 Observer；JSON 行含 `operation`、`stage`、`instance_id`、`user_id`、`duration_ms`、`kind`、`error`，以及进度字段 `attempt` / `detail` / `current` / `total`。

- `pull.progress`：镜像层下载字节（`current`/`total`）
- `wait.retry`：第 n 次探测及上次失败原因
- 补偿和最终错误
- `portpool.warn` / `portpool.restrict`

建议指标：

- 生命周期操作成功率和 p95 延迟
- 各 `sandboxerr.Kind` 计数
- 创建补偿次数与补偿失败数
- WaitReady 超时率
- 端口池使用率（`PortPool.Stats().InUse / Size`，以及 `portpool.warn` / `portpool.restrict` 事件计数）
- destroyed workspace 数量
- Docker daemon 不可用时长

日志不得记录完整环境变量、代理凭据、构建密钥或未经脱敏的 Docker build 输出。

## CI 与本地门禁

```powershell
$env:GOTOOLCHAIN = "local"
$env:DOCKER_SANDBOX_LIVE = "0"

$files = Get-ChildItem -Recurse -Filter *.go | ForEach-Object { $_.FullName }
gofmt -l $files
go vet -mod=mod ./...
go test -count=1 -mod=mod ./...
go test -race -count=1 -mod=mod ./...
go test -count=1 -mod=mod ./... "-coverprofile=coverage.out"
go tool cover -func coverage.out
```

`gofmt -l` 必须无输出。默认 CI 不依赖正在运行的 Docker daemon。真实 Docker smoke 在 `.github/workflows/smoke.yml`，需手动触发或周一定时运行，并设置 `DOCKER_SANDBOX_LIVE=1`。
Windows 本地执行 `-race` 需要启用 CGO 并安装兼容 C 编译器；仓库 CI 在 Ubuntu 独立任务中执行 race detector，Windows 任务负责平台构建、单测、vet 与覆盖率。

PowerShell 必须给 `-coverprofile=coverage.out` 加引号。否则 `.out` 会被拆成独立参数，`go test` 会把它当成包名并报 `malformed module path ".out"`。

## 覆盖率怎么计算

仓库使用 Go 默认的 **statement coverage**（语句覆盖），不是行覆盖，也不是分支覆盖。

1. `go test -coverprofile=coverage.out` 在编译后的 AST 语句上插桩。一条 `if` 算进入判断的语句；`if` 的两个分支各算语句。未被任何测试执行到的函数，其全部语句计为 0%。
2. 每个包的百分比 = `该包已执行语句数 / 该包全部语句数`。例如 `dockerx 78.3% of statements` 只统计 `dockerx` 包。
3. `total:` 是把所有被测包的语句加总后再相除，因此体积大的包权重大。`cmd/sandboxctl` 的 `main` / `os.Exit` 路径几乎跑不到，会明显拉低总分。
4. `go tool cover -func coverage.out` 按函数列出百分比；`go tool cover -html coverage.out` 可看未覆盖的具体语句。
5. 覆盖率只说明测试执行过这些语句，不证明并发、Docker Desktop 故障或 300 实例压测已经演练。

当前质量目标：`sandbox` 与 `dockerx` 尽量不低于 80%；CLI 包以错误分类和参数解析测试为主，不把 `main` 行覆盖当作门禁。

## 故障演练

每个发布周期至少验证：

1. Docker Desktop 重启后，实例和端口占用可从 Docker 状态重建。
2. 镜像拉取中断时，同镜像等待者可独立取消。
3. 容器创建在卷创建后失败时，不遗留不可重试的伪 destroyed 实例。
4. 端口第一次绑定冲突时，租约不泄漏且第二端口可成功绑定。
5. workspace 删除失败时，Destroy 仍尝试网络清理并返回聚合错误。
6. 300 个实例下 `List`、`Get`、并发 Create 和 Restore 的延迟与资源占用。内存插件基线见下一节；Live Docker 300 仍须在发布周期演练。

## 300 实例延迟基线（内存插件）

`go test -count=1 -mod=mod ./sandbox -run TestScale_300ListAndCreateP95 -v` 在 Windows / Go 1.24.2 上测得（2026-08-19，不经过 Docker Engine）：

| 操作 | 样本 | p95 |
|---|---|---|
| `Create` | 300 | 0.75 ms |
| `List` | 50 次（各 300 条） | 2.9 ms |

这是编排内核 + 内存 Registry 的上限参考，用来证明 300 规模下 List 全量扫描在进程内可接受。真实 Docker Desktop 的 Create/List 还会叠加镜像层、卷和端口池，值班 SLA 仍以 live 演练为准。

## 已知边界

- 跨进程相同 `instance_id` 的最终互斥仍依赖 Docker 名称冲突和补偿；上层多进程控制面应提供分布式幂等键。
- Registry 基于 Docker 容器和卷扫描；内存 300 实例 List p95 约 3ms，Live 路径需单独取样。
- 未设置 `JournalPath` 时，创建补偿待重试标记是进程内状态；进程在补偿失败后立即退出时，管理员需按错误上下文检查残留 workspace。
- 同一用户网络默认允许容器间通信；互不信任实训任务使用 `NetworkIsolation: instance` / CLI `-net-instance`。
