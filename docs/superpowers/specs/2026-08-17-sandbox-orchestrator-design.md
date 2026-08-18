# 沙箱编排系统设计

日期：2026-08-17  
状态：已实现（含 P0–P2 修订）  
范围：在现有 `dockerx` Docker 客户端之上，新增以 `instance_id` 为主键的可插拔编排层。

## 实现对照

已落地：

- `sandbox.Service`：Create / Start / Stop / Restart / Destroy / Restore / WaitReady / Get / List
- Docker 插件：`dockerx.NewOrchestrator` / `BindOrchestrator`（Runtime、Network、Storage、Registry、Health）
- 主键为 `instance_id`；容器名 `{prefix}-{instanceId}`；还原后 Docker ID 可变
- Create 不自动 Start；已销毁（无容器、卷仍在）再 Create → conflict，须走 Restore
- Restore 走 provision，不复用 Create 的查重
- `start/stop/restart/inspect` 只接受 instance_id，不再回退 Docker 容器 ID
- 镜像拉取：解析 pull JSON 流的 `error`/`errorDetail`，拉取后 re-inspect，按镜像 ref singleflight
- 端口池：启动时从 `sandbox.hostports` / 已发布端口 Seed；`LockPath` 文件锁 + 持久化，跨进程共享已分配端口；Allocate 仍做 Listen 占用检查
- Exec 探测：`Runtime.Exec` 接 Docker `ContainerExecCreate/Attach/Inspect`；非 0 退出码失败；`wait -exec` 可用
- 用户网络名清洗碰撞用短哈希；`created` 视为占用网络；Reap 按 `sandbox.role=usernet` 列举；hold 卷阻止回收

仍保留 / 未做：

- 无 `-instance` 的 CLI `create` 仍走旧路径（ContainerManager / UserSandbox），不经 Orchestrator
- 无独立 UDP 端口池
- 无 Kubernetes / HTTP 控制面 / 跨主机调度

## 背景

当前仓库已具备 Docker 引擎客户端、容器生命周期、per-user 网络、workspace 卷、内存限制与动态构建。缺口是：

- 没有统一编排门面；平台需要自己拼容器 ID、网络、卷。
- `restore` / `destroy` 只操作卷，不重建沙箱实例。
- 没有以 `instance_id` 为稳定主键的实例注册表。
- 没有等待服务就绪的健康检查。
- CPU 限制在 `CreateOptions.NanoCPUs` 已存在，CLI 未暴露。

平台要求：还原后 `instance_id` 不变，平台侧引用不断开。

## 目标

提供企业化、可插拔、规范化的沙箱编排 API：

1. 生命周期：创建、启动、停止、销毁、还原
2. 网络隔离：per-user 网络
3. 存储：workspace 持久化
4. 资源控制：内存、CPU
5. 健康检查：等待服务就绪
6. 实例注册：管理全部沙箱实例

非目标：Kubernetes 后端、HTTP/gRPC 控制面、跨主机调度、镜像构建（沿用现有 `dockerx` ImageBuilder）。

## 身份模型

| 字段 | 稳定性 | 用途 |
| --- | --- | --- |
| `instance_id` | 不变 | 平台唯一主键；所有编排 API 的参数 |
| 容器名 | 不变 | `{prefix}-{sanitize(instance_id)}`，例如 `sandbox-hello-test` |
| Docker 容器 ID | 可变 | 还原后会变；不对平台暴露为引用 |
| workspace 卷名 | 不变 | `{prefix}-ws-{sanitize(instance_id)}` |
| 用户网络名 | 按用户稳定 | `{prefix}-net-{sanitize(user_id)}` |

还原默认保留 workspace 数据。Docker 容器 ID 变化不影响平台引用。

## 包结构

