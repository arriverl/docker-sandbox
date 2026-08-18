package dockerx_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/dockerx"
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
