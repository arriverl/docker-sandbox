package dockerx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

const (
	DefaultNetworkPrefix = "sandbox"
	labelUserID          = "sandbox.userid"
	labelNetwork         = "sandbox.network"
	labelHostPorts       = "sandbox.hostports"
	labelRole            = "sandbox.role"
	roleUserNet          = "usernet"
	roleInstNet          = "instnet"
	roleHold             = "hold"
)

// UserSandboxConfig 描述按用户隔离的网络与端口池。
type UserSandboxConfig struct {
	Prefix          string
	Pool            *PortPool
	DefaultMemory   int64
	DefaultNanoCPUs int64
	MaxInstances    int
	Observer        sandbox.Observer
	JournalPath     string
}

// UserSandbox 为每个用户维护独立 Docker 网络，并用端口池分配宿主机端口。
type UserSandbox struct {
	client          *Client
	pool            *PortPool
	prefix          string
	workspace       *Workspace
	defaultMemory   int64
	defaultNanoCPUs int64
	maxInstances    int
	observer        sandbox.Observer
	journalPath     string

	mu     sync.Mutex
	users  map[string]*userNetState
	leases map[string][]int // container id -> host ports
}

type userNetState struct {
	network string
	active  bool
}

// NewUserSandbox 构造用户级网络/端口编排器。
func NewUserSandbox(c *Client, cfg UserSandboxConfig) *UserSandbox {
	prefix := strings.TrimSpace(cfg.Prefix)
	if prefix == "" {
		prefix = DefaultNetworkPrefix
	}
	pool := cfg.Pool
	if pool == nil {
		pool = NewPortPool(PortPoolConfig{
			LockPath: DefaultPortLockPath(prefix, DefaultPortBase),
		})
	}
	s := &UserSandbox{
		client:          c,
		pool:            pool,
		prefix:          prefix,
		workspace:       NewWorkspace(c, WorkspaceConfig{Prefix: prefix}),
		defaultMemory:   cfg.DefaultMemory,
		defaultNanoCPUs: cfg.DefaultNanoCPUs,
		maxInstances:    cfg.MaxInstances,
		observer:        cfg.Observer,
		journalPath:     cfg.JournalPath,
		users:           map[string]*userNetState{},
		leases:          map[string][]int{},
	}
	if s.client != nil && s.observer != nil {
		s.client.SetPullProgress(func(p PullProgress) {
			s.observePull(p)
		})
	}
	_ = s.SeedPorts(context.Background())
	return s
}

