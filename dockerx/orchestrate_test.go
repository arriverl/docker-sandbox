package dockerx_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestOrchestrator_CreateRestoreByInstanceID(t *testing.T) {
	fake := &fakeEngine{}
	client := dockerx.NewWithAPI(fake)
	orch, err := dockerx.NewOrchestrator(client, dockerx.UserSandboxConfig{Prefix: "sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := sandbox.Spec{
		InstanceID: "box1",
		UserID:     "alice",
		Image:      "alpine:3.20",
		Memory:     64 << 20,
		NanoCPUs:   500_000_000,
		Expose:     []string{"8080"},
	}
	created, err := orch.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "box1" || created.Name != "sandbox-box1" {
		t.Fatalf("created=%+v", created)
	}
	if created.Status == sandbox.StatusRunning {
		t.Fatal("create must not start")
	}
	if fake.lastHost == nil || fake.lastHost.Memory != 64<<20 || fake.lastHost.NanoCPUs != 500_000_000 {
		t.Fatalf("resources=%+v", fake.lastHost)
	}
	if fake.lastHost.NetworkMode == "" {
		t.Fatal("expected user network")
	}
	listed, err := orch.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "box1" {
		t.Fatalf("listed=%+v", listed)
	}

	if err := orch.Destroy(ctx, "box1", sandbox.DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := orch.Get(ctx, "box1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != sandbox.StatusDestroyed {
		t.Fatalf("status=%s want destroyed", got.Status)
	}

	restored, err := orch.Restore(ctx, "box1", sandbox.RestoreOptions{KeepVolume: true})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != "box1" || restored.Name != "sandbox-box1" {
		t.Fatalf("restored identity %+v", restored)
	}
}

func TestOrchestrator_CreateMissingImageWording(t *testing.T) {
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(&fakeEngine{imageMissing: true}), dockerx.UserSandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = orch.Create(context.Background(), sandbox.Spec{
		InstanceID: "box1",
		UserID:     "alice",
		Image:      "missing:tag",
		SkipPull:   true,
	})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err=%v want not_found", err)
	}
	if !strings.Contains(err.Error(), "镜像") {
		t.Fatalf("want 镜像, got %q", err)
	}
	if strings.Contains(err.Error(), "指定的实例不存在") {
		t.Fatalf("must not say 实例不存在: %q", err)
	}
}

func TestOrchestrator_CreateConflict(t *testing.T) {
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(&fakeEngine{}), dockerx.UserSandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{InstanceID: "box1", UserID: "alice", Image: "alpine:3.20"}
	if _, err := orch.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	_, err = orch.Create(context.Background(), spec)
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err=%v want conflict", err)
	}
	if !strings.Contains(err.Error(), "实例") {
		t.Fatalf("want 实例 in %q", err)
	}
}

func TestOrchestratorFallsBackToInspectWhenListLags(t *testing.T) {
	fake := &fakeEngine{hideCreatedFromList: true}
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := orch.Create(context.Background(), sandbox.Spec{
		InstanceID: "box1",
		UserID:     "alice",
		Image:      "alpine:3.20",
		Expose:     []string{"8080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "box1" || got.ContainerID == "" || len(got.HostPorts) != 1 {
		t.Fatalf("inspect fallback=%+v", got)
	}
}

func TestNewOrchestratorAppliesConfiguredDefaultLimits(t *testing.T) {
	fake := &fakeEngine{}
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{
		DefaultMemory:   256 << 20,
		DefaultNanoCPUs: 500_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = orch.Create(context.Background(), sandbox.Spec{
		InstanceID: "box1",
		UserID:     "alice",
		Image:      "alpine:3.20",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastHost.Memory != 256<<20 || fake.lastHost.NanoCPUs != 500_000_000 {
		t.Fatalf("limits memory=%d nanoCPUs=%d", fake.lastHost.Memory, fake.lastHost.NanoCPUs)
	}
}

func TestNewOrchestratorAppliesRecommendedDefaults(t *testing.T) {
	fake := &fakeEngine{}
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = orch.Create(context.Background(), sandbox.Spec{
		InstanceID: "box1",
		UserID:     "alice",
		Image:      "alpine:3.20",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastHost.Memory != sandbox.RecommendedDefaultMemory || fake.lastHost.NanoCPUs != sandbox.RecommendedDefaultNanoCPUs {
		t.Fatalf("recommended limits memory=%d nanoCPUs=%d", fake.lastHost.Memory, fake.lastHost.NanoCPUs)
	}
}

func TestOrchestrator_WaitReadyExec(t *testing.T) {
	fake := &fakeEngine{}
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := orch.Create(ctx, sandbox.Spec{InstanceID: "box1", UserID: "alice", Image: "alpine:3.20"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Start(ctx, "box1"); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.WaitReady(ctx, "box1", sandbox.Probe{Kind: sandbox.ProbeExec, Command: []string{"true"}}); err != nil {
		t.Fatalf("exec probe: %v", err)
	}
	if got := strings.Join(fake.lastExecCmd, " "); got != "true" {
		t.Fatalf("exec cmd = %q", got)
	}
}

func TestOrchestrator_WaitReadyExecNonZero(t *testing.T) {
	fake := &fakeEngine{execExit: 7}
	orch, err := dockerx.NewOrchestrator(dockerx.NewWithAPI(fake), dockerx.UserSandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	base := context.Background()
	if _, err := orch.Create(base, sandbox.Spec{InstanceID: "box1", UserID: "alice", Image: "alpine:3.20"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Start(base, "box1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(base, 300*time.Millisecond)
	defer cancel()
	_, err = orch.WaitReady(ctx, "box1", sandbox.Probe{Kind: sandbox.ProbeExec, Command: []string{"false"}})
	if err == nil {
		t.Fatal("want exec failure")
	}
}
