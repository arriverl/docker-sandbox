package dockerx

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

// EngineAPI 是 Docker Engine SDK 中本阶段用到的最小子集，便于单测注入。
type EngineAPI interface {
	Ping(ctx context.Context) (types.Ping, error)
	ServerVersion(ctx context.Context) (types.Version, error)
	Info(ctx context.Context) (system.Info, error)
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)
	ImageList(ctx context.Context, options image.ListOptions) ([]image.Summary, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRestart(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerUnpause(ctx context.Context, containerID string) error
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Summary, error)
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeInspect(ctx context.Context, volumeID string) (volume.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	VolumeList(ctx context.Context, options volume.ListOptions) (volume.ListResponse, error)
	ImageInspectWithRaw(ctx context.Context, imageID string) (image.InspectResponse, []byte, error)
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error)
	ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	Close() error
	DaemonHost() string
}

// Client 封装 Docker Engine API 客户端。
type Client struct {
	api   EngineAPI
	pulls *pullGate
}

// New 根据 Config 创建真实 Docker 客户端。
// 构造阶段不要求 daemon 已启动；连通性由 Ping/Version/Info 检测。
func New(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	httpClient, err := cfg.HTTPClient()
	if err != nil {
		return nil, err
	}

	host := cfg.ResolveHost()

	opts := []client.Opt{
		client.WithHTTPClient(httpClient),
		client.WithHost(host),
	}
	if cfg.APIVersion != "" {
		opts = append(opts, client.WithVersion(cfg.APIVersion))
	} else {
		opts = append(opts, client.WithAPIVersionNegotiation())
	}
	if cfg.Timeout > 0 {
		opts = append(opts, client.WithTimeout(cfg.Timeout))
	}

	api, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, wrapEngine("connect", err)
	}
	return &Client{api: api, pulls: newPullGate()}, nil
}

// NewWithAPI 注入自定义 Engine API，主要用于测试。
func NewWithAPI(api EngineAPI) *Client {
	return &Client{api: api, pulls: newPullGate()}
}

// Close 释放底层连接。
func (c *Client) Close() error {
	if c == nil || c.api == nil {
		return nil
	}
	return wrapEngine("close", c.api.Close())
}

// Host 返回当前连接的 daemon 地址。
func (c *Client) Host() string {
	if c == nil || c.api == nil {
		return ""
	}
	return c.api.DaemonHost()
}

// Ping 探测 daemon 是否可达。
func (c *Client) Ping(ctx context.Context) (PingInfo, error) {
	raw, err := c.api.Ping(ctx)
	if err != nil {
		return PingInfo{}, wrapEngine("ping", err)
	}
	return PingInfo{
		Host:         c.Host(),
		APIVersion:   raw.APIVersion,
		OSType:       raw.OSType,
		Experimental: raw.Experimental,
	}, nil
}

// Version 查询 Docker Engine 版本。
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	raw, err := c.api.ServerVersion(ctx)
	if err != nil {
		return VersionInfo{}, wrapEngine("version", err)
	}
	return VersionInfo{
		Version:       raw.Version,
		APIVersion:    raw.APIVersion,
		MinAPIVersion: raw.MinAPIVersion,
		GitCommit:     raw.GitCommit,
		GoVersion:     raw.GoVersion,
		Os:            raw.Os,
		Arch:          raw.Arch,
		KernelVersion: raw.KernelVersion,
		BuildTime:     raw.BuildTime,
	}, nil
}

// Info 查询 daemon 基础资源与容器/镜像计数。
func (c *Client) Info(ctx context.Context) (EngineInfo, error) {
	raw, err := c.api.Info(ctx)
	if err != nil {
		return EngineInfo{}, wrapEngine("info", err)
	}
	return EngineInfo{
		ID:                raw.ID,
		Name:              raw.Name,
		ServerVersion:     raw.ServerVersion,
		OperatingSystem:   raw.OperatingSystem,
		OSType:            raw.OSType,
		Architecture:      raw.Architecture,
		KernelVersion:     raw.KernelVersion,
		NCPU:              raw.NCPU,
		MemTotal:          raw.MemTotal,
		Driver:            raw.Driver,
		DockerRootDir:     raw.DockerRootDir,
		Containers:        raw.Containers,
		ContainersRunning: raw.ContainersRunning,
		ContainersPaused:  raw.ContainersPaused,
		ContainersStopped: raw.ContainersStopped,
		Images:            raw.Images,
	}, nil
}

// ListContainers 列出全部容器（含已停止），等价于 docker ps -a。
func (c *Client) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	raw, err := c.api.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, wrapEngine("container.list", err)
	}
	out := make([]ContainerSummary, 0, len(raw))
	for _, item := range raw {
		ports := make([]PortMapping, 0, len(item.Ports))
		for _, p := range item.Ports {
			ports = append(ports, PortMapping{
				IP:          p.IP,
				PrivatePort: p.PrivatePort,
				PublicPort:  p.PublicPort,
				Type:        p.Type,
			})
		}
		out = append(out, ContainerSummary{
			ID:      item.ID,
			Names:   item.Names,
			Image:   item.Image,
			ImageID: item.ImageID,
			Command: item.Command,
			Created: item.Created,
			State:   item.State,
			Status:  item.Status,
			Ports:   ports,
			Labels:  item.Labels,
		})
	}
	return out, nil
}

// ListImages 列出全部镜像（含中间层），等价于 docker images -a。
func (c *Client) ListImages(ctx context.Context) ([]ImageSummary, error) {
	raw, err := c.api.ImageList(ctx, image.ListOptions{All: true})
	if err != nil {
		return nil, wrapEngine("image.list", err)
	}
	out := make([]ImageSummary, 0, len(raw))
	for _, item := range raw {
		out = append(out, ImageSummary{
			ID:          item.ID,
			ParentID:    item.ParentID,
			RepoTags:    item.RepoTags,
			RepoDigests: item.RepoDigests,
			Created:     item.Created,
			Size:        item.Size,
			SharedSize:  item.SharedSize,
			Containers:  item.Containers,
			Labels:      item.Labels,
		})
	}
	return out, nil
}

func wrapEngine(op string, err error) error {
	if err == nil {
		return nil
	}
	kind := engineKind(err)
	if kind == sandboxerr.KindUnavailable {
		return sandboxerr.WrapKind(op, kind,
			fmt.Errorf("%w: docker daemon is not running or not reachable (%v)", sandboxerr.ErrUnavailable, err))
	}
	if kind != sandboxerr.KindUnknown {
		return sandboxerr.WrapKind(op, kind, err)
	}
	return sandboxerr.Wrap(op, err)
}

func ignoreNotModified(err error) error {
	if err == nil || errdefs.IsNotModified(err) {
		return nil
	}
	return err
}

func engineKind(err error) sandboxerr.Kind {
	switch {
	case errdefs.IsNotFound(err):
		return sandboxerr.KindNotFound
	case errdefs.IsConflict(err):
		return sandboxerr.KindConflict
	case errdefs.IsUnauthorized(err):
		return sandboxerr.KindUnauthorized
	case errdefs.IsForbidden(err):
		return sandboxerr.KindForbidden
	case errdefs.IsDeadline(err):
		return sandboxerr.KindTimeout
	case errdefs.IsCancelled(err):
		return sandboxerr.KindCanceled
	case errdefs.IsInvalidParameter(err):
		return sandboxerr.KindInvalid
	case errdefs.IsUnavailable(err), client.IsErrConnectionFailed(err):
		return sandboxerr.KindUnavailable
	default:
		return sandboxerr.Classify(err)
	}
}
