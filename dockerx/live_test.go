package dockerx_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestClient_LiveDaemonSkipIfUnavailable(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") == "0" {
		t.Skip("live daemon test disabled")
	}
	c, err := dockerx.New(dockerx.Config{Timeout: 5 * time.Second, DisableProxy: true})
	if err != nil {
		if sandboxerr.IsInvalid(err) || sandboxerr.IsUnavailable(err) {
			t.Skipf("cannot construct client: %v", err)
		}
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Ping(ctx); sandboxerr.IsUnavailable(err) || sandboxerr.IsTimeout(err) {
		t.Skipf("docker daemon not reachable: %v", err)
	} else if err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestOrchestrator_LiveCreateDestroy(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run live create/destroy smoke")
	}
	c, err := dockerx.New(dockerx.Config{Timeout: 2 * time.Minute, DisableProxy: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	orch, err := dockerx.NewOrchestrator(c, dockerx.UserSandboxConfig{
		Prefix:       "smoketest",
		MaxInstances: 2,
		Observer:     sandbox.LogObserver{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := fmt.Sprintf("smoke-%d", time.Now().UnixNano())
	created, err := orch.Create(ctx, sandbox.Spec{
		InstanceID: id,
		UserID:     "smoke",
		Image:      "alpine:3.20",
		Cmd:        []string{"sleep", "30"},
		SkipPull:   false,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = orch.Destroy(context.Background(), id, sandbox.DestroyOptions{Purge: true})
	})
	if created.ID != id {
		t.Fatalf("created=%+v", created)
	}
	if _, err := orch.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := orch.Destroy(ctx, id, sandbox.DestroyOptions{Purge: true}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}
