package dockerx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func newWorkspace(t *testing.T, fake *fakeEngine) *dockerx.Workspace {
	t.Helper()
	return dockerx.NewWorkspace(dockerx.NewWithAPI(fake), dockerx.WorkspaceConfig{Prefix: "sandbox"})
}

func TestWorkspaceVolumeName(t *testing.T) {
	if got := dockerx.WorkspaceVolumeName("sandbox", "inst-1"); got != "sandbox-ws-inst-1" {
		t.Fatalf("name = %q", got)
	}
	if got := dockerx.WorkspaceVolumeName("sandbox", "a@b"); got != "sandbox-ws-a-b" {
		t.Fatalf("sanitized = %q", got)
	}
}

func TestWorkspace_EnsureCreatesThenReuses(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	ctx := context.Background()

	a, err := ws.Ensure(ctx, "box1")
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	if a.Name != "sandbox-ws-box1" || a.Reused || a.InstanceID != "box1" {
		t.Fatalf("first = %+v", a)
	}
	b, err := ws.Ensure(ctx, "box1")
	if err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if !b.Reused || b.Name != a.Name {
		t.Fatalf("reuse = %+v", b)
	}
	creates := 0
	for _, op := range fake.ops {
		if op == "vol-create:sandbox-ws-box1" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("creates = %d, ops=%v", creates, fake.ops)
	}
}

func TestWorkspace_InspectMissingInChinese(t *testing.T) {
	ws := newWorkspace(t, &fakeEngine{})
	_, err := ws.Inspect(context.Background(), "missing")
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "工作区") || !strings.Contains(msg, "sandbox-ws-missing") {
		t.Fatalf("want volume context, got %q", msg)
	}
}

func TestWorkspace_EnsureLabelsPersistsSpec(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	got, err := ws.EnsureLabels(context.Background(), "box1", map[string]string{
		"sandbox.spec": `{"instance_id":"box1","user_id":"alice","image":"alpine:3.20"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["sandbox.spec"] == "" || got.Labels["sandbox.instance"] != "box1" {
		t.Fatalf("labels=%v", got.Labels)
	}
}

func TestWorkspace_RequiresInstanceID(t *testing.T) {
	ws := newWorkspace(t, &fakeEngine{})
	_, err := ws.Ensure(context.Background(), "  ")
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
}

func TestWorkspace_DestroyKeepsVolume(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	ctx := context.Background()
	if _, err := ws.Ensure(ctx, "keep"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got, err := ws.Destroy(ctx, "keep", dockerx.DestroyOptions{RemoveVolume: false})
	if err != nil {
		t.Fatalf("destroy keep: %v", err)
	}
	if !got.Kept || got.Name != "sandbox-ws-keep" {
		t.Fatalf("destroy = %+v", got)
	}
	if _, err := ws.Inspect(ctx, "keep"); err != nil {
		t.Fatalf("volume should remain: %v", err)
	}
}

func TestWorkspace_DestroyRemovesVolume(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	ctx := context.Background()
	if _, err := ws.Ensure(ctx, "gone"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got, err := ws.Destroy(ctx, "gone", dockerx.DestroyOptions{RemoveVolume: true})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got.Kept || got.Name != "sandbox-ws-gone" {
		t.Fatalf("destroy = %+v", got)
	}
	_, err = ws.Inspect(ctx, "gone")
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("inspect after destroy = %v, want not_found", err)
	}
}

func TestWorkspace_DestroyMissingVolumeIsOK(t *testing.T) {
	ws := newWorkspace(t, &fakeEngine{})
	got, err := ws.Destroy(context.Background(), "nope", dockerx.DestroyOptions{RemoveVolume: true})
	if err != nil {
		t.Fatalf("destroy missing: %v", err)
	}
	if got.Name != "sandbox-ws-nope" || got.Kept {
		t.Fatalf("got %+v", got)
	}
}

func TestWorkspace_RestoreKeepReuses(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	ctx := context.Background()
	first, err := ws.Ensure(ctx, "r1")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got, err := ws.Restore(ctx, "r1", dockerx.RestoreOptions{KeepVolume: true})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !got.Reused || got.Name != first.Name {
		t.Fatalf("restore = %+v", got)
	}
}

func TestWorkspace_RestoreWithoutKeepRecreates(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	ctx := context.Background()
	if _, err := ws.Ensure(ctx, "r2"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got, err := ws.Restore(ctx, "r2", dockerx.RestoreOptions{KeepVolume: false})
	if err != nil {
		t.Fatalf("restore reset: %v", err)
	}
	if got.Reused || got.Name != "sandbox-ws-r2" {
		t.Fatalf("restore = %+v", got)
	}
	removed, created := 0, 0
	for _, op := range fake.ops {
		if op == "vol-remove:sandbox-ws-r2" {
			removed++
		}
		if op == "vol-create:sandbox-ws-r2" {
			created++
		}
	}
	if removed < 1 || created < 2 {
		t.Fatalf("want remove+recreate, ops=%v", fake.ops)
	}
}

func TestWorkspace_ListAndBindSpec(t *testing.T) {
	fake := &fakeEngine{}
	ws := newWorkspace(t, fake)
	ctx := context.Background()
	if _, err := ws.Ensure(ctx, "a"); err != nil {
		t.Fatalf("a: %v", err)
	}
	if _, err := ws.Ensure(ctx, "b"); err != nil {
		t.Fatalf("b: %v", err)
	}
	list, err := ws.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if spec := ws.BindSpec("a"); spec != "sandbox-ws-a:/workspace" {
		t.Fatalf("bind = %q", spec)
	}
}

func TestUserSandbox_CreateMountsWorkspace(t *testing.T) {
	fake := &fakeEngine{createID: "cid-ws"}
	sb := dockerx.NewUserSandbox(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{
		Prefix: "sandbox",
		Pool:   dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 20000, Checker: &fakeOccupancy{}}),
	})
	got, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:      "alpine",
		UserID:     "u1",
		InstanceID: "job9",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.Volume != "sandbox-ws-job9" || got.Network != "sandbox-net-u1" {
		t.Fatalf("result = %+v", got)
	}
	found := false
	for _, b := range fake.lastHost.Binds {
		if b == "sandbox-ws-job9:/workspace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("binds = %#v", fake.lastHost.Binds)
	}
}
