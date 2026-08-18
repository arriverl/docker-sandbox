package dockerx

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

type runtimeAdapter struct {
	sb  *UserSandbox
	mgr *ContainerManager
}

type networkAdapter struct {
	sb *UserSandbox
}

type storageAdapter struct {
	ws *Workspace
}

type registryAdapter struct {
	client *Client
	ws     *Workspace
	prefix string
}

// NewOrchestrator 用当前 Docker 客户端装配默认插件并返回编排器。
func NewOrchestrator(c *Client, cfg UserSandboxConfig) (*sandbox.Service, error) {
	if c == nil {
		return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
			fmt.Errorf("初始化编排器失败：容器管理器未初始化"))
	}
	return BindOrchestrator(c, NewUserSandbox(c, cfg), cfg.Prefix)
}

// BindOrchestrator 把已有 UserSandbox 接到编排器，避免 CLI 重复创建端口池。
func BindOrchestrator(c *Client, sb *UserSandbox, prefix string) (*sandbox.Service, error) {
	if c == nil || sb == nil {
		return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
			fmt.Errorf("初始化编排器失败：容器管理器未初始化"))
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = DefaultNetworkPrefix
	}
	rt := runtimeAdapter{sb: sb, mgr: c.Containers()}
	return sandbox.New(sandbox.Config{
		Prefix:          prefix,
		DefaultMemory:   sb.defaultMemory,
		DefaultNanoCPUs: sb.defaultNanoCPUs,
		Observer:        sb.observer,
	}, sandbox.Plugins{
		Runtime:  rt,
		Network:  networkAdapter{sb: sb},
		Storage:  storageAdapter{ws: sb.Workspace()},
		Registry: registryAdapter{client: c, ws: sb.Workspace(), prefix: prefix},
		Health:   sandbox.NewStdHealth(rt.Exec),
	})
}

func (a runtimeAdapter) Create(ctx context.Context, req sandbox.RuntimeCreate) (sandbox.RuntimeContainer, error) {
	labelNetwork := strings.TrimSpace(req.Labels[sandbox.LabelNetwork])
	if network := strings.TrimSpace(req.Network); network != "" && labelNetwork != "" && network != labelNetwork {
		return sandbox.RuntimeContainer{}, sandboxerr.WrapKind("创建实例", sandboxerr.KindInvalid,
			fmt.Errorf("%w: runtime network %q does not match label %q", sandboxerr.ErrInvalid, network, labelNetwork))
	}
	opts := CreateOptions{
		Name:        req.Name,
		Image:       req.Image,
		Cmd:         req.Cmd,
		Env:         req.Env,
		Labels:      req.Labels,
		Memory:      req.Memory,
		NanoCPUs:    req.NanoCPUs,
		Expose:      req.Expose,
		Binds:       append([]string(nil), req.Binds...),
		NetworkMode: req.Network,
		UserID:      req.Labels[sandbox.LabelUser],
		SkipPull:    req.SkipPull,
	}
	got, err := a.sb.Create(ctx, opts)
	if err != nil {
		return sandbox.RuntimeContainer{}, err
	}
	status := sandbox.StatusCreated
	if info, ierr := a.mgr.Inspect(ctx, got.ID); ierr == nil && info.Status != "" {
		status = strings.ToLower(info.Status)
	}
	labels := req.Labels
	if len(got.HostPorts) > 0 {
		labels = cloneLabels(labels)
		labels[sandbox.LabelHostPorts] = joinInts(got.HostPorts)
	}
	return sandbox.RuntimeContainer{
		ID:        got.ID,
		Name:      req.Name,
		Status:    status,
		Image:     req.Image,
		Labels:    labels,
		HostPorts: got.HostPorts,
		Network:   got.Network,
	}, nil
}

func (a runtimeAdapter) Start(ctx context.Context, containerID string) error {
	return a.mgr.Start(ctx, containerID)
}

func (a runtimeAdapter) Stop(ctx context.Context, containerID string, opts sandbox.StopOptions) error {
	return a.mgr.Stop(ctx, containerID, StopOptions{Timeout: opts.Timeout})
}

