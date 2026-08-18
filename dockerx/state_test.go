package dockerx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestRemoveContainer_MissingIsOK(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{gone: true})
	if err := c.RemoveContainer(context.Background(), "missing", dockerx.RemoveOptions{}); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}

func TestContainerManager_RemoveMissingIsOK(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{gone: true}))
	if err := m.Remove(context.Background(), "missing", dockerx.RemoveOptions{}); err != nil {
		t.Fatalf("manager remove missing: %v", err)
	}
}

func TestStartContainer_AlreadyRunningIsOK(t *testing.T) {
	fake := &fakeEngine{state: container.StateRunning}
	c := dockerx.NewWithAPI(fake)
	if err := c.StartContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("start running: %v", err)
	}
	if strings.Contains(strings.Join(fake.ops, ","), "start:") {
		t.Fatalf("ops = %v, did not expect start", fake.ops)
	}
}

func TestStopContainer_AlreadyExitedIsOK(t *testing.T) {
	fake := &fakeEngine{state: container.StateExited}
	c := dockerx.NewWithAPI(fake)
	if err := c.StopContainer(context.Background(), "demo", dockerx.StopOptions{}); err != nil {
		t.Fatalf("stop exited: %v", err)
	}
	if strings.Contains(strings.Join(fake.ops, ","), "stop:") {
		t.Fatalf("ops = %v, did not expect stop", fake.ops)
	}
}

func TestStartContainer_PausedUnpauses(t *testing.T) {
	fake := &fakeEngine{state: container.StatePaused}
	c := dockerx.NewWithAPI(fake)
	if err := c.StartContainer(context.Background(), "demo"); err != nil {
		t.Fatalf("start paused: %v", err)
	}
	joined := strings.Join(fake.ops, ",")
	if !strings.Contains(joined, "unpause:demo") {
		t.Fatalf("ops = %v, want unpause", fake.ops)
	}
	if strings.Contains(joined, "start:") {
		t.Fatalf("ops = %v, did not expect start", fake.ops)
	}
}

func TestRemoveContainer_RunningStopsThenRemoves(t *testing.T) {
	fake := &fakeEngine{state: container.StateRunning}
	c := dockerx.NewWithAPI(fake)
	if err := c.RemoveContainer(context.Background(), "demo", dockerx.RemoveOptions{}); err != nil {
		t.Fatalf("remove running: %v", err)
	}
	joined := strings.Join(fake.ops, ",")
	if !strings.Contains(joined, "stop:demo") || !strings.Contains(joined, "remove:demo") {
		t.Fatalf("ops = %v, want stop then remove", fake.ops)
	}
}

func TestStartContainer_DeadHasContext(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{state: container.StateDead}))
	err := m.Start(context.Background(), "demo")
	if err == nil {
		t.Fatal("want error starting dead container")
	}
	msg := err.Error()
	if !strings.Contains(msg, "容器=demo") || !strings.Contains(msg, "状态=dead") {
		t.Fatalf("want container/status context, got %q", msg)
	}
	if !strings.Contains(msg, "无法启动") && !strings.Contains(msg, "dead") {
		t.Fatalf("want dead-state reason, got %q", msg)
	}
}

func TestContainerManager_StopNotFoundIncludesContext(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{
		err: errdefs.NotFound(errors.New("no such container")),
	}))
	err := m.Stop(context.Background(), "missing", dockerx.StopOptions{})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "停止容器失败") || !strings.Contains(msg, "容器=missing") {
		t.Fatalf("want action and container context, got %q", msg)
	}
}

func TestContainerManager_CreateConflictIncludesContext(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{
		err: errdefs.Conflict(errors.New("Conflict. The container name \"/demo\" is already in use")),
	}))
	_, err := m.Create(context.Background(), dockerx.CreateOptions{Image: "alpine:3.20", Name: "demo"})
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err = %v, want conflict", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "容器=demo") || !strings.Contains(msg, "镜像=alpine:3.20") {
		t.Fatalf("want name and image context, got %q", msg)
	}
}

func TestRestartContainer_CreatedRestarts(t *testing.T) {
	fake := &fakeEngine{state: container.StateCreated}
	c := dockerx.NewWithAPI(fake)
	if err := c.RestartContainer(context.Background(), "demo", dockerx.StopOptions{}); err != nil {
		t.Fatalf("restart created: %v", err)
	}
	if !strings.Contains(strings.Join(fake.ops, ","), "restart:demo") {
		t.Fatalf("ops = %v, want restart", fake.ops)
	}
}

func TestRemoveContainer_RemovingIsOK(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{state: container.StateRemoving})
	if err := c.RemoveContainer(context.Background(), "demo", dockerx.RemoveOptions{}); err != nil {
		t.Fatalf("remove removing: %v", err)
	}
}

func TestStopContainer_PausedStops(t *testing.T) {
	fake := &fakeEngine{state: container.StatePaused}
	c := dockerx.NewWithAPI(fake)
	if err := c.StopContainer(context.Background(), "demo", dockerx.StopOptions{}); err != nil {
		t.Fatalf("stop paused: %v", err)
	}
	if !strings.Contains(strings.Join(fake.ops, ","), "stop:demo") {
		t.Fatalf("ops = %v, want stop", fake.ops)
	}
}
