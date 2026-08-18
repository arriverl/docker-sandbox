package dockerx

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/volume"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

const (
	// DefaultWorkspaceMount 是工作区卷在容器内的默认挂载路径。
	DefaultWorkspaceMount = "/workspace"
	labelInstanceID       = "sandbox.instance"
	labelVolume           = "sandbox.volume"
	roleWorkspace         = "workspace"
)

// WorkspaceConfig 描述工作区卷管理器。
type WorkspaceConfig struct {
	Prefix    string
	MountPath string
}

// WorkspaceInfo 是命名工作区卷的摘要。
type WorkspaceInfo struct {
	Name       string
	InstanceID string
	Driver     string
	Mountpoint string
	Reused     bool
	Kept       bool
	Labels     map[string]string
}

// DestroyOptions 控制 Destroy 是否清理卷。
type DestroyOptions struct {
	RemoveVolume bool
	Force        bool
}

// RestoreOptions 控制 Restore 是否保留已有卷。
type RestoreOptions struct {
	KeepVolume bool
}

// Workspace 管理 {prefix}-ws-{instanceId} 命名卷的创建、复用与回收。
type Workspace struct {
	client *Client
	prefix string
	mount  string
	mu     sync.Mutex
}

// NewWorkspace 构造工作区卷管理器。
func NewWorkspace(c *Client, cfg WorkspaceConfig) *Workspace {
	prefix := strings.TrimSpace(cfg.Prefix)
	if prefix == "" {
		prefix = DefaultNetworkPrefix
	}
	mount := strings.TrimSpace(cfg.MountPath)
	if mount == "" {
		mount = DefaultWorkspaceMount
	}
	return &Workspace{client: c, prefix: prefix, mount: mount}
}

// WorkspaceVolumeName 返回 {prefix}-ws-{instanceId}。
func WorkspaceVolumeName(prefix, instanceID string) string {
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultNetworkPrefix
	}
	return sanitizeName(prefix) + "-ws-" + sanitizeName(instanceID)
}

func (w *Workspace) requireInstance(instanceID string) (string, string, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return "", "", managerError("管理工作区卷",
			sandboxerr.WrapKind("volume", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid)),
			errorContext{})
	}
	return instanceID, WorkspaceVolumeName(w.prefix, instanceID), nil
}

func (w *Workspace) ready() error {
	if w == nil || w.client == nil {
		return sandboxerr.WrapKind("管理工作区卷", sandboxerr.KindInvalid,
			fmt.Errorf("管理工作区卷失败：容器管理器未初始化"))
	}
	return nil
}

// BindSpec 返回 name:mount 形式的 Docker bind。
func (w *Workspace) BindSpec(instanceID string) string {
	return WorkspaceVolumeName(w.prefix, instanceID) + ":" + w.mount
}

// MountPath 返回容器内挂载点。
func (w *Workspace) MountPath() string {
	if w == nil || w.mount == "" {
		return DefaultWorkspaceMount
	}
	return w.mount
}

func mapVolume(vol volume.Volume, instanceID string, reused, kept bool) WorkspaceInfo {
	return WorkspaceInfo{
		Name:       vol.Name,
		InstanceID: instanceID,
		Driver:     vol.Driver,
		Mountpoint: vol.Mountpoint,
		Reused:     reused,
		Kept:       kept,
		Labels:     vol.Labels,
	}
}

// Ensure 创建工作区卷；已存在则复用。
func (w *Workspace) Ensure(ctx context.Context, instanceID string) (WorkspaceInfo, error) {
	if err := w.ready(); err != nil {
		return WorkspaceInfo{}, err
	}
	instanceID, name, err := w.requireInstance(instanceID)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ensureLocked(ctx, instanceID, name, nil)
}

// EnsureLabels 创建工作区卷并在首次创建时写入额外标签（例如 sandbox.spec）。
func (w *Workspace) EnsureLabels(ctx context.Context, instanceID string, extra map[string]string) (WorkspaceInfo, error) {
	if err := w.ready(); err != nil {
		return WorkspaceInfo{}, err
	}
	instanceID, name, err := w.requireInstance(instanceID)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ensureLocked(ctx, instanceID, name, extra)
}

func (w *Workspace) ensureLocked(ctx context.Context, instanceID, name string, extra map[string]string) (WorkspaceInfo, error) {
	vol, err := w.client.api.VolumeInspect(ctx, name)
	if err == nil {
		return mapVolume(vol, instanceID, true, true), nil
	}
	if engineKind(err) != sandboxerr.KindNotFound {
		return WorkspaceInfo{}, managerError("查询工作区卷", wrapEngine("volume.inspect", err), errorContext{Ref: name})
	}
	labels := map[string]string{
		labelRole:       roleWorkspace,
		labelInstanceID: instanceID,
		labelVolume:     name,
	}
	for k, v := range extra {
		if strings.TrimSpace(k) != "" {
			labels[k] = v
		}
	}
	vol, err = w.client.api.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		Labels: labels,
	})
	if err != nil {
		if engineKind(err) == sandboxerr.KindConflict {
			existing, ierr := w.client.api.VolumeInspect(ctx, name)
			if ierr == nil {
				return mapVolume(existing, instanceID, true, true), nil
			}
		}
		return WorkspaceInfo{}, managerError("创建工作区卷", wrapEngine("volume.create", err), errorContext{Ref: name})
	}
	return mapVolume(vol, instanceID, false, true), nil
}