```
sandbox/                 # 对外编排 API 与插件接口
  orchestrator.go        # Orchestrator：Create/Start/Stop/Destroy/Restore/WaitReady/Get/List
  types.go               # Instance, Spec, Probe, Destroy/Restore options
  plugin.go              # Runtime / Network / Storage / Registry / Health
dockerx/                 # 现有 Docker 引擎客户端；实现上述五个接口
cmd/sandboxctl/          # 按 instance_id 编排；旧容器 ID 命令保留但不作为主路径
sandboxerr/              # 沿用中文分类错误
```

平台只依赖 `sandbox` 包。`dockerx` 不继续膨胀为编排器。

## 插件接口

Orchestrator 只依赖接口，构造时注入。默认实现由 `dockerx` 提供。

### Runtime

创建、启动、停止、删除、检查容器，以及 Exec（健康检查）。创建时应用：

- `Memory`：字节，0 表示不限制
- `NanoCPUs`：1e9 = 1 CPU；CLI 用 `-cpus 0.5` 换算
- 容器名、labels、binds、端口、网络模式

### Network

`Ensure(userID)` 得到 `{prefix}-net-{userId}`。清洗会改写的 user id 追加短哈希，避免 `user@1` 与 `user-1` 撞名。同用户共享一张 bridge 网，不同用户隔离。用户 inactive、无 hold 卷、且无 created/running/paused/restarting 容器时回收网络。端口池仍属于 Docker 网络实现：进程启动从 Docker 重建已用端口，跨进程用锁文件序列化分配。

### Storage

`Ensure` / `Attach` / `Destroy` workspace 卷。默认挂载 `/workspace`。还原默认 `KeepVolume=true`。

### Registry

真相源为 Docker labels（进程重启后仍可 List/Get/Restore）：

- `sandbox.role=instance`
- `sandbox.instance=<instance_id>`
- `sandbox.userid=<user_id>`
- `sandbox.network=<network name>`
- `sandbox.volume=<volume name>`
- `sandbox.hostports=<comma-separated>`

规格以容器 `inspect` 反推为主（image、cmd、env、memory、nanoCPUs、网络、卷、端口绑定）。调用方原始 `Expose` 等 inspect 不稳定的字段写入紧凑 `sandbox.spec` 标签。

同一份紧凑 spec 同时写到 workspace 卷 labels。Destroy 默认留卷后，即使容器已删除，Restore 仍能从卷标签恢复规格并重建同名容器。

`Get(instance_id)` / `List()`：优先列带 `sandbox.role=instance` 的容器；无容器但卷仍在时，`Get` 返回 Status=`destroyed`（可 Restore），`List` 包含这些条目。

### Health

`Probe` 三种：

- HTTP：对已发布 HostPort 或指定 URL 做 GET，2xx 为就绪
- TCP：连接 HostPort 成功即就绪
- Exec：容器内命令 exit 0 为就绪（Docker exec，非 0 退出码视为未就绪并继续轮询）

`WaitReady` 默认间隔 500ms，超时跟随 `context`。实例未运行则失败。

## Orchestrator API

```go
type Spec struct {
    InstanceID string
    UserID     string
    Image      string
    Cmd        []string
    Env        []string
    Memory     int64 // bytes
    NanoCPUs   int64 // 1e9 = 1 CPU
    Expose     []string
    Labels     map[string]string
}

type Instance struct {
    ID         string
    Name       string
    UserID     string
    Status     string
    Image      string
    Network    string
    Volume     string
    HostPorts  []int
    ContainerID string // 仅诊断，平台不要当主键
}

type Orchestrator interface {
    Create(ctx context.Context, spec Spec) (Instance, error)
    Start(ctx context.Context, instanceID string) (Instance, error)
    Stop(ctx context.Context, instanceID string, opts StopOptions) (Instance, error)
    Restart(ctx context.Context, instanceID string, opts StopOptions) (Instance, error)
    Destroy(ctx context.Context, instanceID string, opts DestroyOptions) error
    Restore(ctx context.Context, instanceID string, opts RestoreOptions) (Instance, error)
    WaitReady(ctx context.Context, instanceID string, probe Probe) (Instance, error)
    Get(ctx context.Context, instanceID string) (Instance, error)
    List(ctx context.Context) ([]Instance, error)
}
```

