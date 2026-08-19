package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

// Service 是可插拔沙箱编排器。平台只通过 instance_id 引用实例。
type Service struct {
	prefix          string
	waitEvery       time.Duration
	runtime         Runtime
	network         Network
	storage         Storage
	registry        Registry
	health          Health
	observer        Observer
	defaultMemory   int64
	defaultNanoCPUs int64
	maxInstances    int
	mutations       keyedLocker
	users           keyedLocker
	failed          sync.Map
	journal         *fileJournal
}

// New 构造编排器。plugins 五项均不可为空。
func New(cfg Config, plugins Plugins) (*Service, error) {
	if plugins.Runtime == nil || plugins.Network == nil || plugins.Storage == nil || plugins.Registry == nil || plugins.Health == nil {
		return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
			fmt.Errorf("%w: orchestrator plugins are incomplete", sandboxerr.ErrInvalid))
	}
	if cfg.DefaultMemory < 0 || cfg.DefaultNanoCPUs < 0 {
		return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
			fmt.Errorf("%w: default resource limits must not be negative", sandboxerr.ErrInvalid))
	}
	if cfg.MaxInstances < 0 {
		return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
			fmt.Errorf("%w: max instances must not be negative", sandboxerr.ErrInvalid))
	}
	wait := cfg.WaitInterval
	if wait <= 0 {
		wait = 500 * time.Millisecond
	}
	memory := cfg.DefaultMemory
	if memory == 0 {
		memory = RecommendedDefaultMemory
	}
	nanoCPUs := cfg.DefaultNanoCPUs
	if nanoCPUs == 0 {
		nanoCPUs = RecommendedDefaultNanoCPUs
	}
	svc := &Service{
		prefix:          prefixOrDefault(cfg.Prefix),
		waitEvery:       wait,
		runtime:         plugins.Runtime,
		network:         plugins.Network,
		storage:         plugins.Storage,
		registry:        plugins.Registry,
		health:          plugins.Health,
		observer:        cfg.Observer,
		defaultMemory:   memory,
		defaultNanoCPUs: nanoCPUs,
		maxInstances:    cfg.MaxInstances,
	}
	if path := strings.TrimSpace(cfg.JournalPath); path != "" {
		svc.journal = newFileJournal(path)
		if err := svc.loadJournal(); err != nil {
			return nil, sandboxerr.WrapKind("初始化编排器", sandboxerr.KindInvalid,
				fmt.Errorf("%w: load compensation journal: %v", sandboxerr.ErrInvalid, err))
		}
	}
	return svc, nil
}

func (s *Service) require() error {
	if s == nil {
		return sandboxerr.WrapKind("编排沙箱", sandboxerr.KindInvalid,
			fmt.Errorf("编排沙箱失败：编排器未初始化"))
	}
	return nil
}

func (s *Service) loadJournal() error {
	if s == nil || s.journal == nil {
		return nil
	}
	entries, err := s.journal.load()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		s.failed.Store(entry.InstanceID, entry)
	}
	return nil
}