// SeedPorts 从 Docker 容器 labels / 端口绑定重建本进程端口池。
func (s *UserSandbox) SeedPorts(ctx context.Context) error {
	if s == nil || s.client == nil || s.pool == nil {
		return nil
	}
	items, err := s.client.ListContainers(ctx)
	if err != nil {
		return err
	}
	type seeded struct {
		id    string
		owner string
		ports []int
	}
	var found []seeded
	for _, item := range items {
		ports := hostPortsFromSummary(item)
		if len(ports) == 0 {
			continue
		}
		owner := ""
		if item.Labels != nil {
			owner = item.Labels[labelUserID]
		}
		if owner == "" {
			owner = item.ID
		}
		found = append(found, seeded{id: item.ID, owner: owner, ports: ports})
	}
	for _, item := range found {
		for _, port := range item.ports {
			s.pool.Reserve(port, item.owner)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range found {
		if item.id != "" {
			s.leases[item.id] = item.ports
		}
	}
	return nil
}

func hostPortsFromSummary(item ContainerSummary) []int {
	if item.Labels != nil {
		if raw := item.Labels[labelHostPorts]; raw != "" {
			return parseHostPorts(raw)
		}
	}
	var out []int
	seen := map[int]struct{}{}
	for _, p := range item.Ports {
		if p.PublicPort == 0 {
			continue
		}
		n := int(p.PublicPort)
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func parseHostPorts(raw string) []int {
	var out []int
	for _, p := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// InstanceNetworkOwner 返回 per-instance 网络在 Docker 网络插件中的键。
func InstanceNetworkOwner(instanceID string) string {
	return sandbox.InstanceNetworkOwner(instanceID)
}

func isInstanceNetworkOwner(owner string) bool {
	return strings.HasPrefix(owner, "i/")
}

// UserNetworkName 返回 {prefix}-net-{userId}。清洗会改写的 ID 追加短哈希，避免 user@1 与 user-1 撞名。
func UserNetworkName(prefix, userID string) string {
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultNetworkPrefix
	}
	raw := strings.TrimSpace(userID)
	cleaned := sanitizeName(raw)
	name := sanitizeName(prefix) + "-net-" + cleaned
	if cleaned != raw {
		return name + "-" + shortHash(raw)
	}
	return name
}

func holdVolumeName(prefix, userID string) string {
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultNetworkPrefix
	}
	raw := strings.TrimSpace(userID)
	cleaned := sanitizeName(raw)
	name := sanitizeName(prefix) + "-hold-" + cleaned
	if cleaned != raw {
		return name + "-" + shortHash(raw)
	}
	return name
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i, r := range s {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-'
		if ok && (i > 0 || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "user"
	}
	return out
}

// MarkActive 标记用户仍活跃。会创建持久 hold 卷，跨进程 reap 时保留空网。
func (s *UserSandbox) MarkActive(userID string) {
	s.markActive(context.Background(), userID)
}

func (s *UserSandbox) markActive(ctx context.Context, userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" || s == nil {
		return
	}
	s.mu.Lock()
	st := s.users[userID]
	if st == nil {
		st = &userNetState{network: UserNetworkName(s.prefix, userID), active: true}
		s.users[userID] = st
	} else {
		st.active = true
	}
	s.mu.Unlock()
	if s.client == nil || s.client.api == nil {
		return
	}
	name := holdVolumeName(s.prefix, userID)
	_, err := s.client.api.VolumeInspect(ctx, name)
	if err == nil {
		return
	}
	if engineKind(err) != sandboxerr.KindNotFound {
		return
	}
	_, _ = s.client.api.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		Labels: map[string]string{
			labelRole:   roleHold,
			labelUserID: userID,
		},
	})
}

// MarkInactive 标记用户不再活跃，并删除 hold 卷。
func (s *UserSandbox) MarkInactive(userID string) {
	s.markInactive(context.Background(), userID)
}

func (s *UserSandbox) markInactive(ctx context.Context, userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" || s == nil {
		return
	}
	s.mu.Lock()
	if st := s.users[userID]; st != nil {
		st.active = false
	}
	s.mu.Unlock()
	if s.client == nil || s.client.api == nil {
		return
	}
	_ = s.client.api.VolumeRemove(ctx, holdVolumeName(s.prefix, userID), true)
}

// EnsureNetwork 确保用户网络存在，同用户复用同一网络。
func (s *UserSandbox) EnsureNetwork(ctx context.Context, userID string) (string, error) {
	name, _, err := s.EnsureNetworkTracked(ctx, userID)
	return name, err
}

// EnsureNetworkTracked 确保用户网络存在，并报告本次调用是否创建了网络。
func (s *UserSandbox) EnsureNetworkTracked(ctx context.Context, userID string) (string, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", false, sandboxerr.WrapKind("network.ensure", sandboxerr.KindInvalid,
			fmt.Errorf("%w: user id is empty", sandboxerr.ErrInvalid))
	}
	name := UserNetworkName(s.prefix, userID)
	s.mu.Lock()
	st := s.users[userID]
	if st == nil {
		st = &userNetState{network: name, active: true}
		s.users[userID] = st
	} else {
		st.active = true
		if st.network == "" {
			st.network = name
		}
		name = st.network
	}
	s.mu.Unlock()

	created, err := s.claimOrCreateNetwork(ctx, userID, name)
	if err != nil {
		return "", false, err
	}
	return name, created, nil
}

func (s *UserSandbox) claimOrCreateNetwork(ctx context.Context, userID, name string) (bool, error) {
	info, err := s.client.api.NetworkInspect(ctx, name, network.InspectOptions{})
	if err == nil {
		return false, claimNetwork(name, userID, info)
	}
	if engineKind(err) != sandboxerr.KindNotFound {
		return false, wrapEngine("network.inspect", err)
	}
	_, err = s.client.api.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
		Labels: map[string]string{
			labelUserID:  userID,
			labelRole:    netRole(userID),
			labelNetwork: name,
		},
		Options: map[string]string{
			"com.docker.network.bridge.enable_icc": netICC(userID),
		},
	})
	if err == nil {
		return true, nil
	}
	if engineKind(err) != sandboxerr.KindConflict {
		return false, wrapEngine("network.create", err)
	}
	info, ierr := s.client.api.NetworkInspect(ctx, name, network.InspectOptions{})
	if ierr != nil {
		return false, wrapEngine("network.inspect", ierr)
	}
	return false, claimNetwork(name, userID, info)
}

