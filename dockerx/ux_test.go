package dockerx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
)

func TestDefaultConfigDisablesProxy(t *testing.T) {
	cfg := dockerx.DefaultConfig()
	if !cfg.DisableProxy {
		t.Fatal("DefaultConfig must disable HTTP proxy for npipe/unix")
	}
	sb := dockerx.DefaultSandboxConfig()
	if sb.DefaultMemory != sandbox.RecommendedDefaultMemory || sb.DefaultNanoCPUs != sandbox.RecommendedDefaultNanoCPUs {
		t.Fatalf("sandbox defaults memory=%d cpus=%d", sb.DefaultMemory, sb.DefaultNanoCPUs)
	}
	if sb.MaxInstances != sandbox.DesignPeakInstances {
		t.Fatalf("max instances=%d", sb.MaxInstances)
	}
	if sb.Observer == nil {
		t.Fatal("default observer must be set")
	}
}

func TestUserSandbox_HonorsPresetNetworkMode(t *testing.T) {
	fake := &fakeEngine{createID: "c1"}
	sb := newUserSandbox(t, fake)
	got, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:       "alpine",
		UserID:      "alice",
		Name:        "box",
		NetworkMode: "sandbox-net-custom",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != "sandbox-net-custom" {
		t.Fatalf("network=%q want preset", got.Network)
	}
	if fake.lastHost == nil || string(fake.lastHost.NetworkMode) != "sandbox-net-custom" {
		t.Fatalf("engine network=%v", fake.lastHost)
	}
}

func TestUserSandbox_EmitsPullProgress(t *testing.T) {
	fake := &fakeEngine{
		imageMissing: true,
		createID:     "c1",
		pullStream: `{"status":"Downloading","id":"layer1","progressDetail":{"current":10,"total":100}}` + "\n" +
			`{"status":"Pull complete"}` + "\n",
	}
	observer := &recordingSandboxObserver{}
	sb := dockerx.NewUserSandbox(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{
		Prefix:   "sandbox",
		Observer: observer,
		Pool:     dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 20000, Checker: &fakeOccupancy{}}),
	})
	if _, err := sb.Create(context.Background(), dockerx.CreateOptions{
		Image:  "alpine:3.20",
		UserID: "alice",
		Name:   "box",
	}); err != nil {
		t.Fatal(err)
	}
	if !observer.hasStage("pull.progress") {
		t.Fatalf("stages=%v want pull.progress", observer.stages)
	}
}

func TestInstanceNetworkOwnerIsDistinctFromUser(t *testing.T) {
	owner := dockerx.InstanceNetworkOwner("box1")
	if owner == "box1" || owner == "alice" || !strings.Contains(owner, "box1") {
		t.Fatalf("owner=%q", owner)
	}
	if dockerx.UserNetworkName("sandbox", owner) == dockerx.UserNetworkName("sandbox", "alice") {
		t.Fatal("instance network name collided with user network")
	}
}
