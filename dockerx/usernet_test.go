package dockerx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestUserSandbox_RetriesPortBindingConflictWithoutLeaseLeak(t *testing.T) {
	fake := &fakeEngine{
		createID:      "c1",
		createErrOnce: errdefs.Conflict(errors.New("Bind for 0.0.0.0 failed: port is already allocated")),
	}
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 20000, Checker: &fakeOccupancy{}})
	sb := dockerx.NewUserSandbox(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{
		Prefix: "sandbox",
		Pool:   pool,
	})

	got, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:  "alpine",
		UserID: "alice",
		Name:   "box",
		Expose: []string{"80"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(got.HostPorts) != 1 {
		t.Fatalf("HostPorts=%v", got.HostPorts)
	}
	if got.HostPorts[0] == 20000 {
		t.Fatalf("retry reused conflicted port: %v", got.HostPorts)
	}
	if got := pool.InUse(); got != 1 {
		t.Fatalf("pool in use=%d want 1", got)
	}
	if got := fake.countOps("create"); got != 2 {
		t.Fatalf("container create count=%d want 2", got)
	}
}

func TestUserSandbox_RejectsMultiPortWhenPoolRestricted(t *testing.T) {
	fake := &fakeEngine{createID: "c1"}
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 28000, Checker: &fakeOccupancy{}})
	for i := 0; i < dockerx.PortPoolRestrictMark(dockerx.PortPoolSize); i++ {
		if _, err := pool.Allocate("fill"); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	observer := &recordingSandboxObserver{}
	sb := dockerx.NewUserSandbox(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{
		Prefix:   "sandbox",
		Pool:     pool,
		Observer: observer,
	})
	inUse := pool.InUse()

	_, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:  "alpine",
		UserID: "alice",
		Name:   "box",
		Expose: []string{"80", "443"},
	})
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("Create=%v want conflict", err)
	}
	if pool.InUse() != inUse {
		t.Fatalf("create must not leak ports: in use=%d want %d", pool.InUse(), inUse)
	}
	if fake.countOps("create") != 0 {
		t.Fatalf("restricted create must not call Docker create")
	}
	if !observer.hasStage("portpool.restrict") {
		t.Fatalf("observer stages=%v", observer.stages)
	}

	got, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:  "alpine",
		UserID: "alice",
		Name:   "single",
		Expose: []string{"80"},
	})
	if err != nil {
		t.Fatalf("single-port Create: %v", err)
	}
	if len(got.HostPorts) != 1 {
		t.Fatalf("HostPorts=%v", got.HostPorts)
	}
}

type recordingSandboxObserver struct {
	stages []string
}

func (o *recordingSandboxObserver) Observe(_ context.Context, event sandbox.OperationEvent) {
	o.stages = append(o.stages, event.Stage)
}

func (o *recordingSandboxObserver) hasStage(stage string) bool {
	for _, got := range o.stages {
		if got == stage {
			return true
		}
	}
	return false
}

func newUserSandbox(t *testing.T, fake *fakeEngine) *dockerx.UserSandbox {
	t.Helper()
	return dockerx.NewUserSandbox(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{
		Prefix: "sandbox",
		Pool:   dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 20000, Checker: &fakeOccupancy{}}),
	})
}

func TestUserNetworkName(t *testing.T) {
	if got := dockerx.UserNetworkName("sandbox", "alice"); got != "sandbox-net-alice" {
		t.Fatalf("name = %q", got)
	}
	a := dockerx.UserNetworkName("sandbox", "user@1")
	b := dockerx.UserNetworkName("sandbox", "user-1")
	if a == b {
		t.Fatalf("sanitized ids must not collide: %q", a)
	}
	if !strings.HasPrefix(a, "sandbox-net-user-1-") {
		t.Fatalf("dirty id should keep sanitized prefix, got %q", a)
	}
	if b != "sandbox-net-user-1" {
		t.Fatalf("clean id = %q", b)
	}
}

func TestUserSandbox_SameUserSharesNetwork(t *testing.T) {
	fake := &fakeEngine{createID: "c1"}
	sb := newUserSandbox(t, fake)
	ctx := context.Background()

	a, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1", Name: "a"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	fake.createID = "c2"
	b, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1", Name: "b"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.Network != "sandbox-net-u1" || b.Network != a.Network {
		t.Fatalf("networks a=%q b=%q", a.Network, b.Network)
	}
	creates := 0
	for _, op := range fake.ops {
		if op == "net-create:sandbox-net-u1" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("network creates = %d, want 1; ops=%v", creates, fake.ops)
	}
}

func TestUserSandbox_DifferentUsersIsolated(t *testing.T) {
	fake := &fakeEngine{createID: "c1"}
	sb := newUserSandbox(t, fake)
	ctx := context.Background()

	a, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1"})
	if err != nil {
		t.Fatalf("u1: %v", err)
	}
	fake.createID = "c2"
	b, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u2"})
	if err != nil {
		t.Fatalf("u2: %v", err)
	}
	if a.Network == b.Network {
		t.Fatalf("users must not share network: %q", a.Network)
	}
	if a.Network != "sandbox-net-u1" || b.Network != "sandbox-net-u2" {
		t.Fatalf("got %q %q", a.Network, b.Network)
	}
	if fake.lastHost == nil || string(fake.lastHost.NetworkMode) != "sandbox-net-u2" {
		t.Fatalf("last network mode = %q", fake.lastHost.NetworkMode)
	}
}

