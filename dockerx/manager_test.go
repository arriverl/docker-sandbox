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

func TestContainerManager_CreateRequiresImageInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{}))
	_, err := m.Create(context.Background(), dockerx.CreateOptions{Name: "box"})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
	if !strings.Contains(err.Error(), "创建容器失败") || !strings.Contains(err.Error(), "镜像") {
		t.Fatalf("want 中文创建错误, got %q", err)
	}
}

func TestContainerManager_CreateMissingImageInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{imageMissing: true}))
	_, err := m.Create(context.Background(), dockerx.CreateOptions{
		Image:    "alpine:3.20",
		SkipPull: true,
	})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	if !strings.Contains(err.Error(), "创建容器失败") || !strings.Contains(err.Error(), "镜像") {
		t.Fatalf("want 中文镜像不存在错误, got %q", err)
	}
	if strings.Contains(err.Error(), "容器不存在") {
		t.Fatalf("create missing image should not say 容器不存在, got %q", err)
	}
}

func TestContainerManager_StartEmptyIDInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{}))
	err := m.Start(context.Background(), " ")
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
	if !strings.Contains(err.Error(), "启动容器失败") || !strings.Contains(err.Error(), "ID") {
		t.Fatalf("want 中文启动错误, got %q", err)
	}
}

func TestContainerManager_StopNotFoundInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{
		err: errdefs.NotFound(errors.New("no such container")),
	}))
	err := m.Stop(context.Background(), "missing", dockerx.StopOptions{})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	if !strings.Contains(err.Error(), "停止容器失败") || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("want 中文停止错误, got %q", err)
	}
}

func TestContainerManager_RemoveUnavailableInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{
		err: errors.New("Cannot connect to the Docker daemon. Is the docker daemon running?"),
	}))
	err := m.Remove(context.Background(), "cid", dockerx.RemoveOptions{Force: true})
	if !sandboxerr.IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "删除容器失败") || !strings.Contains(err.Error(), "守护进程") {
		t.Fatalf("want 中文删除错误, got %q", err)
	}
}

func TestContainerManager_InspectAndList(t *testing.T) {
	fake := &fakeEngine{
		createID: "cid-inspect",
		inspect: container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{
				ID:    "cid-inspect",
				Name:  "/sandbox-1",
				Image: "sha256:aaa",
				State: &container.State{
					Status:     container.StateRunning,
					Running:    true,
					Pid:        42,
					StartedAt:  "2026-01-01T00:00:00Z",
					FinishedAt: "0001-01-01T00:00:00Z",
				},
				HostConfig: &container.HostConfig{NetworkMode: "bridge"},
			},
			Config: &container.Config{
				Image:  "alpine:3.20",
				Cmd:    []string{"sleep", "30"},
				Labels: map[string]string{"app": "sandbox"},
			},
		},
		containers: []container.Summary{{
			ID:    "cid-inspect",
			Names: []string{"/sandbox-1"},
			Image: "alpine:3.20",
			State: container.StateRunning,
		}},
	}
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(fake))
	ctx := context.Background()

	got, err := m.Inspect(ctx, "cid-inspect")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got.ID != "cid-inspect" || got.Name != "/sandbox-1" || !got.Running || got.Pid != 42 {
		t.Fatalf("inspect = %+v", got)
	}
	if got.Image != "alpine:3.20" || got.Status != "running" {
		t.Fatalf("inspect image/status = %+v", got)
	}

	list, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != "cid-inspect" {
		t.Fatalf("list = %+v", list)
	}
}

func TestContainerManager_InspectEmptyIDInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{}))
	_, err := m.Inspect(context.Background(), "")
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
	if !strings.Contains(err.Error(), "查询容器详情失败") {
		t.Fatalf("want 中文查询错误, got %q", err)
	}
}

func TestContainerManager_ListUnavailableInChinese(t *testing.T) {
	m := dockerx.NewContainerManager(dockerx.NewWithAPI(&fakeEngine{
		err: errors.New("Cannot connect to the Docker daemon. Is the docker daemon running?"),
	}))
	_, err := m.List(context.Background())
	if !sandboxerr.IsUnavailable(err) {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "列出容器失败") {
		t.Fatalf("want 中文列表错误, got %q", err)
	}
}