func (s *Service) markPending(entry journalEntry) error {
	if entry.InstanceID == "" {
		return nil
	}
	s.failed.Store(entry.InstanceID, entry)
	if s.journal != nil {
		if err := s.journal.put(entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clearPending(instanceID string) {
	s.failed.Delete(instanceID)
	if s.journal != nil {
		_ = s.journal.remove(instanceID)
	}
}

func (s *Service) replayPending(ctx context.Context, spec Spec) error {
	raw, _ := s.failed.Load(spec.InstanceID)
	entry, _ := raw.(journalEntry)
	if entry.InstanceID == "" {
		entry.InstanceID = spec.InstanceID
		entry.Volume = true
	}
	if entry.Volume || entry.InstanceID != "" {
		if err := s.cleanupFailedCreate(ctx, spec.InstanceID); err != nil {
			return err
		}
	}
	owner := entry.NetworkOwner
	if owner == "" {
		owner = networkOwner(spec)
	}
	if entry.Network && owner != "" {
		if err := s.cleanupCreatedNetwork(ctx, owner); err != nil {
			return err
		}
	}
	s.clearPending(spec.InstanceID)
	return nil
}

func (s *Service) EnsureRunning(ctx context.Context, spec Spec, probe Probe) (result Instance, retErr error) {
	operationStarted := time.Now()
	spec = s.applyDefaults(normalizeSpec(spec))
	s.observe(ctx, "ensure", spec, "operation.start", operationStarted, nil)
	defer func() {
		s.observe(ctx, "ensure", spec, "operation.end", operationStarted, retErr)
	}()
	inst, err := s.Create(ctx, spec)
	if err != nil {
		if sandboxerr.ActionOf(err) == sandboxerr.ActionUseRestore || sandboxerr.ActionOf(err) == sandboxerr.ActionRetryAfter {
			return Instance{}, err
		}
		if !sandboxerr.IsConflict(err) {
			return Instance{}, err
		}
		inst, err = s.Get(ctx, spec.InstanceID)
		if err != nil {
			return Instance{}, err
		}
	}
	if inst.Status != StatusRunning {
		inst, err = s.Start(ctx, inst.ID)
		if err != nil {
			return Instance{}, err
		}
	}
	if probe.Kind == "" {
		return inst, nil
	}
	return s.WaitReady(ctx, inst.ID, probe)
}

func (s *Service) Create(ctx context.Context, spec Spec) (result Instance, retErr error) {
	operationStarted := time.Now()
	s.observe(ctx, "create", spec, "operation.start", operationStarted, nil)
	defer func() {
		s.observe(ctx, "create", spec, "operation.end", operationStarted, retErr)
	}()
	if err := s.require(); err != nil {
		return Instance{}, err
	}
	spec = s.applyDefaults(normalizeSpec(spec))
	if err := validateSpec(spec); err != nil {
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	unlock := s.mutations.lock(spec.InstanceID)
	defer unlock()
	_, pending := s.failed.Load(spec.InstanceID)
	if existing, err := s.registry.Get(ctx, spec.InstanceID); err == nil {
		if existing.Status == StatusDestroyed && pending {
			if err := s.replayPending(ctx, spec); err != nil {
				return Instance{}, orchError("清理失败创建", err, spec.InstanceID, spec.UserID, spec.Image, existing.Status)
			}
		} else if existing.Status == StatusDestroyed {
			return Instance{}, orchError("创建实例",
				sandboxerr.WrapKindAction("instance.create", sandboxerr.KindConflict, sandboxerr.ActionUseRestore,
					fmt.Errorf("%w: instance was destroyed; use restore", sandboxerr.ErrConflict)),
				spec.InstanceID, spec.UserID, spec.Image, existing.Status)
		} else {
			s.clearPending(spec.InstanceID)
			return Instance{}, orchError("创建实例",
				sandboxerr.WrapKind("instance.create", sandboxerr.KindConflict,
					fmt.Errorf("%w: instance already exists", sandboxerr.ErrConflict)),
				spec.InstanceID, spec.UserID, spec.Image, existing.Status)
		}
	} else if err != nil && !sandboxerr.IsNotFound(err) {
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	} else if pending {
		if err := s.replayPending(ctx, spec); err != nil {
			return Instance{}, orchError("清理失败创建", err, spec.InstanceID, spec.UserID, spec.Image, "")
		}
	}
	if err := s.enforceMaxInstances(ctx, spec); err != nil {
		return Instance{}, err
	}
	return s.provision(ctx, spec)
}

func (s *Service) enforceMaxInstances(ctx context.Context, spec Spec) error {
	if s.maxInstances <= 0 {
		return nil
	}
	items, err := s.registry.List(ctx)
	if err != nil {
		return orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	active := 0
	for _, inst := range items {
		if inst.Status != StatusDestroyed {
			active++
		}
	}
	if active >= s.maxInstances {
		return orchError("创建实例",
			sandboxerr.WrapKindAction("instance.create", sandboxerr.KindConflict, sandboxerr.ActionRetryAfter,
				fmt.Errorf("%w: active instance count %d reached max %d", sandboxerr.ErrConflict, active, s.maxInstances)),
			spec.InstanceID, spec.UserID, spec.Image, "")
	}
	return nil
}

func (s *Service) provision(ctx context.Context, spec Spec) (Instance, error) {
	return s.provisionFor(ctx, spec, "create")
}

func (s *Service) provisionFor(ctx context.Context, spec Spec, operation string) (Instance, error) {
	spec = s.applyDefaults(normalizeSpec(spec))
	if err := validateSpec(spec); err != nil {
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	unlockUser := s.users.lock(spec.UserID)
	defer unlockUser()
	owner := networkOwner(spec)
	started := time.Now()
	netName, networkCreated, err := s.ensureNetwork(ctx, owner)
	s.observe(ctx, operation, spec, "provision.network", started, err)
	if err != nil {
		return Instance{}, orchError("创建用户网络", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	volLabels := instanceLabels(spec, netName, "", nil)
	started = time.Now()
	vol, created, err := s.ensureWorkspace(ctx, spec.InstanceID, volLabels)
	s.observe(ctx, operation, spec, "provision.storage", started, err)
	if err != nil {
		if networkCreated {
			cleanupStarted := time.Now()
			cleanupErr := s.cleanupCreatedNetwork(ctx, owner)
			s.observe(ctx, operation, spec, "compensation.network", cleanupStarted, cleanupErr)
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("回滚用户网络失败: %w", cleanupErr))
				if jerr := s.markPending(journalEntry{InstanceID: spec.InstanceID, UserID: spec.UserID, NetworkOwner: owner, Network: true}); jerr != nil {
					err = errors.Join(err, fmt.Errorf("写入补偿日志失败: %w", jerr))
				}
			}
		}
		return Instance{}, orchError("创建工作区卷", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	name := ContainerName(s.prefix, spec.InstanceID)
	labels := instanceLabels(spec, netName, vol.Name, nil)
	started = time.Now()
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
	s.observe(ctx, operation, spec, "provision.runtime", started, err)
	if err != nil {
		pending := journalEntry{InstanceID: spec.InstanceID, UserID: spec.UserID, NetworkOwner: owner}
		if created {
			cleanupStarted := time.Now()
			cleanupErr := s.cleanupFailedCreate(ctx, spec.InstanceID)
			s.observe(ctx, operation, spec, "compensation.storage", cleanupStarted, cleanupErr)
			if cleanupErr != nil {
				pending.Volume = true
				err = errors.Join(err, fmt.Errorf("回滚工作区卷失败: %w", cleanupErr))
			}
		}
		if networkCreated {
			cleanupStarted := time.Now()
			cleanupErr := s.cleanupCreatedNetwork(ctx, owner)
			s.observe(ctx, operation, spec, "compensation.network", cleanupStarted, cleanupErr)
			if cleanupErr != nil {
				pending.Network = true
				err = errors.Join(err, fmt.Errorf("回滚用户网络失败: %w", cleanupErr))
			}
		}
		if pending.Volume || pending.Network {
			if jerr := s.markPending(pending); jerr != nil {
				err = errors.Join(err, fmt.Errorf("写入补偿日志失败: %w", jerr))
			}
		}
		return Instance{}, orchError("创建实例", err, spec.InstanceID, spec.UserID, spec.Image, "")
	}
	return s.Get(ctx, spec.InstanceID)
}

func (s *Service) ensureNetwork(ctx context.Context, userID string) (string, bool, error) {
	if tracked, ok := s.network.(trackedNetwork); ok {
		return tracked.EnsureTracked(ctx, userID)
	}
	name, err := s.network.Ensure(ctx, userID)
	return name, false, err
}

func (s *Service) cleanupCreatedNetwork(parent context.Context, userID string) error {
	tracked, ok := s.network.(trackedNetwork)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	err := tracked.ReleaseCreated(ctx, userID)
	if sandboxerr.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Service) applyDefaults(spec Spec) Spec {
	if spec.Memory == 0 && s.defaultMemory > 0 {
		spec.Memory = s.defaultMemory
	}
	if spec.NanoCPUs == 0 && s.defaultNanoCPUs > 0 {
		spec.NanoCPUs = s.defaultNanoCPUs
	}
	return spec
}

func (s *Service) observe(ctx context.Context, operation string, spec Spec, stage string, started time.Time, err error) {
	s.emit(ctx, OperationEvent{
		Operation:  operation,
		Stage:      stage,
		InstanceID: spec.InstanceID,
		UserID:     spec.UserID,
		Duration:   time.Since(started),
		Kind:       sandboxerr.Classify(err),
		Err:        err,
	})
}

func (s *Service) emit(ctx context.Context, event OperationEvent) {
	if s == nil || s.observer == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		s.observer.Observe(ctx, event)
	}()
}

func (s *Service) ensureWorkspace(ctx context.Context, instanceID string, labels map[string]string) (Volume, bool, error) {
	if tracked, ok := s.storage.(trackedStorage); ok {
		return tracked.EnsureTracked(ctx, instanceID, labels)
	}
	vol, err := s.storage.Ensure(ctx, instanceID, labels)
	return vol, false, err
}

func (s *Service) cleanupFailedCreate(parent context.Context, instanceID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	err := s.storage.Destroy(ctx, instanceID, true)
	if sandboxerr.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Service) Start(ctx context.Context, instanceID string) (result Instance, retErr error) {
	operationStarted := time.Now()
	eventSpec := Spec{InstanceID: strings.TrimSpace(instanceID)}
	s.observe(ctx, "start", eventSpec, "operation.start", operationStarted, nil)
	defer func() { s.observe(ctx, "start", eventSpec, "operation.end", operationStarted, retErr) }()
	unlock := s.mutations.lock(strings.TrimSpace(instanceID))
	defer unlock()
	inst, err := s.requireInstance(ctx, "启动实例", instanceID)
	if err != nil {
		return Instance{}, err
	}
	eventSpec.UserID, eventSpec.Image = inst.UserID, inst.Image
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

func (s *Service) Stop(ctx context.Context, instanceID string, opts StopOptions) (result Instance, retErr error) {
	operationStarted := time.Now()
	eventSpec := Spec{InstanceID: strings.TrimSpace(instanceID)}
	s.observe(ctx, "stop", eventSpec, "operation.start", operationStarted, nil)
	defer func() { s.observe(ctx, "stop", eventSpec, "operation.end", operationStarted, retErr) }()
	unlock := s.mutations.lock(strings.TrimSpace(instanceID))
	defer unlock()
	inst, err := s.requireInstance(ctx, "停止实例", instanceID)
	if err != nil {
		return Instance{}, err
	}
	eventSpec.UserID, eventSpec.Image = inst.UserID, inst.Image
	if inst.Status == StatusDestroyed || inst.ContainerID == "" {
		return inst, nil
	}
	if err := s.runtime.Stop(ctx, inst.ContainerID, opts); err != nil {
		return Instance{}, orchError("停止实例", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	return s.Get(ctx, instanceID)
}

func (s *Service) Destroy(ctx context.Context, instanceID string, opts DestroyOptions) (retErr error) {
	operationStarted := time.Now()
	eventSpec := Spec{InstanceID: strings.TrimSpace(instanceID)}
	s.observe(ctx, "destroy", eventSpec, "operation.start", operationStarted, nil)
	defer func() {
		s.observe(ctx, "destroy", eventSpec, "operation.end", operationStarted, retErr)
	}()
	if err := s.require(); err != nil {
		return err
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return orchError("销毁实例",
			sandboxerr.WrapKind("instance.destroy", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance id is empty", sandboxerr.ErrInvalid)),
			"", "", "", "")
	}
	unlock := s.mutations.lock(instanceID)
	defer unlock()

	inst, err := s.registry.Get(ctx, instanceID)
	if err != nil && !sandboxerr.IsNotFound(err) {
		return orchError("销毁实例", err, instanceID, "", "", "")
	}
	eventSpec.UserID = inst.UserID
	eventSpec.Image = inst.Image
	var cleanupErrs []error
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelCleanup()
	if err == nil && inst.ContainerID != "" {
		if stopErr := s.runtime.Stop(ctx, inst.ContainerID, StopOptions{}); stopErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("停止容器: %w", stopErr))
		}
		if removeErr := s.runtime.Remove(cleanupCtx, inst.ContainerID); removeErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("删除容器: %w", removeErr))
		}
	}
	if storageErr := s.storage.Destroy(cleanupCtx, instanceID, opts.Purge); storageErr != nil && !sandboxerr.IsNotFound(storageErr) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("清理工作区卷: %w", storageErr))
	}
	if inst.UserID != "" || inst.Spec.InstanceID != "" {
		owner := networkOwner(inst.Spec)
		if owner == "" {
			owner = inst.UserID
		}
		if networkErr := s.network.Release(cleanupCtx, owner); networkErr != nil && !sandboxerr.IsNotFound(networkErr) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("释放用户网络: %w", networkErr))
		}
	}
	if len(cleanupErrs) > 0 {
		return orchError("销毁实例", errors.Join(cleanupErrs...), instanceID, inst.UserID, inst.Image, inst.Status)
	}
	s.clearPending(instanceID)
	return nil
}

func (s *Service) Restore(ctx context.Context, instanceID string, opts RestoreOptions) (result Instance, retErr error) {
	operationStarted := time.Now()
	eventSpec := Spec{InstanceID: strings.TrimSpace(instanceID)}
	s.observe(ctx, "restore", eventSpec, "operation.start", operationStarted, nil)
	defer func() {
		s.observe(ctx, "restore", eventSpec, "operation.end", operationStarted, retErr)
	}()
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
	unlock := s.mutations.lock(instanceID)
	defer unlock()
	inst, spec, err := s.loadSpec(ctx, instanceID)
	if err != nil {
		return Instance{}, orchError("还原实例", err, instanceID, "", "", "")
	}
	eventSpec = spec
	var cleanupErrs []error
	if inst.ContainerID != "" {
		if stopErr := s.runtime.Stop(ctx, inst.ContainerID, StopOptions{}); stopErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("停止旧容器: %w", stopErr))
		}
		if removeErr := s.runtime.Remove(ctx, inst.ContainerID); removeErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("删除旧容器: %w", removeErr))
			return Instance{}, orchError("还原实例", errors.Join(cleanupErrs...), instanceID, spec.UserID, spec.Image, inst.Status)
		}
	}
	if opts.ResetVolume {
		if storageErr := s.storage.Destroy(ctx, instanceID, true); storageErr != nil && !sandboxerr.IsNotFound(storageErr) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("重置工作区卷: %w", storageErr))
			return Instance{}, orchError("还原实例", errors.Join(cleanupErrs...), instanceID, spec.UserID, spec.Image, inst.Status)
		}
	}
	restored, err := s.provisionFor(ctx, spec, "restore")
	if err != nil {
		cleanupErrs = append(cleanupErrs, err)
		return Instance{}, orchError("还原实例", errors.Join(cleanupErrs...), instanceID, spec.UserID, spec.Image, inst.Status)
	}
	s.clearPending(instanceID)
	if len(cleanupErrs) > 0 {
		return restored, orchError("还原实例", errors.Join(cleanupErrs...), instanceID, spec.UserID, spec.Image, inst.Status)
	}
	return restored, nil
}

