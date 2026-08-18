package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

// ContainerManager 提供容器创建、启动、停止、删除、详情与列表。
// 所有方法在失败时返回带中文说明的错误，并保留可程序化分类（invalid/not_found 等）。
type ContainerManager struct {
	client *Client
}

// NewContainerManager 基于已有 Docker 客户端构造容器管理器。
func NewContainerManager(c *Client) *ContainerManager {
	return &ContainerManager{client: c}
}

// Containers 返回绑定到当前客户端的容器管理器。
func (c *Client) Containers() *ContainerManager {
	return NewContainerManager(c)
}

func requireID(op, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", sandboxerr.WrapKind(op, sandboxerr.KindInvalid,
			fmt.Errorf("%w: container id is empty", sandboxerr.ErrInvalid))
	}
	return id, nil
}

func (c *Client) buildCreateSpec(opts CreateOptions) (*container.Config, *container.HostConfig, error) {
	image := strings.TrimSpace(opts.Image)
	if image == "" {
		return nil, nil, sandboxerr.WrapKind("container.create", sandboxerr.KindInvalid,
			fmt.Errorf("%w: image is required", sandboxerr.ErrInvalid))
	}

	cfg := &container.Config{
		Image:      image,
		Cmd:        opts.Cmd,
		Entrypoint: opts.Entrypoint,
		Env:        opts.Env,
		WorkingDir: opts.WorkingDir,
		Hostname:   opts.Hostname,
		User:       opts.User,
		Labels:     opts.Labels,
	}
	host := &container.HostConfig{
		AutoRemove:  opts.AutoRemove,
		Privileged:  opts.Privileged,
		Binds:       opts.Binds,
		NetworkMode: container.NetworkMode(opts.NetworkMode),
		Resources: container.Resources{
			Memory:   opts.Memory,
			NanoCPUs: opts.NanoCPUs,
		},
	}
	if len(opts.Ports) > 0 {
		exposed, bindings, err := nat.ParsePortSpecs(opts.Ports)
		if err != nil {
			return nil, nil, sandboxerr.WrapKind("container.create", sandboxerr.KindInvalid,
				fmt.Errorf("%w: invalid port spec: %v", sandboxerr.ErrInvalid, err))
		}
		cfg.ExposedPorts = exposed
		host.PortBindings = bindings
	}
	return cfg, host, nil
}

// CreateContainer 创建容器，不自动启动。
// 默认在本地缺少镜像时自动拉取；SkipPull 为 true 时跳过拉取。
func (c *Client) CreateContainer(ctx context.Context, opts CreateOptions) (CreateResult, error) {
	cfg, host, err := c.buildCreateSpec(opts)
	if err != nil {
		return CreateResult{}, err
	}
	if !opts.SkipPull {
		if err := c.ensureImage(ctx, cfg.Image); err != nil {
			return CreateResult{}, err
		}
	}
	raw, err := c.api.ContainerCreate(ctx, cfg, host, nil, nil, strings.TrimSpace(opts.Name))
	if err != nil {
		return CreateResult{}, wrapEngine("container.create", err)
	}
	return CreateResult{ID: raw.ID, Warnings: raw.Warnings}, nil
}

func (c *Client) ensureImage(ctx context.Context, ref string) error {
	_, _, err := c.api.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return nil
	}
	if engineKind(err) != sandboxerr.KindNotFound {
		return wrapEngine("image.inspect", err)
	}
	if c.pulls == nil {
		c.pulls = newPullGate()
	}
	return c.pulls.Do(ctx, ref, func() error {
		return c.pullImage(ctx, ref)
	})
}

func (c *Client) pullImage(ctx context.Context, ref string) error {
	_, _, err := c.api.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return nil
	}
	if engineKind(err) != sandboxerr.KindNotFound {
		return wrapEngine("image.inspect", err)
	}
	rc, err := c.api.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return wrapEngine("image.pull", err)
	}
	defer rc.Close()
	if err := parsePullStream(rc); err != nil {
		return err
	}
	_, _, err = c.api.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return nil
	}
	if engineKind(err) == sandboxerr.KindNotFound {
		return sandboxerr.WrapKind("image.pull", sandboxerr.KindNotFound,
			fmt.Errorf("%w: image %s still missing after pull", sandboxerr.ErrNotFound, ref))
	}
	return wrapEngine("image.inspect", err)
}