func netRole(owner string) string {
	if isInstanceNetworkOwner(owner) {
		return roleInstNet
	}
	return roleUserNet
}

func netICC(owner string) string {
	if isInstanceNetworkOwner(owner) {
		return "false"
	}
	return "true"
}

func claimNetwork(name, userID string, info network.Inspect) error {
	owner := ""
	if info.Labels != nil {
		owner = strings.TrimSpace(info.Labels[labelUserID])
	}
	if owner == "" || owner != userID {
		return sandboxerr.WrapKind("创建用户网络", sandboxerr.KindConflict,
			fmt.Errorf("%w: network %s belongs to %q", sandboxerr.ErrConflict, name, owner))
	}
	return nil
}

// Create 在用户网络中创建容器，并从端口池分配宿主机端口。
func (s *UserSandbox) Create(ctx context.Context, opts CreateOptions) (CreateResult, error) {
	if s == nil || s.client == nil {
		return CreateResult{}, sandboxerr.WrapKind("创建容器", sandboxerr.KindInvalid,
			fmt.Errorf("创建容器失败：容器管理器未初始化"))
	}
	userID := strings.TrimSpace(opts.UserID)
	if userID == "" {
		return CreateResult{}, managerError("创建容器",
			sandboxerr.WrapKind("container.create", sandboxerr.KindInvalid,
				fmt.Errorf("%w: user id is empty", sandboxerr.ErrInvalid)),
			errorContext{})
	}
	netName := strings.TrimSpace(opts.NetworkMode)
	if netName == "" {
		got, err := s.EnsureNetwork(ctx, userID)
		if err != nil {
			return CreateResult{}, managerError("创建用户网络", err, errorContext{Ref: userID})
		}
		netName = got
	}
	opts.NetworkMode = netName
	opts.Labels = cloneLabels(opts.Labels)
	opts.Labels[labelUserID] = userID
	opts.Labels[labelNetwork] = netName

	var vol WorkspaceInfo
	if strings.TrimSpace(opts.InstanceID) != "" {
		gotVol, err := s.workspace.Attach(ctx, &opts)
		if err != nil {
			return CreateResult{}, err
		}
		vol = gotVol
	}

	basePorts := append([]string(nil), opts.Ports...)
	baseLabels := cloneLabels(opts.Labels)
	var blockedPorts []int
	for attempt := 0; attempt < 2; attempt++ {
		opts.Ports = append([]string(nil), basePorts...)
		opts.Labels = cloneLabels(baseLabels)
		var hostPorts []int
		if len(opts.Expose) > 0 {
			specs, allocErr := bindFromPool(s.pool, userID, opts.Expose)
			if allocErr != nil {
				releasePorts(s.pool, blockedPorts)
				if errors.Is(allocErr, ErrMultiPortRestricted) {
					s.observe(ctx, "create", "portpool.restrict", strings.TrimSpace(opts.Name), userID, allocErr)
				}
				return CreateResult{}, managerError("分配宿主机端口", allocErr, errorContext{Ref: userID, Image: opts.Image})
			}
			hostPorts = specs.hosts
			opts.Ports = append(opts.Ports, specs.bindings...)
			opts.Labels[labelHostPorts] = joinInts(hostPorts)
			if s.pool.Stats().Warn {
				s.observe(ctx, "create", "portpool.warn", strings.TrimSpace(opts.Name), userID, nil)
			}
		}

		got, createErr := s.client.CreateContainer(ctx, opts)
		if createErr == nil {
			releasePorts(s.pool, blockedPorts)
			s.mu.Lock()
			s.leases[got.ID] = hostPorts
			s.mu.Unlock()
			got.Network = netName
			got.HostPorts = hostPorts
			got.Volume = vol.Name
			got.VolumeReused = vol.Reused
			return got, nil
		}
		if attempt == 0 && len(hostPorts) > 0 && isPortBindingConflict(createErr) {
			blockedPorts = append(blockedPorts, hostPorts...)
			continue
		}
		releasePorts(s.pool, hostPorts)
		releasePorts(s.pool, blockedPorts)
		return CreateResult{}, managerError("创建容器", createErr, errorContext{
			Ref:   strings.TrimSpace(opts.Name),
			Image: strings.TrimSpace(opts.Image),
		})
	}
	return CreateResult{}, sandboxerr.WrapKind("创建容器", sandboxerr.KindUnknown,
		fmt.Errorf("创建容器失败：端口重试流程未返回结果"))
}

