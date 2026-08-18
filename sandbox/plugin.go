package sandbox

import "context"

// Runtime 管理容器进程，不感知 instance_id 语义。
type Runtime interface {
	Create(ctx context.Context, req RuntimeCreate) (RuntimeContainer, error)
	Start(ctx context.Context, containerID string) error
	Stop(ctx context.Context, containerID string, opts StopOptions) error
	Remove(ctx context.Context, containerID string) error
	Inspect(ctx context.Context, containerID string) (RuntimeContainer, error)
	Exec(ctx context.Context, containerID string, cmd []string) error
}

// RuntimeCreate 是运行时创建请求。
type RuntimeCreate struct {
	Name     string
	Image    string
	Cmd      []string
	Env      []string
	Labels   map[string]string
	Network  string
	Binds    []string
	Expose   []string
	Memory   int64
	NanoCPUs int64
	SkipPull bool
}

// RuntimeContainer 是运行时容器快照。
type RuntimeContainer struct {
	ID        string
	Name      string
	Status    string
	Image     string
	Labels    map[string]string
	HostPorts []int
	Network   string
}

// Network 管理 per-user 隔离网络。
type Network interface {
	Ensure(ctx context.Context, userID string) (string, error)
	Release(ctx context.Context, userID string) error
}

type trackedNetwork interface {
	EnsureTracked(ctx context.Context, userID string) (name string, created bool, err error)
	ReleaseCreated(ctx context.Context, userID string) error
}

// Storage 管理 workspace 卷。
type Storage interface {
	Ensure(ctx context.Context, instanceID string, labels map[string]string) (Volume, error)
	Inspect(ctx context.Context, instanceID string) (Volume, error)
	Destroy(ctx context.Context, instanceID string, purge bool) error
	List(ctx context.Context) ([]Volume, error)
}

// Volume 是 workspace 卷快照。
type Volume struct {
	Name       string
	InstanceID string
	MountPath  string
	Labels     map[string]string
}

// trackedStorage 是 Storage 的可选扩展。默认 Docker 插件用它告知编排器
// Ensure 是否新建了卷，从而在后续步骤失败时安全补偿，避免误删复用卷。
type trackedStorage interface {
	EnsureTracked(ctx context.Context, instanceID string, labels map[string]string) (Volume, bool, error)
}

// Registry 以 instance_id 为键登记实例。容器优先，否则回落到卷（destroyed）。
type Registry interface {
	Get(ctx context.Context, instanceID string) (Instance, error)
	List(ctx context.Context) ([]Instance, error)
}

// Health 探测实例是否就绪。
type Health interface {
	Check(ctx context.Context, inst Instance, probe Probe) error
}

// Plugins 是 Orchestrator 依赖的全部插件。
type Plugins struct {
	Runtime  Runtime
	Network  Network
	Storage  Storage
	Registry Registry
	Health   Health
}
