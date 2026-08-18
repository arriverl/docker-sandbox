package dockerx_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/errdefs"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestCreateContainer_RequiresImage(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{})
	_, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{Name: "box"})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
}

func TestCreateContainer_MapsSandboxSpec(t *testing.T) {
	fake := &fakeEngine{createID: "abc123", createWarnings: []string{"deprecated"}}
	c := dockerx.NewWithAPI(fake)

	got, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{
		Name:        "sandbox-1",
		Image:       "alpine:3.20",
		Cmd:         []string{"sleep", "30"},
		Env:         []string{"FOO=bar"},
		WorkingDir:  "/work",
		Labels:      map[string]string{"app": "sandbox"},
		AutoRemove:  true,
		NetworkMode: "bridge",
		Binds:       []string{"/tmp/data:/data:ro"},
		Ports:       []string{"8080:80/tcp"},
		Memory:      64 << 20,
		NanoCPUs:    500_000_000,
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if got.ID != "abc123" || len(got.Warnings) != 1 {
		t.Fatalf("result = %+v", got)
	}
	if fake.lastName != "sandbox-1" {
		t.Fatalf("name = %q", fake.lastName)
	}
	if fake.lastConfig.Image != "alpine:3.20" || strings.Join(fake.lastConfig.Cmd, " ") != "sleep 30" {
		t.Fatalf("config = %+v", fake.lastConfig)
	}
	if fake.lastHost == nil || !fake.lastHost.AutoRemove || fake.lastHost.Memory != 64<<20 {
		t.Fatalf("host = %+v", fake.lastHost)
	}
	if _, ok := fake.lastConfig.ExposedPorts["80/tcp"]; !ok {
		t.Fatalf("exposed ports = %#v", fake.lastConfig.ExposedPorts)
	}
	if bindings := fake.lastHost.PortBindings["80/tcp"]; len(bindings) == 0 || bindings[0].HostPort != "8080" {
		t.Fatalf("port bindings = %#v", fake.lastHost.PortBindings)
	}
}

func TestCreateContainer_InvalidPort(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{})
	_, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{
		Image: "alpine",
		Ports: []string{"not-a-port"},
	})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err = %v, want invalid", err)
	}
}

func TestCreateContainer_Conflict(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{err: errdefs.Conflict(errors.New("name in use"))})
	_, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{Image: "alpine", Name: "dup"})
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err = %v, want conflict", err)
	}
}

func TestStartStopRestartRemove_EmptyID(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{})
	ctx := context.Background()
	if err := c.StartContainer(ctx, "  "); !sandboxerr.IsInvalid(err) {
		t.Fatalf("Start err = %v, want invalid", err)
	}
	if err := c.StopContainer(ctx, "", dockerx.StopOptions{}); !sandboxerr.IsInvalid(err) {
		t.Fatalf("Stop err = %v, want invalid", err)
	}
	if err := c.RestartContainer(ctx, "", dockerx.StopOptions{}); !sandboxerr.IsInvalid(err) {
		t.Fatalf("Restart err = %v, want invalid", err)
	}
	if err := c.RemoveContainer(ctx, "", dockerx.RemoveOptions{}); !sandboxerr.IsInvalid(err) {
		t.Fatalf("Remove err = %v, want invalid", err)
	}
}

func TestStartContainer_NotFound(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{err: errdefs.NotFound(errors.New("no such container"))})
	err := c.StartContainer(context.Background(), "missing")
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
}

func TestClient_ContainerLifecycle(t *testing.T) {
	fake := &fakeEngine{createID: "cid-9"}
	c := dockerx.NewWithAPI(fake)
	ctx := context.Background()

	created, err := c.CreateContainer(ctx, dockerx.CreateOptions{Image: "alpine", Cmd: []string{"sleep", "1"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.StartContainer(ctx, created.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	timeout := 5
	if err := c.StopContainer(ctx, created.ID, dockerx.StopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if fake.lastStop.Timeout == nil || *fake.lastStop.Timeout != 5 {
		t.Fatalf("stop timeout = %+v", fake.lastStop)
	}
	if err := c.RestartContainer(ctx, created.ID, dockerx.StopOptions{Timeout: &timeout}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err := c.RemoveContainer(ctx, created.ID, dockerx.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !fake.lastRemove.Force || !fake.lastRemove.RemoveVolumes {
		t.Fatalf("remove opts = %+v", fake.lastRemove)
	}

	want := []string{"inspect-image:alpine", "create", "inspect:cid-9", "start:cid-9", "inspect:cid-9", "stop:cid-9", "inspect:cid-9", "restart:cid-9", "inspect:cid-9", "remove:cid-9"}
	if strings.Join(fake.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", fake.ops, want)
	}
}

func TestCreateContainer_PullsMissingImage(t *testing.T) {
	fake := &fakeEngine{imageMissing: true, createID: "cid-pulled"}
	c := dockerx.NewWithAPI(fake)

	got, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{
		Image: "alpine:3.20",
		Name:  "demo",
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if got.ID != "cid-pulled" {
		t.Fatalf("id = %q", got.ID)
	}
	want := []string{"inspect-image:alpine:3.20", "inspect-image:alpine:3.20", "pull:alpine:3.20", "inspect-image:alpine:3.20", "create"}
	if strings.Join(fake.ops, ",") != strings.Join(want, ",") {
		t.Fatalf("ops = %v, want %v", fake.ops, want)
	}
}

func TestCreateContainer_SkipPullDoesNotPull(t *testing.T) {
	fake := &fakeEngine{imageMissing: true}
	c := dockerx.NewWithAPI(fake)

	_, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{
		Image:    "alpine:3.20",
		SkipPull: true,
	})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	if strings.Contains(strings.Join(fake.ops, ","), "pull:") {
		t.Fatalf("ops = %v, did not expect pull", fake.ops)
	}
}

func TestCreateContainer_PullFailure(t *testing.T) {
	fake := &fakeEngine{
		imageMissing: true,
		pullErr:      errdefs.NotFound(errors.New("manifest unknown")),
	}
	c := dockerx.NewWithAPI(fake)

	_, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{Image: "alpine:3.20"})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
}

func TestCreateContainer_PullStreamError(t *testing.T) {
	fake := &fakeEngine{
		imageMissing: true,
		pullStream:   `{"error":"manifest unknown: not found","errorDetail":{"message":"manifest unknown: not found"}}` + "\n",
	}
	c := dockerx.NewWithAPI(fake)

	_, err := c.CreateContainer(context.Background(), dockerx.CreateOptions{Image: "missing:tag"})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	if fake.countOps("create") != 0 {
		t.Fatalf("ops = %v, did not expect create after pull stream error", fake.ops)
	}
	if fake.countOps("pull:") != 1 {
		t.Fatalf("ops = %v, want one pull", fake.ops)
	}
}

func TestCreateContainer_PullSingleflight(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 2)
	fake := &fakeEngine{
		imageMissing: true,
		createID:     "cid-once",
		pullHold:     hold,
		pullEntered:  entered,
	}
	c := dockerx.NewWithAPI(fake)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = c.CreateContainer(context.Background(), dockerx.CreateOptions{
				Image: "alpine:3.20",
				Name:  "box",
			})
		}()
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for image pull")
	}
	time.Sleep(50 * time.Millisecond)
	close(hold)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if got := fake.countOps("pull:"); got != 1 {
		t.Fatalf("pull count = %d, want 1 (singleflight); ops=%v", got, fake.ops)
	}
}
