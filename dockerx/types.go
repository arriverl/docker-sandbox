package dockerx

import "time"

// Config 描述 Docker Engine 客户端的连接方式。
type Config struct {
	// Host 为 Docker daemon 地址，例如：
	//   unix:///var/run/docker.sock
	//   npipe:////./pipe/docker_engine
	//   tcp://127.0.0.1:2375
	// 为空时回退到环境变量 DOCKER_HOST / 平台默认值。
	Host string

	// ProxyURL 仅对 tcp/http/https 主机生效，例如 http://127.0.0.1:8888。
	ProxyURL string

	// DisableProxy 忽略 HTTP_PROXY/HTTPS_PROXY 环境变量，也不使用 ProxyURL。
	// 本地 unix/npipe 连接始终不走代理，以便正常连上 Docker daemon。
	DisableProxy bool

	// Timeout 为单次 HTTP 请求超时，零值表示 30s。
	Timeout time.Duration

	// APIVersion 固定 Engine API 版本；为空则自动协商。
	APIVersion string
}

// PingInfo 是 daemon 可达性探测结果。
type PingInfo struct {
	Host         string
	APIVersion   string
	OSType       string
	Experimental bool
}

// VersionInfo 是 Docker Engine / API 版本信息。
type VersionInfo struct {
	Version       string
	APIVersion    string
	MinAPIVersion string
	GitCommit     string
	GoVersion     string
	Os            string
	Arch          string
	KernelVersion string
	BuildTime     string
}

// EngineInfo 是 docker info 的核心摘要，供沙箱编排做容量判断。
type EngineInfo struct {
	ID                string
	Name              string
	ServerVersion     string
	OperatingSystem   string
	OSType            string
	Architecture      string
	KernelVersion     string
	NCPU              int
	MemTotal          int64
	Driver            string
	DockerRootDir     string
	Containers        int
	ContainersRunning int
	ContainersPaused  int
	ContainersStopped int
	Images            int
}

// ContainerSummary 是 docker ps -a 的核心字段。
type ContainerSummary struct {
	ID      string
	Names   []string
	Image   string
	ImageID string
	Command string
	Created int64
	State   string
	Status  string
	Ports   []PortMapping
	Labels  map[string]string
}

// PortMapping 描述容器端口发布。
type PortMapping struct {
	IP          string
	PrivatePort uint16
	PublicPort  uint16
	Type        string
}

// ImageSummary 是 docker images -a 的核心字段。
type ImageSummary struct {
	ID          string
	ParentID    string
	RepoTags    []string
	RepoDigests []string
	Created     int64
	Size        int64
	SharedSize  int64
	Containers  int64
	Labels      map[string]string
}

// CreateOptions 描述沙箱容器的创建规格。
type CreateOptions struct {
	Name        string
	Image       string
	Cmd         []string
	Entrypoint  []string
	Env         []string
	WorkingDir  string
	Hostname    string
	User        string
	Labels      map[string]string
	AutoRemove  bool
	Privileged  bool
	NetworkMode string
	Binds       []string
	Ports       []string // docker 端口语法，如 8080:80/tcp
	Memory      int64    // 字节，0 表示不限制
	NanoCPUs    int64    // 1e9 = 1 CPU，0 表示不限制
	SkipPull    bool     // true 时不自动拉取缺失镜像
	UserID      string   // 非空时归入用户隔离网络 {prefix}-net-{userId}
	Expose      []string // 容器端口，由端口池分配宿主机端口，如 80 或 80/tcp
	InstanceID  string   // 非空时挂载工作区卷 {prefix}-ws-{instanceId}
}

// CreateResult 是容器创建结果。
type CreateResult struct {
	ID           string
	Warnings     []string
	Network      string
	HostPorts    []int
	Volume       string
	VolumeReused bool
}

// StopOptions 用于停止或重启容器。
type StopOptions struct {
	Timeout *int // 秒；nil 表示引擎默认值
	Signal  string
}

// RemoveOptions 用于删除容器。
type RemoveOptions struct {
	Force         bool
	RemoveVolumes bool
}

// ContainerInspect 是 docker inspect 的核心字段。
type ContainerInspect struct {
	ID           string
	Name         string
	Image        string
	Created      string
	Path         string
	Args         []string
	Status       string
	Running      bool
	Paused       bool
	Pid          int
	ExitCode     int
	StartedAt    string
	FinishedAt   string
	RestartCount int
	NetworkMode  string
	Labels       map[string]string
	Env          []string
	Cmd          []string
}
