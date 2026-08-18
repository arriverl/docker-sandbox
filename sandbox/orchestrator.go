package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

// Service 是可插拔沙箱编排器。平台只通过 instance_id 引用实例。
type Service struct {
	prefix    string
	waitEvery time.Duration
	runtime   Runtime
	network   Network
	storage   Storage
	registry  Registry
	health    Health
}

// New 构造编排器。plugins 五项均不可为空。
func New(cfg Config, plugins Plugins) (*Service, error) {
	if plugins.Runtime == nil || plugins.Network == nil || plugins.Storage == nil || plugins.Registry == nil || plugins.Health == nil {
		return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
			fmt.Errorf("%w: orchestrator plugins are incomplete", sandboxerr.ErrInvalid))
	}
	wait := cfg.WaitInterval
	if wait <= 0 {
		wait = 500 * time.Millisecond
	}
	return &Service{
		prefix:    prefixOrDefault(cfg.Prefix),
		waitEvery: wait,
		runtime:   plugins.Runtime,
		network:   plugins.Network,
		storage:   plugins.Storage,
		registry:  plugins.Registry,
		health:    plugins.Health,
	}, nil
}

func (s *Service) require() error {
	if s == nil {
		return sandboxerr.WrapKind("编排沙箱", sandboxerr.KindInvalid,
			fmt.Errorf("编排沙箱失败：编排器未初始化"))
	}
	return nil
}