type pullStreamMsg struct {
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func parsePullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg pullStreamMsg
		if derr := dec.Decode(&msg); derr != nil {
			if derr == io.EOF {
				return nil
			}
			return sandboxerr.WrapKind("image.pull", sandboxerr.KindInvalid,
				fmt.Errorf("%w: invalid pull stream: %v", sandboxerr.ErrInvalid, derr))
		}
		fail := strings.TrimSpace(msg.Error)
		if fail == "" {
			fail = strings.TrimSpace(msg.ErrorDetail.Message)
		}
		if fail == "" {
			continue
		}
		kind := sandboxerr.KindInvalid
		sent := sandboxerr.ErrInvalid
		lower := strings.ToLower(fail)
		if strings.Contains(lower, "not found") ||
			strings.Contains(lower, "manifest unknown") ||
			strings.Contains(lower, "does not exist") {
			kind = sandboxerr.KindNotFound
			sent = sandboxerr.ErrNotFound
		}
		return sandboxerr.WrapKind("image.pull", kind, fmt.Errorf("%w: %s", sent, fail))
	}
}

// ExecContainer 在容器内执行命令；非 0 退出码视为失败。
func (c *Client) ExecContainer(ctx context.Context, id string, cmd []string) error {
	id, err := requireID("container.exec", id)
	if err != nil {
		return err
	}
	if len(cmd) == 0 {
		return sandboxerr.WrapKind("container.exec", sandboxerr.KindInvalid,
			fmt.Errorf("%w: exec command is empty", sandboxerr.ErrInvalid))
	}
	created, err := c.api.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return wrapEngine("container.exec", err)
	}
	hijack, err := c.api.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return wrapEngine("container.exec", err)
	}
	defer hijack.Close()
	if hijack.Reader != nil {
		_, _ = io.Copy(io.Discard, hijack.Reader)
	}
	insp, err := c.api.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return wrapEngine("container.exec", err)
	}
	if insp.ExitCode != 0 {
		return sandboxerr.WrapKind("container.exec", sandboxerr.KindInvalid,
			fmt.Errorf("%w: exec exit %d", sandboxerr.ErrInvalid, insp.ExitCode))
	}
	return nil
}

// StartContainer 启动已创建的容器。
// 已运行或正在重启视为成功；已暂停则恢复运行。
func (c *Client) StartContainer(ctx context.Context, id string) error {
	id, err := requireID("container.start", id)
	if err != nil {
		return err
	}
	info, gone, err := c.lookupContainer(ctx, id)
	if err != nil {
		return err
	}
	if gone {
		return wrapEngine("container.start", errdefs.NotFound(fmt.Errorf("no such container: %s", id)))
	}
	switch containerStatus(info) {
	case string(container.StateRunning), string(container.StateRestarting):
		return nil
	case string(container.StatePaused):
		return wrapEngine("container.start", c.api.ContainerUnpause(ctx, id))
	case string(container.StateRemoving):
		return wrapStateConflict("container.start", id, info.Status, "cannot start a container that is being removed")
	case string(container.StateDead):
		return wrapStateConflict("container.start", id, info.Status, "cannot start a dead container")
	default:
		return wrapEngine("container.start", ignoreNotModified(c.api.ContainerStart(ctx, id, container.StartOptions{})))
	}
}

// StopContainer 停止运行中的容器。已停止、已退出或正在删除视为成功。
func (c *Client) StopContainer(ctx context.Context, id string, opts StopOptions) error {
	id, err := requireID("container.stop", id)
	if err != nil {
		return err
	}
	info, gone, err := c.lookupContainer(ctx, id)
	if err != nil {
		return err
	}
	if gone {
		return wrapEngine("container.stop", errdefs.NotFound(fmt.Errorf("no such container: %s", id)))
	}
	switch containerStatus(info) {
	case string(container.StateRunning), string(container.StatePaused), string(container.StateRestarting):
		return wrapEngine("container.stop", ignoreNotModified(c.api.ContainerStop(ctx, id, container.StopOptions{
			Timeout: opts.Timeout,
			Signal:  opts.Signal,
		})))
	default:
		return nil
	}
}

// RestartContainer 重启容器。已暂停时先恢复再重启。
func (c *Client) RestartContainer(ctx context.Context, id string, opts StopOptions) error {
	id, err := requireID("container.restart", id)
	if err != nil {
		return err
	}
	info, gone, err := c.lookupContainer(ctx, id)
	if err != nil {
		return err
	}
	if gone {
		return wrapEngine("container.restart", errdefs.NotFound(fmt.Errorf("no such container: %s", id)))
	}
	switch containerStatus(info) {
	case string(container.StateRemoving):
		return wrapStateConflict("container.restart", id, info.Status, "cannot restart a container that is being removed")
	case string(container.StateDead):
		return wrapStateConflict("container.restart", id, info.Status, "cannot restart a dead container")
	case string(container.StatePaused):
		if err := wrapEngine("container.restart", c.api.ContainerUnpause(ctx, id)); err != nil {
			return err
		}
	}
	return wrapEngine("container.restart", ignoreNotModified(c.api.ContainerRestart(ctx, id, container.StopOptions{
		Timeout: opts.Timeout,
		Signal:  opts.Signal,
	})))
}

