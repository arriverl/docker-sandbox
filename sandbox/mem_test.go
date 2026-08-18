package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

type memWorld struct {
	mu      sync.Mutex
	seq     int
	boxes   map[string]*memBox
	byName  map[string]string
	vols    map[string]*Volume
	nets    map[string]string
	healthN int
	health  func(inst Instance, probe Probe, n int) error
}

type memBox struct {
	RuntimeContainer
}

func newMemWorld() *memWorld {
	return &memWorld{
		boxes:  map[string]*memBox{},
		byName: map[string]string{},
		vols:   map[string]*Volume{},
		nets:   map[string]string{},
	}
}

type memRuntime struct{ *memWorld }
type memNetwork struct{ *memWorld }
type memStorage struct{ *memWorld }
type memRegistry struct{ *memWorld }
type memHealth struct{ *memWorld }

func (m *memWorld) plugins() Plugins {
	return Plugins{
		Runtime:  memRuntime{m},
		Network:  memNetwork{m},
		Storage:  memStorage{m},
		Registry: memRegistry{m},
		Health:   memHealth{m},
	}
}

func (r memRuntime) Create(_ context.Context, req RuntimeCreate) (RuntimeContainer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[req.Name]; ok {
		return RuntimeContainer{}, sandboxerr.WrapKind("runtime.create", sandboxerr.KindConflict,
			fmt.Errorf("%w: container name exists", sandboxerr.ErrConflict))
	}
	r.seq++
	id := fmt.Sprintf("cid-%d", r.seq)
	var ports []int
	for i := range req.Expose {
		ports = append(ports, 20000+i)
	}
	labels := cloneMap(req.Labels)
	if len(ports) > 0 {
		labels[LabelHostPorts] = joinHostPorts(ports)
	}
	box := &memBox{RuntimeContainer: RuntimeContainer{
		ID:        id,
		Name:      req.Name,
		Status:    StatusCreated,
		Image:     req.Image,
		Labels:    labels,
		HostPorts: ports,
		Network:   req.Network,
	}}
	r.boxes[id] = box
	r.byName[req.Name] = id
	return box.RuntimeContainer, nil
}

func (r memRuntime) Start(_ context.Context, containerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	box, ok := r.boxes[containerID]
	if !ok {
		return sandboxerr.WrapKind("runtime.start", sandboxerr.KindNotFound, sandboxerr.ErrNotFound)
	}
	box.Status = StatusRunning
	return nil
}

func (r memRuntime) Stop(_ context.Context, containerID string, _ StopOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if box, ok := r.boxes[containerID]; ok {
		box.Status = StatusExited
	}
	return nil
}

func (r memRuntime) Remove(_ context.Context, containerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	box, ok := r.boxes[containerID]
	if !ok {
		return nil
	}
	delete(r.boxes, containerID)
	delete(r.byName, box.Name)
	return nil
}

func (r memRuntime) Inspect(_ context.Context, containerID string) (RuntimeContainer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	box, ok := r.boxes[containerID]
	if !ok {
		return RuntimeContainer{}, sandboxerr.WrapKind("runtime.inspect", sandboxerr.KindNotFound, sandboxerr.ErrNotFound)
	}
	return box.RuntimeContainer, nil
}

func (r memRuntime) Exec(_ context.Context, containerID string, cmd []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.boxes[containerID]; !ok {
		return sandboxerr.WrapKind("runtime.exec", sandboxerr.KindNotFound, sandboxerr.ErrNotFound)
	}
	if len(cmd) == 1 && cmd[0] == "fail" {
		return fmt.Errorf("exec failed")
	}
	return nil
}

func (n memNetwork) Ensure(ctx context.Context, userID string) (string, error) {
	name, _, err := n.EnsureTracked(ctx, userID)
	return name, err
}

func (n memNetwork) EnsureTracked(_ context.Context, userID string) (string, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if name, ok := n.nets[userID]; ok {
		return name, false, nil
	}
	name := "sandbox-net-" + sanitizeName(userID)
	n.nets[userID] = name
	return name, true, nil
}

func (n memNetwork) ReleaseCreated(ctx context.Context, userID string) error {
	return n.Release(ctx, userID)
}

