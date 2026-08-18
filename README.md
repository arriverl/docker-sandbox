# docker-sandbox

`docker-sandbox` 是面向单机 Docker Engine 的 Go 容器沙箱编排内核与命令行工具。它以稳定的 `instance_id` 管理容器生命周期，为在线实训、代码沙箱和隔离执行环境提供可嵌入的基础能力。

## 当前能力

- 创建、启动、停止、重启、销毁与还原沙箱实例
- 基于 `instance_id` 的稳定身份模型
- 按用户创建独立 Docker bridge 网络
- 持久化 workspace 命名卷
- CPU 与内存限制
- HTTP、TCP、Exec 就绪探测
- 跨进程宿主机端口池
- Windows Docker Desktop `npipe` 支持
- 可程序化识别的错误分类
- 同实例变更串行化与失败补偿
- 可配置默认资源配额
- 生命周期阶段可观测事件
- Windows CI、竞态检查和覆盖率门禁

## 环境要求

- Go 1.24.2
- Docker Engine 或 Windows Docker Desktop

## 测试

```powershell
$env:GOTOOLCHAIN = "local"
go test -mod=mod ./...
go test -race -count=1 -mod=mod ./...
go vet -mod=mod ./...
```

真实 Docker 连通性测试可通过 `DOCKER_SANDBOX_LIVE` 控制。

## CLI 示例

```powershell
go run -mod=mod ./cmd/sandboxctl create -instance box1 -user alice -image alpine:3.20 -expose 80
go run -mod=mod ./cmd/sandboxctl start box1
go run -mod=mod ./cmd/sandboxctl inspect box1
go run -mod=mod ./cmd/sandboxctl destroy box1
go run -mod=mod ./cmd/sandboxctl restore box1
```

## 生产边界

当前目标是 Windows Docker Desktop 上的单主机编排，设计峰值 300 个实例。多主机调度、鉴权、作业队列和 HTTP/gRPC 控制面应由上层平台或后续独立组件提供。

生产环境应配置非零的默认 CPU/内存配额，并为每次调用设置 deadline。`Restore` 后必须重新获取 HostPort，不能把 Docker 容器 ID 作为平台主键。

详细架构参见 `docs/superpowers/specs/`，部署、容量、监控与故障演练参见 [`docs/operations.md`](docs/operations.md)。