// RemoveContainer 删除容器。容器不存在或正在删除视为成功；运行中则先停止再删除。
func (c *Client) RemoveContainer(ctx context.Context, id string, opts RemoveOptions) error {
	id, err := requireID("container.remove", id)
	if err != nil {
		return err
	}
	info, gone, err := c.lookupContainer(ctx, id)
	if err != nil {
		return err
	}
	if gone || containerStatus(info) == string(container.StateRemoving) {
		return nil
	}
	rm := container.RemoveOptions{
		Force:         opts.Force,
		RemoveVolumes: opts.RemoveVolumes,
	}
	status := containerStatus(info)
	if status == string(container.StateDead) {
		rm.Force = true
	}
	if !rm.Force && isActiveStatus(status) {
		if err := ignoreNotModified(c.api.ContainerStop(ctx, id, container.StopOptions{})); err != nil && engineKind(err) != sandboxerr.KindNotFound {
			return wrapEngine("container.remove", err)
		}
	}
	err = ignoreNotModified(c.api.ContainerRemove(ctx, id, rm))
	if engineKind(err) == sandboxerr.KindNotFound {
		return nil
	}
	return wrapEngine("container.remove", err)
}

func (c *Client) lookupContainer(ctx context.Context, id string) (ContainerInspect, bool, error) {
	raw, err := c.api.ContainerInspect(ctx, id)
	if err != nil {
		if engineKind(err) == sandboxerr.KindNotFound {
			return ContainerInspect{}, true, nil
		}
		return ContainerInspect{}, false, wrapEngine("container.inspect", err)
	}
	return mapInspect(raw), false, nil
}

func containerStatus(info ContainerInspect) string {
	return strings.ToLower(strings.TrimSpace(info.Status))
}

func isActiveStatus(status string) bool {
	switch status {
	case string(container.StateCreated), string(container.StateRunning), string(container.StatePaused), string(container.StateRestarting):
		return true
	default:
		return false
	}
}

func wrapStateConflict(op, id, status, reason string) error {
	return sandboxerr.WrapKind(op, sandboxerr.KindConflict,
		fmt.Errorf("container %s is in state %s: %s", id, status, reason))
}

// InspectContainer 查询单个容器详情。
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerInspect, error) {
	id, err := requireID("container.inspect", id)
	if err != nil {
		return ContainerInspect{}, err
	}
	raw, err := c.api.ContainerInspect(ctx, id)
	if err != nil {
		return ContainerInspect{}, wrapEngine("container.inspect", err)
	}
	return mapInspect(raw), nil
}

func mapInspect(raw container.InspectResponse) ContainerInspect {
	info := ContainerInspect{}
	if raw.ContainerJSONBase != nil {
		info.ID = raw.ID
		info.Name = raw.Name
		info.Image = raw.Image
		info.Created = raw.Created
		info.Path = raw.Path
		info.Args = raw.Args
		info.RestartCount = raw.RestartCount
		if raw.State != nil {
			info.Status = string(raw.State.Status)
			info.Running = raw.State.Running
			info.Paused = raw.State.Paused
			info.Pid = raw.State.Pid
			info.ExitCode = raw.State.ExitCode
			info.StartedAt = raw.State.StartedAt
			info.FinishedAt = raw.State.FinishedAt
		}
		if raw.HostConfig != nil {
			info.NetworkMode = string(raw.HostConfig.NetworkMode)
		}
	}
	if raw.Config != nil {
		info.Labels = raw.Config.Labels
		info.Env = raw.Config.Env
		info.Cmd = raw.Config.Cmd
		if raw.Config.Image != "" {
			info.Image = raw.Config.Image
		}
	}
	return info
}

func (m *ContainerManager) ready(action string) (*Client, error) {
	if m == nil || m.client == nil {
		return nil, sandboxerr.WrapKind(action, sandboxerr.KindInvalid,
			fmt.Errorf("%s失败：容器管理器未初始化", action))
	}
	return m.client, nil
}

type errorContext struct {
	Ref    string
	Image  string
	Status string
}