func (n memNetwork) Release(_ context.Context, userID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, box := range n.boxes {
		if box.Labels[LabelUser] == userID && (box.Status == StatusRunning || box.Status == StatusPaused) {
			return nil
		}
	}
	delete(n.nets, userID)
	return nil
}

func (s memStorage) Ensure(ctx context.Context, instanceID string, labels map[string]string) (Volume, error) {
	vol, _, err := s.EnsureTracked(ctx, instanceID, labels)
	return vol, err
}

func (s memStorage) EnsureTracked(_ context.Context, instanceID string, labels map[string]string) (Volume, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vol, ok := s.vols[instanceID]; ok {
		if vol.Labels == nil {
			vol.Labels = map[string]string{}
		}
		for k, v := range labels {
			if strings.TrimSpace(k) != "" {
				vol.Labels[k] = v
			}
		}
		return *vol, false, nil
	}
	vol := &Volume{
		Name:       "sandbox-ws-" + sanitizeName(instanceID),
		InstanceID: instanceID,
		MountPath:  "/workspace",
		Labels:     cloneMap(labels),
	}
	if vol.Labels == nil {
		vol.Labels = map[string]string{}
	}
	vol.Labels[LabelRole] = RoleWorkspace
	vol.Labels[LabelInstance] = instanceID
	s.vols[instanceID] = vol
	return *vol, true, nil
}

func (s memStorage) Inspect(_ context.Context, instanceID string) (Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vol, ok := s.vols[instanceID]
	if !ok {
		return Volume{}, sandboxerr.WrapKind("storage.inspect", sandboxerr.KindNotFound, sandboxerr.ErrNotFound)
	}
	return *vol, nil
}

func (s memStorage) Destroy(_ context.Context, instanceID string, purge bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !purge {
		return nil
	}
	delete(s.vols, instanceID)
	return nil
}

func (s memStorage) List(_ context.Context) ([]Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Volume, 0, len(s.vols))
	for _, v := range s.vols {
		out = append(out, *v)
	}
	return out, nil
}

func (r memRegistry) Get(_ context.Context, instanceID string) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, box := range r.boxes {
		if box.Labels[LabelInstance] == instanceID {
			return instanceFromBox(box), nil
		}
	}
	if vol, ok := r.vols[instanceID]; ok {
		spec, _ := DecodeSpec(vol.Labels[LabelSpec])
		return Instance{
			ID:     instanceID,
			Name:   ContainerName(DefaultPrefix, instanceID),
			UserID: spec.UserID,
			Status: StatusDestroyed,
			Image:  spec.Image,
			Volume: vol.Name,
			Spec:   spec,
		}, nil
	}
	return Instance{}, sandboxerr.WrapKind("registry.get", sandboxerr.KindNotFound, sandboxerr.ErrNotFound)
}

func (r memRegistry) List(_ context.Context) ([]Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]struct{}{}
	var out []Instance
	for _, box := range r.boxes {
		id := box.Labels[LabelInstance]
		seen[id] = struct{}{}
		out = append(out, instanceFromBox(box))
	}
	for id, vol := range r.vols {
		if _, ok := seen[id]; ok {
			continue
		}
		spec, _ := DecodeSpec(vol.Labels[LabelSpec])
		out = append(out, Instance{
			ID:     id,
			Name:   ContainerName(DefaultPrefix, id),
			UserID: spec.UserID,
			Status: StatusDestroyed,
			Image:  spec.Image,
			Volume: vol.Name,
			Spec:   spec,
		})
	}
	return out, nil
}

func (h memHealth) Check(_ context.Context, inst Instance, probe Probe) error {
	h.mu.Lock()
	h.healthN++
	n := h.healthN
	fn := h.health
	h.mu.Unlock()
	if fn != nil {
		return fn(inst, probe, n)
	}
	return nil
}

func instanceFromBox(box *memBox) Instance {
	spec, _ := DecodeSpec(box.Labels[LabelSpec])
	return Instance{
		ID:          box.Labels[LabelInstance],
		Name:        box.Name,
		UserID:      box.Labels[LabelUser],
		Status:      box.Status,
		Image:       box.Image,
		Network:     box.Network,
		Volume:      box.Labels[LabelVolume],
		HostPorts:   box.HostPorts,
		ContainerID: box.ID,
		Spec:        spec,
	}
}