func (s *Service) Restart(ctx context.Context, instanceID string, opts StopOptions) (result Instance, retErr error) {
	operationStarted := time.Now()
	eventSpec := Spec{InstanceID: strings.TrimSpace(instanceID)}
	s.observe(ctx, "restart", eventSpec, "operation.start", operationStarted, nil)
	defer func() { s.observe(ctx, "restart", eventSpec, "operation.end", operationStarted, retErr) }()
	unlock := s.mutations.lock(strings.TrimSpace(instanceID))
	defer unlock()
	inst, err := s.requireInstance(ctx, "重启实例", instanceID)
	if err != nil {
		return Instance{}, err
	}
	eventSpec.UserID, eventSpec.Image = inst.UserID, inst.Image
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
	if err := ValidateProbe(inst, probe); err != nil {
		return Instance{}, orchError("等待实例就绪", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	}
	waitStarted := time.Now()
	attempt := 0
	emitRetry := func(checkErr error) {
		attempt++
		s.emit(ctx, OperationEvent{
			Operation:  "wait",
			Stage:      "wait.retry",
			InstanceID: inst.ID,
			UserID:     inst.UserID,
			Duration:   time.Since(waitStarted),
			Kind:       sandboxerr.Classify(checkErr),
			Err:        checkErr,
			Attempt:    attempt,
			Detail:     checkErr.Error(),
		})
	}
	ticker := time.NewTicker(s.waitEvery)
	defer ticker.Stop()
	if err := s.health.Check(ctx, inst, probe); err == nil {
		return inst, nil
	} else if isPermanentProbeError(err) {
		return Instance{}, orchError("等待实例就绪", err, inst.ID, inst.UserID, inst.Image, inst.Status)
	} else {
		emitRetry(err)
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
			inst = cur
			if err := s.health.Check(ctx, cur, probe); err == nil {
				return cur, nil
			} else if isPermanentProbeError(err) {
				return Instance{}, orchError("等待实例就绪", err, cur.ID, cur.UserID, cur.Image, cur.Status)
			} else {
				emitRetry(err)
			}
		}
	}
}

func isPermanentProbeError(err error) bool {
	if err == nil {
		return false
	}
	if IsProbeNotReady(err) {
		return false
	}
	if sandboxerr.IsUnauthorized(err) || sandboxerr.IsForbidden(err) {
		return true
	}
	return sandboxerr.IsInvalid(err)
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
	if _, err := EncodeSpecChecked(spec); err != nil {
		return err
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