func TestUserSandbox_AllocatesHostPortFromPool(t *testing.T) {
	fake := &fakeEngine{createID: "c-port"}
	sb := newUserSandbox(t, fake)
	got, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:  "alpine",
		UserID: "u1",
		Expose: []string{"80/tcp"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(got.HostPorts) != 1 || got.HostPorts[0] != 20000 {
		t.Fatalf("host ports = %v", got.HostPorts)
	}
	bindings := fake.lastHost.PortBindings["80/tcp"]
	if len(bindings) == 0 || bindings[0].HostPort != "20000" {
		t.Fatalf("bindings = %#v", fake.lastHost.PortBindings)
	}
}

func TestUserSandbox_SeedsHostPortsFromDocker(t *testing.T) {
	fake := &fakeEngine{
		createID: "c-new",
		containers: []container.Summary{{
			ID: "already",
			Labels: map[string]string{
				"sandbox.hostports": "20000,20001",
				"sandbox.userid":    "alice",
			},
		}},
	}
	sb := newUserSandbox(t, fake)
	got, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:  "alpine",
		UserID: "alice",
		Expose: []string{"80/tcp"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(got.HostPorts) != 1 || got.HostPorts[0] != 20002 {
		t.Fatalf("host ports = %v, want [20002]", got.HostPorts)
	}
}

func TestUserSandbox_ReapKeepsNetworkIfRunning(t *testing.T) {
	fake := &fakeEngine{createID: "run-1", state: container.StateRunning}
	sb := newUserSandbox(t, fake)
	ctx := context.Background()
	if _, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1", Name: "run-1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sb.MarkInactive("u1")
	if err := sb.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	for _, op := range fake.ops {
		if strings.HasPrefix(op, "net-remove:") {
			t.Fatalf("should not remove network while running, ops=%v", fake.ops)
		}
	}
}

func TestUserSandbox_ReapReleasesNetworkWhenInactiveAndStopped(t *testing.T) {
	fake := &fakeEngine{createID: "ex-1", state: container.StateExited}
	sb := newUserSandbox(t, fake)
	ctx := context.Background()
	if _, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1", Name: "ex-1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sb.MarkInactive("u1")
	if err := sb.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	found := false
	for _, op := range fake.ops {
		if op == "net-remove:sandbox-net-u1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want net-remove, ops=%v", fake.ops)
	}
}

func TestUserSandbox_ReapSkipsActiveUser(t *testing.T) {
	fake := &fakeEngine{createID: "ex-2", state: container.StateExited}
	sb := newUserSandbox(t, fake)
	ctx := context.Background()
	if _, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sb.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	for _, op := range fake.ops {
		if strings.HasPrefix(op, "net-remove:") {
			t.Fatalf("active user network must stay, ops=%v", fake.ops)
		}
	}
}

func TestUserSandbox_EnsureNetworkRejectsForeignLabel(t *testing.T) {
	fake := &fakeEngine{}
	_, err := fake.NetworkCreate(context.Background(), "sandbox-net-alice", network.CreateOptions{
		Labels: map[string]string{"sandbox.userid": "other", "sandbox.role": "usernet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sb := newUserSandbox(t, fake)
	_, err = sb.EnsureNetwork(context.Background(), "alice")
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err=%v want conflict", err)
	}
}

func TestUserSandbox_ReapKeepsCreatedContainer(t *testing.T) {
	fake := &fakeEngine{createID: "new-1", state: container.StateCreated}
	sb := newUserSandbox(t, fake)
	ctx := context.Background()
	if _, err := sb.Create(ctx, dockerx.CreateOptions{Image: "alpine", UserID: "u1", Name: "new-1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	sb.MarkInactive("u1")
	if err := sb.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	for _, op := range fake.ops {
		if strings.HasPrefix(op, "net-remove:") {
			t.Fatalf("created container must keep network, ops=%v", fake.ops)
		}
	}
}

func TestUserSandbox_ReapScansDockerNetworks(t *testing.T) {
	fake := &fakeEngine{}
	if _, err := fake.NetworkCreate(context.Background(), "sandbox-net-ghost", network.CreateOptions{
		Labels: map[string]string{"sandbox.userid": "ghost", "sandbox.role": "usernet"},
	}); err != nil {
		t.Fatal(err)
	}
	sb := newUserSandbox(t, fake)
	if err := sb.Reap(context.Background()); err != nil {
		t.Fatalf("reap: %v", err)
	}
	found := false
	for _, op := range fake.ops {
		if op == "net-remove:sandbox-net-ghost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want reap of labeled network without in-memory state, ops=%v", fake.ops)
	}
}

func TestUserSandbox_ActiveHoldSurvivesNewClient(t *testing.T) {
	fake := &fakeEngine{}
	if _, err := fake.NetworkCreate(context.Background(), "sandbox-net-ghost", network.CreateOptions{
		Labels: map[string]string{"sandbox.userid": "ghost", "sandbox.role": "usernet"},
	}); err != nil {
		t.Fatal(err)
	}
	first := newUserSandbox(t, fake)
	first.MarkActive("ghost")
	second := newUserSandbox(t, fake)
	if err := second.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, op := range fake.ops {
		if op == "net-remove:sandbox-net-ghost" {
			t.Fatalf("hold volume should keep network, ops=%v", fake.ops)
		}
	}
}

func TestUserSandbox_CreateRequiresUser(t *testing.T) {
	sb := newUserSandbox(t, &fakeEngine{})
	_, err := sb.Create(context.Background(), dockerx.CreateOptions{Image: "alpine"})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
}
