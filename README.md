# docker-sandbox

`docker-sandbox` 是面向单机 Docker Engine 的 Go 沙箱编排库与命令行工具。它以稳定的 `instance_id` 管理容器生命周期，并提供按用户隔离的网络、持久化工作区、资源限制和健康检查。

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

## 环境要求

- Go 1.24.2
- Docker Engine 或 Windows Docker Desktop

## 测试

```powershell
$env:GOTOOLCHAIN = "local"
go test -mod=mod ./...
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

详细架构与实现约束参见 `docs/superpowers/specs/`。