func managerError(action string, err error, ctx errorContext) error {
	if err == nil {
		return nil
	}
	kind := sandboxerr.Classify(err)
	reason := chineseReason(action, kind, err, ctx.Status)
	parts := make([]string, 0, 3)
	if ctx.Ref != "" {
		parts = append(parts, "容器="+ctx.Ref)
	}
	if ctx.Image != "" {
		parts = append(parts, "镜像="+ctx.Image)
	}
	if ctx.Status != "" {
		label := ctx.Status
		if cn := statusCN(ctx.Status); cn != "" {
			label = ctx.Status + "/" + cn
		}
		parts = append(parts, "状态="+label)
	}
	if len(parts) == 0 {
		return sandboxerr.WrapKind(action, kind, fmt.Errorf("%s失败：%s: %w", action, reason, err))
	}
	return sandboxerr.WrapKind(action, kind,
		fmt.Errorf("%s失败：%s；%s: %w", action, strings.Join(parts, ", "), reason, err))
}

func (m *ContainerManager) annotate(ctx context.Context, action, id string, err error, extra errorContext) error {
	if err == nil {
		return nil
	}
	d := extra
	if d.Ref == "" {
		d.Ref = strings.TrimSpace(id)
	}
	if m != nil && m.client != nil && strings.TrimSpace(id) != "" {
		if info, ie := m.client.InspectContainer(ctx, id); ie == nil {
			if name := strings.TrimPrefix(info.Name, "/"); name != "" {
				d.Ref = name
			} else if info.ID != "" {
				d.Ref = info.ID
			}
			if d.Status == "" {
				d.Status = info.Status
			}
			if d.Image == "" && info.Image != "" {
				d.Image = info.Image
			}
		}
	}
	return managerError(action, err, d)
}

func statusCN(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case string(container.StateCreated):
		return "已创建未启动"
	case string(container.StateRunning):
		return "运行中"
	case string(container.StatePaused):
		return "已暂停"
	case string(container.StateRestarting):
		return "正在重启"
	case string(container.StateRemoving):
		return "正在删除"
	case string(container.StateExited):
		return "已退出"
	case string(container.StateDead):
		return "已损坏"
	default:
		return ""
	}
}

func chineseReason(action string, kind sandboxerr.Kind, err error, status string) string {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	st := strings.ToLower(strings.TrimSpace(status))
	switch kind {
	case sandboxerr.KindInvalid:
		switch {
		case strings.Contains(msg, "tag is empty"):
			return "镜像标签不能为空"
		case strings.Contains(msg, "base image is required"):
			return "基础镜像不能为空"
		case strings.Contains(msg, "invalid inject"), strings.Contains(msg, "duplicate inject"):
			return "注入文件路径无效"
		case strings.Contains(msg, "build failed"):
			detail := msg
			if i := strings.Index(msg, "build failed:"); i >= 0 {
				detail = strings.TrimSpace(msg[i+len("build failed:"):])
			}
			return "镜像构建失败（" + detail + "）"
		case strings.Contains(msg, "image is required"):
			return "镜像名称不能为空"
		case strings.Contains(msg, "user id is empty"):
			return "用户 ID 不能为空"
		case strings.Contains(msg, "instance id is empty"):
			return "工作区实例 ID 不能为空"
		case strings.Contains(msg, "id is empty"):
			return "容器 ID 或名称不能为空"
		case strings.Contains(msg, "exec command is empty"):
			return "exec 命令不能为空"
		case strings.Contains(msg, "exec exit"):
			return "容器内命令退出码非 0"
		case strings.Contains(msg, "exec probe is not configured"):
			return "exec 探测未配置"
		case strings.Contains(msg, "invalid port"):
			return "端口映射格式无效"
		case strings.Contains(msg, "read inject file"):
			return "无法读取注入文件"
		case strings.Contains(msg, "未初始化"):
			return "容器管理器未初始化"
		default:
			if msg == "" {
				return "参数无效"
			}
			return "参数无效（" + msg + "）"
		}
	case sandboxerr.KindNotFound:
		lower := strings.ToLower(msg)
		if strings.Contains(msg, "inject file not found") {
			path := msg
			if i := strings.Index(msg, "inject file not found:"); i >= 0 {
				path = strings.TrimSpace(msg[i+len("inject file not found:"):])
			}
			return "注入文件不存在（" + path + "）"
		}
		if strings.Contains(action, "卷") || strings.Contains(action, "工作区") {
			return "指定的工作区卷不存在"
		}
		if strings.Contains(action, "创建") || strings.Contains(lower, "image") || strings.Contains(msg, "镜像") {
			return "指定的镜像不存在或拉取失败"
		}
		return "指定的容器不存在"
	case sandboxerr.KindConflict:
		if strings.Contains(strings.ToLower(msg), "port pool exhausted") {
			return "宿主机端口池已满（最多 500 个）"
		}
		if strings.Contains(action, "卷") || strings.Contains(action, "工作区") {
			return "工作区卷仍被使用或名称冲突"
		}
		switch st {
		case string(container.StateDead):
			return "容器已损坏（dead），无法启动或重启"
		case string(container.StateRemoving):
			return "容器正在删除，无法执行该操作"
		case string(container.StatePaused):
			return "容器已暂停，当前状态不允许该操作"
		case string(container.StateRunning):
			return "容器正在运行，当前状态不允许该操作"
		}
		if strings.Contains(action, "创建") {
			return "容器名称已存在"
		}
		if strings.Contains(strings.ToLower(msg), "dead") {
			return "容器已损坏（dead），无法启动或重启"
		}
		return "当前状态不允许该操作"
	case sandboxerr.KindUnavailable:
		return "Docker 守护进程未运行或无法连接"
	case sandboxerr.KindTimeout:
		return "操作超时"
	case sandboxerr.KindCanceled:
		return "操作已取消"
	case sandboxerr.KindUnauthorized, sandboxerr.KindForbidden:
		return "没有权限执行该操作"
	default:
		if msg == "" {
			return "发生未知错误"
		}
		return "发生未知错误（" + msg + "）"
	}
}

