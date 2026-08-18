package sandbox

import "time"

const (
	DefaultPrefix = "sandbox"

	LabelRole      = "sandbox.role"
	LabelInstance  = "sandbox.instance"
	LabelUser      = "sandbox.userid"
	LabelNetwork   = "sandbox.network"
	LabelVolume    = "sandbox.volume"
	LabelHostPorts = "sandbox.hostports"
	LabelSpec      = "sandbox.spec"

	RoleInstance  = "instance"
	RoleWorkspace = "workspace"

	StatusCreated   = "created"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusExited    = "exited"
	StatusDestroyed = "destroyed"
)

// Spec 是平台创建/还原沙箱实例的稳定规格。
type Spec struct {
	InstanceID string
	UserID     string
	Image      string
	Cmd        []string
	Env        []string
	Memory     int64 // bytes，0 表示不限制
	NanoCPUs   int64 // 1e9 = 1 CPU，0 表示不限制
	Expose     []string
	Labels     map[string]string
	SkipPull   bool
}

// Instance 是注册表中的沙箱实例视图。平台主键是 ID（instance_id）。
type Instance struct {
	ID          string
	Name        string
	UserID      string
	Status      string
	Image       string
	Network     string
	Volume      string
	HostPorts   []int
	ContainerID string
	Spec        Spec
}

// StopOptions 控制停止超时。
type StopOptions struct {
	Timeout *int // 秒；nil 表示实现默认值
}

// DestroyOptions 控制销毁是否删除 workspace 卷。
type DestroyOptions struct {
	Purge bool
}

// RestoreOptions 控制还原是否保留 workspace 数据。
type RestoreOptions struct {
	KeepVolume bool
}

// ProbeKind 是就绪探测类型。
type ProbeKind string

const (
	ProbeHTTP ProbeKind = "http"
	ProbeTCP  ProbeKind = "tcp"
	ProbeExec ProbeKind = "exec"
)

// Probe 描述 WaitReady 探测。
type Probe struct {
	Kind    ProbeKind
	Path    string   // HTTP 路径，如 /ready
	Port    int      // 可选；0 则用第一个 HostPort
	Command []string // Exec 命令
}

// Config 描述编排器。
type Config struct {
	Prefix       string
	WaitInterval time.Duration
}