func (s *UserSandbox) PortPoolStats() PortPoolStats {
	if s == nil || s.pool == nil {
		return PortPoolStats{}
	}
	return s.pool.Stats()
}

func (s *UserSandbox) observe(ctx context.Context, operation, stage, instanceID, userID string, err error) {
	if s == nil || s.observer == nil {
		return
	}
	event := sandbox.OperationEvent{
		Operation:  operation,
		Stage:      stage,
		InstanceID: instanceID,
		UserID:     userID,
		Kind:       sandboxerr.Classify(err),
		Err:        err,
	}
	func() {
		defer func() { _ = recover() }()
		s.observer.Observe(ctx, event)
	}()
}

func (s *UserSandbox) observePull(p PullProgress) {
	if s == nil || s.observer == nil {
		return
	}
	detail := strings.TrimSpace(strings.TrimSpace(p.Status) + " " + strings.TrimSpace(p.ID))
	event := sandbox.OperationEvent{
		Operation: "create",
		Stage:     "pull.progress",
		Detail:    detail,
		Current:   p.Current,
		Total:     p.Total,
	}
	func() {
		defer func() { _ = recover() }()
		s.observer.Observe(context.Background(), event)
	}()
}

func releasePorts(pool *PortPool, ports []int) {
	for _, port := range ports {
		pool.Release(port)
	}
}

func isPortBindingConflict(err error) bool {
	if !sandboxerr.IsConflict(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "port is already allocated") ||
		strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "bind for")
}

type poolBindings struct {
	hosts    []int
	bindings []string
}

func bindFromPool(pool *PortPool, owner string, expose []string) (poolBindings, error) {
	var count int
	for _, item := range expose {
		if strings.TrimSpace(item) != "" {
			count++
		}
	}
	if err := pool.Admit(count); err != nil {
		return poolBindings{}, err
	}
	var out poolBindings
	for _, item := range expose {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		proto := "tcp"
		port := item
		if i := strings.LastIndex(item, "/"); i >= 0 {
			port, proto = item[:i], strings.ToLower(item[i+1:])
		}
		if _, err := strconv.Atoi(port); err != nil {
			return poolBindings{}, sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
				fmt.Errorf("%w: invalid container port %q", sandboxerr.ErrInvalid, item))
		}
		host, err := pool.Allocate(owner)
		if err != nil {
			for _, p := range out.hosts {
				pool.Release(p)
			}
			return poolBindings{}, err
		}
		out.hosts = append(out.hosts, host)
		out.bindings = append(out.bindings, fmt.Sprintf("%d:%s/%s", host, port, proto))
	}
	return out, nil
}

func joinInts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Remove 删除容器并归还其宿主机端口，随后尝试回收空闲用户网络。
func (s *UserSandbox) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	if s == nil || s.client == nil {
		return sandboxerr.WrapKind("删除容器", sandboxerr.KindInvalid,
			fmt.Errorf("删除容器失败：容器管理器未初始化"))
	}
	info, inspectErr := s.client.InspectContainer(ctx, id)
	err := s.client.RemoveContainer(ctx, id, opts)
	if err != nil {
		return managerError("删除容器", err, errorContext{Ref: strings.TrimSpace(id)})
	}
	s.releaseLease(id, info)
	if inspectErr == nil {
		if uid := info.Labels[labelUserID]; uid != "" {
			_ = s.reapUser(ctx, uid)
		}
	}
	return nil
}

func (s *UserSandbox) releaseLease(id string, info ContainerInspect) {
	s.mu.Lock()
	ports := s.leases[id]
	delete(s.leases, id)
	s.mu.Unlock()
	if len(ports) == 0 && info.Labels != nil {
		if raw := info.Labels[labelHostPorts]; raw != "" {
			ports = parseHostPorts(raw)
		}
	}
	for _, p := range ports {
		s.pool.Release(p)
	}
}