// Create 创建容器，失败时返回中文错误信息。
func (m *ContainerManager) Create(ctx context.Context, opts CreateOptions) (CreateResult, error) {
	c, err := m.ready("创建容器")
	if err != nil {
		return CreateResult{}, err
	}
	got, err := c.CreateContainer(ctx, opts)
	if err != nil {
		return CreateResult{}, m.annotate(ctx, "创建容器", opts.Name, err, errorContext{
			Ref:   strings.TrimSpace(opts.Name),
			Image: strings.TrimSpace(opts.Image),
		})
	}
	return got, nil
}

// Start 启动容器，失败时返回中文错误信息。
func (m *ContainerManager) Start(ctx context.Context, id string) error {
	c, err := m.ready("启动容器")
	if err != nil {
		return err
	}
	return m.annotate(ctx, "启动容器", id, c.StartContainer(ctx, id), errorContext{})
}

// Stop 停止容器，失败时返回中文错误信息。
func (m *ContainerManager) Stop(ctx context.Context, id string, opts StopOptions) error {
	c, err := m.ready("停止容器")
	if err != nil {
		return err
	}
	return m.annotate(ctx, "停止容器", id, c.StopContainer(ctx, id, opts), errorContext{})
}

// Remove 删除容器，失败时返回中文错误信息。
func (m *ContainerManager) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	c, err := m.ready("删除容器")
	if err != nil {
		return err
	}
	return m.annotate(ctx, "删除容器", id, c.RemoveContainer(ctx, id, opts), errorContext{})
}

// Restart 重启容器，失败时返回中文错误信息。
func (m *ContainerManager) Restart(ctx context.Context, id string, opts StopOptions) error {
	c, err := m.ready("重启容器")
	if err != nil {
		return err
	}
	return m.annotate(ctx, "重启容器", id, c.RestartContainer(ctx, id, opts), errorContext{})
}

// Inspect 查询容器详情，失败时返回中文错误信息。
func (m *ContainerManager) Inspect(ctx context.Context, id string) (ContainerInspect, error) {
	c, err := m.ready("查询容器详情")
	if err != nil {
		return ContainerInspect{}, err
	}
	got, err := c.InspectContainer(ctx, id)
	if err != nil {
		return ContainerInspect{}, m.annotate(ctx, "查询容器详情", id, err, errorContext{})
	}
	return got, nil
}

// Exec 在容器内执行命令，失败时返回中文错误信息。
func (m *ContainerManager) Exec(ctx context.Context, id string, cmd []string) error {
	c, err := m.ready("执行命令")
	if err != nil {
		return err
	}
	return m.annotate(ctx, "执行命令", id, c.ExecContainer(ctx, id, cmd), errorContext{})
}

// List 列出全部容器（含已停止），失败时返回中文错误信息。
func (m *ContainerManager) List(ctx context.Context) ([]ContainerSummary, error) {
	c, err := m.ready("列出容器")
	if err != nil {
		return nil, err
	}
	got, err := c.ListContainers(ctx)
	if err != nil {
		return nil, managerError("列出容器", err, errorContext{})
	}
	return got, nil
}