`Create` 要求非空 `InstanceID`、`UserID`、`Image`。同 `instance_id` 的容器仍存在，或状态为 `destroyed`（卷还在），均为 conflict。后者请使用 Restore。

## 生命周期流程

### Create

1. 校验 Spec
2. Registry 查重：容器仍在，或 Status=`destroyed` → conflict（destroyed 提示改用还原）
3. Network.Ensure(userID)
4. Storage.Ensure(instanceID)
5. Runtime.Create：容器名 `{prefix}-{instanceId}`，打注册 labels，挂卷，限资源，加入用户网络
6. 不自动 Start

### Start / Stop / Restart

幂等：已运行再 Start 成功；已停止再 Stop 成功。paused 的 Start 先 unpause。Restart 为 Stop 再 Start。查找一律按 `instance_id`（label 或派生容器名），不接受裸 Docker 容器 ID。

### Destroy

1. Stop + Remove 容器
2. 默认保留 workspace 卷（之后可 Restore）
3. `Purge=true` 时删除卷
4. 按现有规则尝试回收空闲用户网络

### Restore（默认 KeepVolume）

1. 读规格：容器还在则 `inspect`；否则从 workspace 卷 labels 读 `sandbox.spec` / `sandbox.userid` / `sandbox.instance`
2. 若容器还在：Stop + Remove，不删卷、不删用户网络
3. 用同一 `instance_id`、同一容器名 Runtime.Create
4. 复用 workspace、用户网络、内存/CPU、Expose 端口策略（宿主机端口可能重新分配，平台应以 instance_id 再 Get，不要缓存旧 HostPorts）
5. 不自动 Start
6. 卷不存在且无容器 → not_found；`Purge` 过的实例必须重新 Create

`Create` 在「同 instance_id 的容器仍存在」或「无容器但卷还在（destroyed）」时 conflict。无容器但卷还在时应走 Restore，不要再 Create。

### WaitReady

实例必须 Running。按 Probe 轮询直到成功或 ctx 超时（timeout kind）。

## CLI

全局：`-no-proxy`、`-timeout`、`-prefix`。Go 1.24.2，`GOTOOLCHAIN=local`，`-mod=mod`。

主路径（按 instance_id）：

```
create  -instance ID -user USER -image IMAGE [-memory 64m] [-cpus 0.5] [-expose 80] [-cmd "..."] [-skip-pull]
start   INSTANCE
stop    [-t SECONDS] INSTANCE
restart [-t SECONDS] INSTANCE
destroy INSTANCE [-purge]
restore INSTANCE
inspect INSTANCE
ps
wait    INSTANCE [-http PATH] [-tcp] [-exec CMD]
```

旧的按 Docker 容器 ID 的 `remove` 以及无 `-instance` 的 `create` 仍保留，usage 标明非平台主路径。带 `-instance` 的 `create` 走 Orchestrator。

`start/stop/restart/inspect` 不再把参数当 Docker 容器 ID 回退。

## 错误

沿用 `sandboxerr`。中文说明带上下文：`实例=`、`镜像=`、`状态=`、`用户=`。不把底层原因吞成光秃的「参数无效」。

## 测试

- `sandbox` 包用 fake 插件测：Create 冲突、Start/Stop 幂等、Restore 保持 instance_id 与容器名、Destroy 默认留卷、Destroy 后再 Restore 重建同名容器、WaitReady 成功与超时。
- `dockerx` 现有单测保留。
- 不默认跑 live Docker。
- 约束：`go test -mod=mod`，`GOTOOLCHAIN=local`，不自动 `go mod tidy`，不升级到 Go 1.25。

## 约束

- 模块：`github.com/gitproduct/docker-sandbox`
- Go 1.24.2 + toolchain go1.24.2
- Windows 上 Docker Desktop 走 npipe；本地连接不走 HTTP 代理
- 不引入与现有 docker SDK 无关的新运行时依赖（端口文件锁使用标准库 / 已有 `golang.org/x/sys`）