// Reap 释放所有「用户不活跃、无 hold 卷、且无受保护容器」的用户网络。
// 从 Docker 网络标签列举，不依赖进程内 map，因此 CLI 无参 reap 有效。
func (s *UserSandbox) Reap(ctx context.Context) error {
	ids := map[string]struct{}{}
	s.mu.Lock()
	for id, st := range s.users {
		if st != nil && !st.active {
			ids[id] = struct{}{}
		}
	}
	s.mu.Unlock()

	raw, err := s.client.api.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return wrapEngine("network.list", err)
	}
	for _, item := range raw {
		info, ierr := s.client.api.NetworkInspect(ctx, item.Name, network.InspectOptions{})
		if ierr != nil {
			continue
		}
		if info.Labels[labelRole] != roleUserNet {
			continue
		}
		uid := strings.TrimSpace(info.Labels[labelUserID])
		if uid == "" {
			continue
		}
		s.mu.Lock()
		st := s.users[uid]
		active := st != nil && st.active
		s.mu.Unlock()
		if active {
			continue
		}
		ids[uid] = struct{}{}
	}

	var first error
	for id := range ids {
		if err := s.reapUser(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *UserSandbox) holdExists(ctx context.Context, userID string) bool {
	if s == nil || s.client == nil || s.client.api == nil {
		return false
	}
	_, err := s.client.api.VolumeInspect(ctx, holdVolumeName(s.prefix, userID))
	return err == nil
}

func (s *UserSandbox) reapUser(ctx context.Context, userID string) error {
	s.mu.Lock()
	st := s.users[userID]
	if st != nil && st.active {
		s.mu.Unlock()
		return nil
	}
	netName := UserNetworkName(s.prefix, userID)
	if st != nil && st.network != "" {
		netName = st.network
	}
	s.mu.Unlock()

	if s.holdExists(ctx, userID) {
		return nil
	}

	netInfo, err := s.client.api.NetworkInspect(ctx, netName, network.InspectOptions{Verbose: true})
	if err != nil {
		if engineKind(err) == sandboxerr.KindNotFound {
			s.dropUser(userID)
			return nil
		}
		return wrapEngine("network.inspect", err)
	}
	for cid := range netInfo.Containers {
		info, ierr := s.client.InspectContainer(ctx, cid)
		if ierr != nil {
			return wrapEngine("container.inspect", ierr)
		}
		if isActiveStatus(containerStatus(info)) {
			return nil
		}
	}
	for cid := range netInfo.Containers {
		_ = s.client.api.NetworkDisconnect(ctx, netName, cid, true)
	}
	if err := s.client.api.NetworkRemove(ctx, netName); err != nil && engineKind(err) != sandboxerr.KindNotFound {
		return wrapEngine("network.remove", err)
	}
	s.dropUser(userID)
	return nil
}

// TryReap 在不标记 inactive 的前提下尝试回收用户网络。
func (s *UserSandbox) TryReap(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return sandboxerr.WrapKind("释放用户网络", sandboxerr.KindInvalid,
			fmt.Errorf("%w: user id is empty", sandboxerr.ErrInvalid))
	}
	return s.reapUser(ctx, userID)
}

// ReleaseCreatedNetwork 回滚本次 provision 新建且尚未承载容器的用户网络。
func (s *UserSandbox) ReleaseCreatedNetwork(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return sandboxerr.WrapKind("释放用户网络", sandboxerr.KindInvalid,
			fmt.Errorf("%w: user id is empty", sandboxerr.ErrInvalid))
	}
	s.mu.Lock()
	if st := s.users[userID]; st != nil {
		st.active = false
	}
	s.mu.Unlock()
	return s.reapUser(ctx, userID)
}

// ReapUser 将用户标为不活跃；若无受保护容器则删除其网络。
func (s *UserSandbox) ReapUser(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return sandboxerr.WrapKind("释放用户网络", sandboxerr.KindInvalid,
			fmt.Errorf("%w: user id is empty", sandboxerr.ErrInvalid))
	}
	s.MarkInactive(userID)
	s.mu.Lock()
	if s.users[userID] == nil {
		s.users[userID] = &userNetState{network: UserNetworkName(s.prefix, userID), active: false}
	}
	s.mu.Unlock()
	return s.reapUser(ctx, userID)
}

func (s *UserSandbox) dropUser(userID string) {
	s.mu.Lock()
	delete(s.users, userID)
	s.mu.Unlock()
}

// Workspace 返回绑定到相同前缀的工作区卷管理器。
func (s *UserSandbox) Workspace() *Workspace {
	if s == nil {
		return nil
	}
	return s.workspace
}