func (a runtimeAdapter) Remove(ctx context.Context, containerID string) error {
	return a.sb.Remove(ctx, containerID, RemoveOptions{Force: true})
}

func (a runtimeAdapter) Inspect(ctx context.Context, containerID string) (sandbox.RuntimeContainer, error) {
	info, err := a.mgr.Inspect(ctx, containerID)
	if err != nil {
		return sandbox.RuntimeContainer{}, err
	}
	return runtimeFromInspect(info), nil
}

func (a runtimeAdapter) Exec(ctx context.Context, containerID string, cmd []string) error {
	return a.mgr.Exec(ctx, containerID, cmd)
}

func (n networkAdapter) Ensure(ctx context.Context, userID string) (string, error) {
	return n.sb.EnsureNetwork(ctx, userID)
}

func (n networkAdapter) EnsureTracked(ctx context.Context, userID string) (string, bool, error) {
	return n.sb.EnsureNetworkTracked(ctx, userID)
}

func (n networkAdapter) Release(ctx context.Context, userID string) error {
	return n.sb.TryReap(ctx, userID)
}

func (n networkAdapter) ReleaseCreated(ctx context.Context, userID string) error {
	return n.sb.ReleaseCreatedNetwork(ctx, userID)
}

func (s storageAdapter) Ensure(ctx context.Context, instanceID string, labels map[string]string) (sandbox.Volume, error) {
	vol, _, err := s.EnsureTracked(ctx, instanceID, labels)
	return vol, err
}

func (s storageAdapter) EnsureTracked(ctx context.Context, instanceID string, labels map[string]string) (sandbox.Volume, bool, error) {
	info, err := s.ws.EnsureLabels(ctx, instanceID, labels)
	if err != nil {
		return sandbox.Volume{}, false, err
	}
	return volumeFromInfo(info), !info.Reused, nil
}

func (s storageAdapter) Inspect(ctx context.Context, instanceID string) (sandbox.Volume, error) {
	info, err := s.ws.Inspect(ctx, instanceID)
	if err != nil {
		return sandbox.Volume{}, err
	}
	return volumeFromInfo(info), nil
}

func (s storageAdapter) Destroy(ctx context.Context, instanceID string, purge bool) error {
	_, err := s.ws.Destroy(ctx, instanceID, DestroyOptions{RemoveVolume: purge, Force: true})
	return err
}

func (s storageAdapter) List(ctx context.Context) ([]sandbox.Volume, error) {
	raw, err := s.ws.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sandbox.Volume, 0, len(raw))
	for _, item := range raw {
		out = append(out, volumeFromInfo(item))
	}
	return out, nil
}

func (r registryAdapter) Get(ctx context.Context, instanceID string) (sandbox.Instance, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return sandbox.Instance{}, sandboxerr.WrapKind("查询实例", sandboxerr.KindInvalid,
			fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid))
	}
	if inst, ok, err := r.fromContainers(ctx, instanceID); err != nil {
		return sandbox.Instance{}, err
	} else if ok {
		return inst, nil
	}
	vol, err := r.ws.Inspect(ctx, instanceID)
	if err != nil {
		if sandboxerr.IsNotFound(err) {
			return sandbox.Instance{}, sandboxerr.WrapKind("查询实例", sandboxerr.KindNotFound,
				fmt.Errorf("%w: instance %s not found", sandboxerr.ErrNotFound, instanceID))
		}
		return sandbox.Instance{}, err
	}
	return instanceFromVolume(vol, r.prefix), nil
}

func (r registryAdapter) List(ctx context.Context) ([]sandbox.Instance, error) {
	items, err := r.client.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []sandbox.Instance
	for _, item := range items {
		inst, ok := instanceFromSummary(item)
		if !ok {
			continue
		}
		seen[inst.ID] = struct{}{}
		out = append(out, inst)
	}
	vols, err := r.ws.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, vol := range vols {
		if _, ok := seen[vol.InstanceID]; ok {
			continue
		}
		out = append(out, instanceFromVolume(vol, r.prefix))
	}
	return out, nil
}