func (s *Service) Create(ctx context.Context, spec Spec) (Instance, error) {
	if err := s.require(); err != nil {
		return Instance{}, err
	}
	spec = normalizeSpec(spec)
	if err := validateSpec(spec); err != nil {
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	if existing, err := s.registry.Get(ctx, spec.InstanceID); err == nil {
		if existing.Status == StatusDestroyed {
			return Instance{}, orchError("创建实例",
				sandboxerr.WrapKind("instance.create", sandboxerr.KindConflict,
					fmt.Errorf("%w: instance was destroyed; use restore", sandboxerr.ErrConflict)),
				spec.InstanceID, spec.UserID, spec.Image, existing.Status)
		}
		return Instance{}, orchError("创建实例",
			sandboxerr.WrapKind("instance.create", sandboxerr.KindConflict,
				fmt.Errorf("%w: instance already exists", sandboxerr.ErrConflict)),
			spec.InstanceID, spec.UserID, spec.Image, existing.Status)
	} else if err != nil && !sandboxerr.IsNotFound(err) {
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	return s.provision(ctx, spec)
}

func (s *Service) provision(ctx context.Context, spec Spec) (Instance, error) {
	netName, err := s.network.Ensure(ctx, spec.UserID)
	if err != nil {
		return Instance{}, orchError("创建用户网络", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	volLabels := instanceLabels(spec, netName, "", nil)
	vol, err := s.storage.Ensure(ctx, spec.InstanceID, volLabels)
	if err != nil {
		return Instance{}, orchError("创建工作区卷", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	name := ContainerName(s.prefix, spec.InstanceID)
	labels := instanceLabels(spec, netName, vol.Name, nil)
	_, err = s.runtime.Create(ctx, RuntimeCreate{
		Name:     name,
		Image:    spec.Image,
		Cmd:      spec.Cmd,
		Env:      spec.Env,
		Labels:   labels,
		Network:  netName,
		Binds:    []string{vol.Name + ":" + mountPath(vol)},
		Expose:   spec.Expose,
		Memory:   spec.Memory,
		NanoCPUs: spec.NanoCPUs,
		SkipPull: spec.SkipPull,
	})
	if err != nil {
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	return s.Get(ctx, spec.InstanceID)
}

func (s *Service) Start(ctx context.Context, instanceID string) (Instance, error) {
	inst, err := s.requireInstance(ctx, "启动实例", instanceID)
	if err != nil {
		return Instance{}, err
	}
	if inst.Status == StatusDestroyed || inst.ContainerID == "" {
		return Instance{}, orchError("启动实例",
			sandboxerr.WrapKind("instance.start", sandboxerr.KindNotFound,
				fmt.Errorf("%w: instance container is missing", sandboxerr.ErrNotFound)),
			inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	if err := s.runtime.Start(ctx, inst.ContainerID); err != nil {
		return Instance{}, orchError("启动实例", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	return s.Get(ctx, instanceID)
}

func (s *Service) Stop(ctx context.Context, instanceID string, opts StopOptions) (Instance, error) {
	inst, err := s.requireInstance(ctx, "停止实例", instanceID)
	if err != nil {
		return Instance{}, err
	}
	if inst.Status == StatusDestroyed || inst.ContainerID == "" {
		return inst, nil
	}
	if err := s.runtime.Stop(ctx, inst.ContainerID, opts); err != nil {
		return Instance{}, orchError("停止实例", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	return s.Get(ctx, instanceID)
}

func (s *Service) Destroy(ctx context.Context, instanceID string, opts DestroyOptions) error {
	inst, err := s.registry.Get(ctx, instanceID)
	if err != nil && !sandboxerr.IsNotFound(err) {
		return orchError("销毁实例", err, instanceID, "", "", "")
	}
	if err == nil && inst.ContainerID != "" {
		_ = s.runtime.Stop(ctx, inst.ContainerID, StopOptions{})
		if err := s.runtime.Remove(ctx, inst.ContainerID); err != nil {
			return orchError("销毁实例", err, instanceID, inst.UserID, inst.Image, inst.Status)
		}
	}
	if err := s.storage.Destroy(ctx, instanceID, opts.Purge); err != nil && !sandboxerr.IsNotFound(err) {
		return orchError("销毁实例", err, instanceID, inst.UserID, inst.Image, inst.Status)
	}
	if inst.UserID != "" {
		_ = s.network.Release(ctx, inst.UserID)
	}
	return nil
}

func (s *Service) Restore(ctx context.Context, instanceID string, opts RestoreOptions) (Instance, error) {
	if err := s.require(); err != nil {
		return Instance{}, err
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return Instance{}, orchError("还原实例",
			sandboxerr.WrapKind("instance.restore", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid)),
			"", "", "", "")
	}
	inst, spec, err := s.loadSpec(ctx, instanceID)
	if err != nil {
		return Instance{}, orchError("还原实例", err, instanceID, "", "", "")
	}
	if !opts.KeepVolume {
		if err := s.storage.Destroy(ctx, instanceID, true); err != nil && !sandboxerr.IsNotFound(err) {
			return Instance{}, orchError("还原实例", err, instanceID, spec.UserID, spec.Image, inst.Status)
		}
	}
	if inst.ContainerID != "" {
		_ = s.runtime.Stop(ctx, inst.ContainerID, StopOptions{})
		if err := s.runtime.Remove(ctx, inst.ContainerID); err != nil {
			return Instance{}, orchError("还原实例", err, instanceID, spec.UserID, spec.Image, inst.Status)
		}
	}
	return s.provision(ctx, spec)
}

func (s *Service) Restart(ctx context.Context, instanceID string, opts StopOptions) (Instance, error) {
	inst, err := s.requireInstance(ctx, "重启实例", instanceID)
	if err != nil {
		return Instance{}, err
	}
	if inst.Status == StatusDestroyed || inst.ContainerID == "" {
		return Instance{}, orchError("重启实例",
			sandboxerr.WrapKind("instance.restart", sandboxerr.KindNotFound,
				fmt.Errorf("%w: instance container is missing", sandboxerr.ErrNotFound)),
			inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	if err := s.runtime.Stop(ctx, inst.ContainerID, opts); err != nil {
		return Instance{}, orchError("重启实例", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	if err := s.runtime.Start(ctx, inst.ContainerID); err != nil {
		return Instance{}, orchError("重启实例", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	return s.Get(ctx, instanceID)
}

func (s *Service) WaitReady(ctx context.Context, instanceID string, probe Probe) (Instance, error) {
	inst, err := s.requireInstance(ctx, "等待实例就绪", instanceID)
	if err != nil {
		return Instance{}, err
	}
	if inst.Status != StatusRunning {
		return Instance{}, orchError("等待实例就绪",
			sandboxerr.WrapKind("instance.wait", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance is not running", sandboxerr.ErrInvalid)),
			inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	ticker := time.NewTicker(s.waitEvery)
	defer ticker.Stop()
	if err := s.health.Check(ctx, inst, probe); err == nil {
		return inst, nil
	}
	for {
		select {
		case <-ctx.Done():
			kind := sandboxerr.KindCanceled
			if ctx.Err() == context.DeadlineExceeded {
				kind = sandboxerr.KindTimeout
			}
			return Instance{}, orchError("等待实例就绪",
				sandboxerr.WrapKind("instance.wait", kind, ctx.Err()),
				inst.ID, inst.UserID, inst.Image, inst.Status)
		case <-ticker.C:
			cur, gerr := s.Get(ctx, instanceID)
			if gerr != nil {
				return Instance{}, orchError("等待实例就绪", gerr, instanceID, inst.UserID, inst.Image, inst.Status)
			}
			if cur.Status != StatusRunning {
				return Instance{}, orchError("等待实例就绪",
					sandboxerr.WrapKind("instance.wait", sandboxerr.KindInvalid,
						fmt.Errorf("%w: instance is not running", sandboxerr.ErrInvalid)),
					cur.ID, cur.UserID, cur.Image, cur.Status)
			}
			if err := s.health.Check(ctx, cur, probe); err == nil {
				return cur, nil
			}
		}
	}
}

func (s *Service) Get(ctx context.Context, instanceID string) (Instance, error) {
	if err := s.require(); err != nil {
		return Instance{}, err
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return Instance{}, orchError("查询实例",
			sandboxerr.WrapKind("instance.get", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid)),
			"", "", "", "")
	}
	inst, err := s.registry.Get(ctx, instanceID)
	if err != nil {
		return Instance{}, orchError("查询实例", err, instanceID, "", "", "")
	}
	return inst, nil
}

func (s *Service) List(ctx context.Context) ([]Instance, error) {
	if err := s.require(); err != nil {
		return nil, err
	}
	got, err := s.registry.List(ctx)
	if err != nil {
		return nil, orchError("列出实例", err, "", "", "", "")
	}
	return got, nil
}

func (s *Service) requireInstance(ctx context.Context, action, instanceID string) (Instance, error) {
	if err := s.require(); err != nil {
		return Instance{}, err
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return Instance{}, orchError(action,
			sandboxerr.WrapKind("instance", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid)),
			"", "", "", "")
	}
	inst, err := s.registry.Get(ctx, instanceID)
	if err != nil {
		return Instance{}, orchError(action, err, instanceID, "", "", "")
	}
	return inst, nil
}

func (s *Service) loadSpec(ctx context.Context, instanceID string) (Instance, Spec, error) {
	inst, err := s.registry.Get(ctx, instanceID)
	if err == nil {
		spec := inst.Spec
		if spec.InstanceID == "" {
			spec = specFromInstance(inst)
		}
		return inst, spec, nil
	}
	if !sandboxerr.IsNotFound(err) {
		return Instance{}, Spec{}, err
	}
	vol, verr := s.storage.Inspect(ctx, instanceID)
	if verr != nil {
		return Instance{}, Spec{}, verr
	}
	spec, ok := DecodeSpec(vol.Labels[LabelSpec])
	if !ok {
		return Instance{}, Spec{}, sandboxerr.WrapKind("instance.restore", sandboxerr.KindNotFound,
			fmt.Errorf("%w: instance spec not found", sandboxerr.ErrNotFound))
	}
	return Instance{ID: instanceID, Status: StatusDestroyed, Volume: vol.Name, Spec: spec}, spec, nil
}

func specFromInstance(inst Instance) Spec {
	if inst.Spec.InstanceID != "" {
		return inst.Spec
	}
	return Spec{
		InstanceID: inst.ID,
		UserID:     inst.UserID,
		Image:      inst.Image,
	}
}

func validateSpec(spec Spec) error {
	if spec.InstanceID == "" {
		return sandboxerr.WrapKind("instance.create", sandboxerr.KindInvalid,
			fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid))
	}
	if spec.UserID == "" {
		return sandboxerr.WrapKind("instance.create", sandboxerr.KindInvalid,
			fmt.Errorf("%w: user id is empty", sandboxerr.ErrInvalid))
	}
	if spec.Image == "" {
		return sandboxerr.WrapKind("instance.create", sandboxerr.KindInvalid,
			fmt.Errorf("%w: image is required", sandboxerr.ErrInvalid))
	}
	return nil
}

func normalizeSpec(spec Spec) Spec {
	spec.InstanceID = strings.TrimSpace(spec.InstanceID)
	spec.UserID = strings.TrimSpace(spec.UserID)
	spec.Image = strings.TrimSpace(spec.Image)
	spec.Cmd = cloneStrings(spec.Cmd)
	spec.Env = cloneStrings(spec.Env)
	spec.Expose = cloneStrings(spec.Expose)
	spec.Labels = cloneMap(spec.Labels)
	return spec
}

func instanceLabels(spec Spec, network, volume string, hostPorts []int) map[string]string {
	labels := cloneMap(spec.Labels)
	labels[LabelRole] = RoleInstance
	labels[LabelInstance] = spec.InstanceID
	labels[LabelUser] = spec.UserID
	labels[LabelNetwork] = network
	if volume != "" {
		labels[LabelVolume] = volume
	}
	if len(hostPorts) > 0 {
		labels[LabelHostPorts] = joinHostPorts(hostPorts)
	}
	labels[LabelSpec] = EncodeSpec(spec)
	return labels
}

func mountPath(vol Volume) string {
	if strings.TrimSpace(vol.MountPath) != "" {
		return vol.MountPath
	}
	return "/workspace"
}