// Inspect 查询工作区卷。
func (w *Workspace) Inspect(ctx context.Context, instanceID string) (WorkspaceInfo, error) {
	if err := w.ready(); err != nil {
		return WorkspaceInfo{}, err
	}
	instanceID, name, err := w.requireInstance(instanceID)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	vol, err := w.client.api.VolumeInspect(ctx, name)
	if err != nil {
		return WorkspaceInfo{}, managerError("查询工作区卷", wrapEngine("volume.inspect", err), errorContext{Ref: name})
	}
	return mapVolume(vol, instanceID, true, true), nil
}

// List 列出当前前缀下的工作区卷。
func (w *Workspace) List(ctx context.Context) ([]WorkspaceInfo, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	raw, err := w.client.api.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, managerError("列出工作区卷", wrapEngine("volume.list", err), errorContext{})
	}
	prefix := sanitizeName(w.prefix) + "-ws-"
	out := make([]WorkspaceInfo, 0, len(raw.Volumes))
	for _, vol := range raw.Volumes {
		if vol == nil {
			continue
		}
		if !strings.HasPrefix(vol.Name, prefix) && vol.Labels[labelRole] != roleWorkspace {
			continue
		}
		instanceID := vol.Labels[labelInstanceID]
		if instanceID == "" && strings.HasPrefix(vol.Name, prefix) {
			instanceID = strings.TrimPrefix(vol.Name, prefix)
		}
		out = append(out, mapVolume(*vol, instanceID, true, true))
	}
	return out, nil
}

// Destroy 销毁工作区。RemoveVolume 为 true 时删除卷，否则保留。
func (w *Workspace) Destroy(ctx context.Context, instanceID string, opts DestroyOptions) (WorkspaceInfo, error) {
	if err := w.ready(); err != nil {
		return WorkspaceInfo{}, err
	}
	instanceID, name, err := w.requireInstance(instanceID)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	vol, err := w.client.api.VolumeInspect(ctx, name)
	if err != nil {
		if engineKind(err) == sandboxerr.KindNotFound {
			return WorkspaceInfo{Name: name, InstanceID: instanceID, Kept: false}, nil
		}
		return WorkspaceInfo{}, managerError("查询工作区卷", wrapEngine("volume.inspect", err), errorContext{Ref: name})
	}
	info := mapVolume(vol, instanceID, true, !opts.RemoveVolume)
	if !opts.RemoveVolume {
		return info, nil
	}
	if err := w.client.api.VolumeRemove(ctx, name, opts.Force); err != nil {
		if engineKind(err) == sandboxerr.KindNotFound {
			info.Kept = false
			return info, nil
		}
		return WorkspaceInfo{}, managerError("删除工作区卷", wrapEngine("volume.remove", err), errorContext{Ref: name})
	}
	info.Kept = false
	return info, nil
}

// Restore 恢复工作区。KeepVolume 为 true 时复用已有卷，否则删除后重建空卷。
func (w *Workspace) Restore(ctx context.Context, instanceID string, opts RestoreOptions) (WorkspaceInfo, error) {
	if err := w.ready(); err != nil {
		return WorkspaceInfo{}, err
	}
	instanceID, name, err := w.requireInstance(instanceID)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if opts.KeepVolume {
		return w.ensureLocked(ctx, instanceID, name, nil)
	}
	if err := w.client.api.VolumeRemove(ctx, name, true); err != nil && engineKind(err) != sandboxerr.KindNotFound {
		return WorkspaceInfo{}, managerError("重置工作区卷", wrapEngine("volume.remove", err), errorContext{Ref: name})
	}
	return w.ensureLocked(ctx, instanceID, name, nil)
}

// Attach 确保工作区卷存在，并把它加入创建规格的 Binds/Labels。
func (w *Workspace) Attach(ctx context.Context, opts *CreateOptions) (WorkspaceInfo, error) {
	if opts == nil {
		return WorkspaceInfo{}, managerError("挂载工作区卷",
			sandboxerr.WrapKind("volume.attach", sandboxerr.KindInvalid,
				fmt.Errorf("%w: create options is nil", sandboxerr.ErrInvalid)),
			errorContext{})
	}
	info, err := w.Ensure(ctx, opts.InstanceID)
	if err != nil {
		return WorkspaceInfo{}, err
	}
	bind := info.Name + ":" + w.mount
	opts.Binds = append(append([]string{}, opts.Binds...), bind)
	opts.Labels = cloneLabels(opts.Labels)
	opts.Labels[labelInstanceID] = strings.TrimSpace(opts.InstanceID)
	opts.Labels[labelVolume] = info.Name
	return info, nil
}