func (r registryAdapter) fromContainers(ctx context.Context, instanceID string) (sandbox.Instance, bool, error) {
	items, err := r.client.ListContainers(ctx)
	if err != nil {
		return sandbox.Instance{}, false, err
	}
	for _, item := range items {
		inst, ok := instanceFromSummary(item)
		if ok && inst.ID == instanceID {
			return inst, true, nil
		}
	}
	info, err := r.client.InspectContainer(ctx, sandbox.ContainerName(r.prefix, instanceID))
	if err != nil {
		if sandboxerr.IsNotFound(err) {
			return sandbox.Instance{}, false, nil
		}
		return sandbox.Instance{}, false, err
	}
	if info.Labels[sandbox.LabelInstance] != instanceID && info.Labels[sandbox.LabelRole] != sandbox.RoleInstance {
		return sandbox.Instance{}, false, nil
	}
	return instanceFromInspect(info), true, nil
}

func runtimeFromInspect(info ContainerInspect) sandbox.RuntimeContainer {
	return sandbox.RuntimeContainer{
		ID:        info.ID,
		Name:      strings.TrimPrefix(info.Name, "/"),
		Status:    strings.ToLower(info.Status),
		Image:     info.Image,
		Labels:    info.Labels,
		HostPorts: sandboxHostPorts(info.Labels),
		Network:   info.Labels[sandbox.LabelNetwork],
	}
}

func instanceFromInspect(info ContainerInspect) sandbox.Instance {
	labels := info.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	spec, _ := sandbox.DecodeSpec(labels[sandbox.LabelSpec])
	id := labels[sandbox.LabelInstance]
	if id == "" {
		id = strings.TrimPrefix(info.Name, "/")
	}
	return sandbox.Instance{
		ID:          id,
		Name:        strings.TrimPrefix(info.Name, "/"),
		UserID:      labels[sandbox.LabelUser],
		Status:      strings.ToLower(info.Status),
		Image:       info.Image,
		Network:     labels[sandbox.LabelNetwork],
		Volume:      labels[sandbox.LabelVolume],
		HostPorts:   sandboxHostPorts(labels),
		ContainerID: info.ID,
		Spec:        spec,
	}
}

func instanceFromSummary(item ContainerSummary) (sandbox.Instance, bool) {
	if item.Labels[sandbox.LabelRole] != sandbox.RoleInstance {
		return sandbox.Instance{}, false
	}
	id := item.Labels[sandbox.LabelInstance]
	if id == "" {
		return sandbox.Instance{}, false
	}
	name := id
	if len(item.Names) > 0 {
		name = strings.TrimPrefix(item.Names[0], "/")
	}
	spec, _ := sandbox.DecodeSpec(item.Labels[sandbox.LabelSpec])
	status := strings.ToLower(item.State)
	if status == "" {
		status = strings.ToLower(item.Status)
	}
	return sandbox.Instance{
		ID:          id,
		Name:        name,
		UserID:      item.Labels[sandbox.LabelUser],
		Status:      status,
		Image:       item.Image,
		Network:     item.Labels[sandbox.LabelNetwork],
		Volume:      item.Labels[sandbox.LabelVolume],
		HostPorts:   sandboxHostPorts(item.Labels),
		ContainerID: item.ID,
		Spec:        spec,
	}, true
}

func instanceFromVolume(vol WorkspaceInfo, prefix string) sandbox.Instance {
	spec, _ := sandbox.DecodeSpec(vol.Labels[sandbox.LabelSpec])
	id := vol.InstanceID
	if id == "" {
		id = spec.InstanceID
	}
	return sandbox.Instance{
		ID:     id,
		Name:   sandbox.ContainerName(prefix, id),
		UserID: firstNonEmpty(spec.UserID, vol.Labels[sandbox.LabelUser]),
		Status: sandbox.StatusDestroyed,
		Image:  spec.Image,
		Volume: vol.Name,
		Spec:   spec,
	}
}

func volumeFromInfo(info WorkspaceInfo) sandbox.Volume {
	return sandbox.Volume{
		Name:       info.Name,
		InstanceID: info.InstanceID,
		MountPath:  DefaultWorkspaceMount,
		Labels:     info.Labels,
	}
}

func sandboxHostPorts(labels map[string]string) []int {
	if labels == nil {
		return nil
	}
	raw := labels[sandbox.LabelHostPorts]
	if raw == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
