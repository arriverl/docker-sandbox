package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func testService(t *testing.T, world *memWorld) *Service {
	t.Helper()
	svc, err := New(Config{Prefix: "sandbox", WaitInterval: 10 * time.Millisecond}, world.plugins())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func sampleSpec() Spec {
	return Spec{
		InstanceID: "box1",
		UserID:     "alice",
		Image:      "alpine:3.20",
		Cmd:        []string{"sleep", "30"},
		Memory:     64 << 20,
		NanoCPUs:   500_000_000,
		Expose:     []string{"80"},
	}
}

func TestCreate_RequiresIdentity(t *testing.T) {
	svc := testService(t, newMemWorld())
	_, err := svc.Create(context.Background(), Spec{Image: "alpine:3.20"})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err=%v want invalid", err)
	}
	if !strings.Contains(err.Error(), "实例") {
		t.Fatalf("want 实例 in error, got %q", err)
	}
}

func TestCreate_ConflictWhenContainerExists(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(context.Background(), sampleSpec())
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err=%v want conflict", err)
	}
}

func TestCreate_DoesNotStart(t *testing.T) {
	svc := testService(t, newMemWorld())
	got, err := svc.Create(context.Background(), sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "box1" || got.Name != "sandbox-box1" {
		t.Fatalf("identity=%+v", got)
	}
	if got.Status != StatusCreated {
		t.Fatalf("status=%s want created", got.Status)
	}
	if got.UserID != "alice" || got.Network == "" || got.Volume == "" {
		t.Fatalf("missing net/vol: %+v", got)
	}
	if got.ContainerID == "" {
		t.Fatal("expected diagnostic container id")
	}
}

func TestStartStop_Idempotent(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	a, err := svc.Start(context.Background(), "box1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusRunning {
		t.Fatalf("status=%s", a.Status)
	}
	b, err := svc.Start(context.Background(), "box1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusRunning {
		t.Fatalf("second start status=%s", b.Status)
	}
	c, err := svc.Stop(context.Background(), "box1", StopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusExited {
		t.Fatalf("stop status=%s", c.Status)
	}
	d, err := svc.Stop(context.Background(), "box1", StopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusExited {
		t.Fatalf("second stop status=%s", d.Status)
	}
}

func TestDestroy_KeepsVolumeAndRestoreRebuildsSameID(t *testing.T) {
	world := newMemWorld()
	svc := testService(t, world)
	created, err := svc.Create(context.Background(), sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	oldCID := created.ContainerID
	if err := svc.Destroy(context.Background(), "box1", DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), "box1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDestroyed {
		t.Fatalf("status=%s want destroyed", got.Status)
	}
	if got.Volume == "" {
		t.Fatal("volume should remain")
	}
	restored, err := svc.Restore(context.Background(), "box1", RestoreOptions{KeepVolume: true})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != "box1" || restored.Name != "sandbox-box1" {
		t.Fatalf("identity changed: %+v", restored)
	}
	if restored.ContainerID == "" || restored.ContainerID == oldCID {
		t.Fatalf("container id should change, old=%s new=%s", oldCID, restored.ContainerID)
	}
	if restored.Status != StatusCreated {
		t.Fatalf("restore should not auto-start, status=%s", restored.Status)
	}
	if restored.Volume != created.Volume {
		t.Fatalf("volume=%s want %s", restored.Volume, created.Volume)
	}
}

func TestRestore_KeepsInstanceIDWhileContainerExists(t *testing.T) {
	svc := testService(t, newMemWorld())
	created, err := svc.Create(context.Background(), sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := svc.Restore(context.Background(), "box1", RestoreOptions{KeepVolume: true})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != created.ID || restored.Name != created.Name {
		t.Fatalf("id/name changed: %+v vs %+v", restored, created)
	}
	if restored.ContainerID == created.ContainerID {
		t.Fatal("docker id should change on restore")
	}
}

func TestWaitReady_TimeoutAndSuccess(t *testing.T) {
	world := newMemWorld()
	world.health = func(Instance, Probe, int) error {
		return errors.New("not ready")
	}
	svc := testService(t, world)
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := svc.WaitReady(ctx, "box1", Probe{Kind: ProbeHTTP, Path: "/ready"})
	if !sandboxerr.IsTimeout(err) {
		t.Fatalf("err=%v want timeout", err)
	}

	world.healthN = 0
	world.health = func(_ Instance, _ Probe, n int) error {
		if n < 2 {
			return errors.New("not ready")
		}
		return nil
	}
	got, err := svc.WaitReady(context.Background(), "box1", Probe{Kind: ProbeTCP})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("status=%s", got.Status)
	}
}

func TestWaitReady_RequiresRunning(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	_, err := svc.WaitReady(context.Background(), "box1", Probe{Kind: ProbeTCP})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err=%v want invalid", err)
	}
}

func TestCreate_AfterDestroyRequiresRestore(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Destroy(context.Background(), "box1", DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(context.Background(), sampleSpec())
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err=%v want conflict", err)
	}
	if !strings.Contains(err.Error(), "还原") && !strings.Contains(err.Error(), "restore") {
		t.Fatalf("want restore hint, got %q", err)
	}
	next := sampleSpec()
	next.Image = "ubuntu:22.04"
	_, err = svc.Create(context.Background(), next)
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("changed image still conflict: %v", err)
	}
	restored, err := svc.Restore(context.Background(), "box1", RestoreOptions{KeepVolume: true})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Image != "alpine:3.20" {
		t.Fatalf("restore image=%s", restored.Image)
	}
}

func TestCreate_ImageNotFoundKeepsImageWording(t *testing.T) {
	world := newMemWorld()
	rt := memRuntime{world}
	orig := world.plugins()
	orig.Runtime = failingImageRuntime{memRuntime: rt}
	svc, err := New(Config{Prefix: "sandbox", WaitInterval: 10 * time.Millisecond}, orig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(context.Background(), sampleSpec())
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err=%v want not_found", err)
	}
	if !strings.Contains(err.Error(), "镜像") {
		t.Fatalf("want 镜像, got %q", err)
	}
	if strings.Contains(err.Error(), "指定的实例不存在") {
		t.Fatalf("must not rewrite as instance missing: %q", err)
	}
}

type failingImageRuntime struct {
	memRuntime
}

func (f failingImageRuntime) Create(ctx context.Context, req RuntimeCreate) (RuntimeContainer, error) {
	return RuntimeContainer{}, sandboxerr.WrapKind("创建容器", sandboxerr.KindNotFound,
		fmt.Errorf("%w: No such image: %s", sandboxerr.ErrNotFound, req.Image))
}

func TestContainerName_Stable(t *testing.T) {
	if got := ContainerName("sandbox", "hello-test"); got != "sandbox-hello-test" {
		t.Fatalf("got %s", got)
	}
}
