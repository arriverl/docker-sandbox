package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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

func TestNewRejectsNegativeDefaultLimits(t *testing.T) {
	world := newMemWorld()
	if _, err := New(Config{DefaultMemory: -1}, world.plugins()); !sandboxerr.IsInvalid(err) {
		t.Fatalf("negative memory err=%v want invalid", err)
	}
	if _, err := New(Config{DefaultNanoCPUs: -1}, world.plugins()); !sandboxerr.IsInvalid(err) {
		t.Fatalf("negative nanoCPUs err=%v want invalid", err)
	}
}

func TestCreate_RejectsOversizedPersistedSpec(t *testing.T) {
	world := newMemWorld()
	svc := testService(t, world)
	spec := sampleSpec()
	spec.Env = []string{"PAYLOAD=" + strings.Repeat("x", MaxEncodedSpecBytes)}

	_, err := svc.Create(context.Background(), spec)
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("Create err=%v want invalid", err)
	}
	if len(world.boxes) != 0 || len(world.vols) != 0 {
		t.Fatalf("oversized spec created resources: boxes=%d vols=%d", len(world.boxes), len(world.vols))
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

func TestCreate_AppliesConfiguredDefaultsAndEmitsProvisionStages(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	runtime := &captureCreateRuntime{Runtime: plugins.Runtime}
	plugins.Runtime = runtime
	observer := &recordingObserver{}
	svc, err := New(Config{
		Prefix:          "sandbox",
		DefaultMemory:   128 << 20,
		DefaultNanoCPUs: 750_000_000,
		Observer:        observer,
	}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	spec := sampleSpec()
	spec.Memory = 0
	spec.NanoCPUs = 0

	if _, err := svc.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if runtime.last.Memory != 128<<20 || runtime.last.NanoCPUs != 750_000_000 {
		t.Fatalf("runtime limits memory=%d nanoCPUs=%d", runtime.last.Memory, runtime.last.NanoCPUs)
	}
	got := observer.stages()
	want := []string{"operation.start", "provision.network", "provision.storage", "provision.runtime", "operation.end"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("observer stages=%v want %v", got, want)
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

func TestRestartAndList(t *testing.T) {
	world := newMemWorld()
	svc := testService(t, world)
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Restart(context.Background(), "box1", StopOptions{}); err != nil || got.Status != StatusRunning {
		t.Fatalf("restart created got=%+v err=%v", got, err)
	}
	if got, err := svc.Restart(context.Background(), "box1", StopOptions{}); err != nil || got.Status != StatusRunning {
		t.Fatalf("restart running got=%+v err=%v", got, err)
	}
	listed, err := svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "box1" {
		t.Fatalf("listed=%+v", listed)
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

func TestRestore_ResetRemovesContainerBeforeVolume(t *testing.T) {
	world := newMemWorld()
	var events []string
	plugins := world.plugins()
	plugins.Runtime = &eventRuntime{Runtime: plugins.Runtime, events: &events}
	plugins.Storage = &eventStorage{Storage: plugins.Storage, events: &events}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	events = nil

	if _, err := svc.Restore(context.Background(), "box1", RestoreOptions{ResetVolume: true}); err != nil {
		t.Fatal(err)
	}
	removeAt, volumeAt := indexOf(events, "runtime.remove"), indexOf(events, "storage.destroy")
	if removeAt < 0 || volumeAt < 0 || removeAt > volumeAt {
		t.Fatalf("events=%v want runtime.remove before storage.destroy", events)
	}
}

func TestRestore_DefaultKeepsWorkspace(t *testing.T) {
	world := newMemWorld()
	var events []string
	plugins := world.plugins()
	plugins.Storage = &eventStorage{Storage: plugins.Storage, events: &events}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Destroy(context.Background(), "box1", DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	events = nil

	if _, err := svc.Restore(context.Background(), "box1", RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := indexOf(events, "storage.destroy"); got >= 0 {
		t.Fatalf("default restore deleted workspace: events=%v", events)
	}
}

func TestDestroy_AttemptsCleanupAndReturnsStopFailure(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	runtime := &stopFailRuntime{Runtime: plugins.Runtime}
	plugins.Runtime = runtime
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}

	err = svc.Destroy(context.Background(), "box1", DestroyOptions{Purge: true})
	if err == nil || !strings.Contains(err.Error(), "injected stop failure") {
		t.Fatalf("destroy err=%v want stop failure", err)
	}
	if !runtime.removed {
		t.Fatal("destroy must attempt remove after stop failure")
	}
}

func TestDestroy_ReleasesNetworkAfterStorageFailure(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	network := &releaseTrackingNetwork{Network: plugins.Network}
	plugins.Network = network
	plugins.Storage = &destroyFailStorage{Storage: plugins.Storage}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}

	err = svc.Destroy(context.Background(), "box1", DestroyOptions{Purge: true})
	if err == nil || !strings.Contains(err.Error(), "injected storage failure") {
		t.Fatalf("destroy err=%v want storage failure", err)
	}
	if !network.released {
		t.Fatal("destroy must attempt network release after storage failure")
	}
}

func TestDestroy_UsesLiveCleanupContextAfterStopCancelsCaller(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &cancelingStopRuntime{Runtime: plugins.Runtime, cancel: cancel}
	plugins.Runtime = runtime
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}

	err = svc.Destroy(ctx, "box1", DestroyOptions{Purge: true})
	if err == nil {
		t.Fatal("destroy should report canceled stop")
	}
	if runtime.removeContextErr != nil {
		t.Fatalf("remove inherited canceled context: %v", runtime.removeContextErr)
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

func TestCreate_RuntimeFailureRemovesNewVolumeAndCanRetry(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	plugins.Runtime = &failOnceRuntime{Runtime: plugins.Runtime}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Create(context.Background(), sampleSpec()); err == nil {
		t.Fatal("first create should fail")
	}
	if _, err := world.plugins().Storage.Inspect(context.Background(), "box1"); !sandboxerr.IsNotFound(err) {
		t.Fatalf("new workspace must be compensated, inspect err=%v", err)
	}
	if len(world.nets) != 0 {
		t.Fatalf("new user network must be compensated: %v", world.nets)
	}
	got, err := svc.Create(context.Background(), sampleSpec())
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if got.ID != "box1" || got.Status != StatusCreated {
		t.Fatalf("retry result=%+v", got)
	}
}

func TestCreate_StorageFailureRemovesNewUserNetwork(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	plugins.Storage = &ensureFailStorage{Storage: plugins.Storage}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Create(context.Background(), sampleSpec()); err == nil {
		t.Fatal("create should fail")
	}
	if len(world.nets) != 0 {
		t.Fatalf("new user network must be compensated: %v", world.nets)
	}
}

func TestCreate_CanceledRuntimeStillCompensatesWithLiveContext(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	storage := &cancelAwareStorage{Storage: plugins.Storage}
	plugins.Storage = storage
	plugins.Runtime = canceledRuntime{}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := svc.Create(ctx, sampleSpec()); err == nil {
		t.Fatal("create should fail")
	}
	if storage.destroyContextErr != nil {
		t.Fatalf("compensation inherited canceled context: %v", storage.destroyContextErr)
	}
	if _, err := world.plugins().Storage.Inspect(context.Background(), "box1"); !sandboxerr.IsNotFound(err) {
		t.Fatalf("new workspace must be removed, inspect err=%v", err)
	}
}

func TestCreate_RetriesPendingCompensationBeforeProvisioning(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	plugins.Storage = &failDestroyOnceStorage{Storage: plugins.Storage}
	plugins.Runtime = &failOnceRuntime{Runtime: plugins.Runtime}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Create(context.Background(), sampleSpec()); err == nil {
		t.Fatal("first create should fail")
	}
	got, err := svc.Create(context.Background(), sampleSpec())
	if err != nil {
		t.Fatalf("retry create: %v", err)
	}
	if got.Status != StatusCreated {
		t.Fatalf("retry status=%s", got.Status)
	}
	if _, pending := svc.failed.Load("box1"); pending {
		t.Fatal("pending compensation marker must be cleared")
	}
}

func TestRestore_ClearsPendingCreateCompensation(t *testing.T) {
	world := newMemWorld()
	plugins := world.plugins()
	plugins.Storage = &failDestroyOnceStorage{Storage: plugins.Storage}
	plugins.Runtime = &failOnceRuntime{Runtime: plugins.Runtime}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Create(context.Background(), sampleSpec()); err == nil {
		t.Fatal("create should fail")
	}
	if _, pending := svc.failed.Load("box1"); !pending {
		t.Fatal("expected pending compensation")
	}
	if _, err := svc.Restore(context.Background(), "box1", RestoreOptions{KeepVolume: true}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, pending := svc.failed.Load("box1"); pending {
		t.Fatal("successful restore must clear pending compensation")
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); !sandboxerr.IsConflict(err) {
		t.Fatalf("create after restore err=%v want conflict", err)
	}
}

func TestProvision_RuntimeFailureKeepsPreexistingVolume(t *testing.T) {
	world := newMemWorld()
	if _, err := world.plugins().Storage.Ensure(context.Background(), "box1", map[string]string{
		LabelSpec: EncodeSpec(sampleSpec()),
	}); err != nil {
		t.Fatal(err)
	}
	plugins := world.plugins()
	plugins.Runtime = failingImageRuntime{memRuntime: memRuntime{world}}
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.provision(context.Background(), sampleSpec()); err == nil {
		t.Fatal("provision should fail")
	}
	if _, err := world.plugins().Storage.Inspect(context.Background(), "box1"); err != nil {
		t.Fatalf("preexisting workspace must remain: %v", err)
	}
}

func TestCreate_SerializesSameInstanceID(t *testing.T) {
	world := newMemWorld()
	gate := newBlockingRuntime(world.plugins().Runtime)
	plugins := world.plugins()
	plugins.Runtime = gate
	svc, err := New(Config{Prefix: "sandbox"}, plugins)
	if err != nil {
		t.Fatal(err)
	}

	results := make(chan error, 2)
	go func() {
		_, err := svc.Create(context.Background(), sampleSpec())
		results <- err
	}()
	<-gate.firstEntered
	go func() {
		_, err := svc.Create(context.Background(), sampleSpec())
		results <- err
	}()

	select {
	case <-gate.secondEntered:
		close(gate.releaseFirst)
		t.Fatal("second runtime create entered before first create completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.releaseFirst)

	firstErr, secondErr := <-results, <-results
	if firstErr != nil && secondErr != nil {
		t.Fatalf("one create should succeed, errors: %v / %v", firstErr, secondErr)
	}
	if firstErr == nil && secondErr == nil {
		t.Fatal("one create should report conflict")
	}
	conflict := firstErr
	if conflict == nil {
		conflict = secondErr
	}
	if !sandboxerr.IsConflict(conflict) {
		t.Fatalf("losing create err=%v want conflict", conflict)
	}
	if got := atomic.LoadInt32(&gate.maxActive); got != 1 {
		t.Fatalf("max concurrent runtime creates=%d want 1", got)
	}
	svc.mutations.mu.Lock()
	defer svc.mutations.mu.Unlock()
	if got := len(svc.mutations.entries); got != 0 {
		t.Fatalf("idle keyed locks=%d want 0", got)
	}
}

type failOnceRuntime struct {
	Runtime
	once sync.Once
}

func (r *failOnceRuntime) Create(ctx context.Context, req RuntimeCreate) (RuntimeContainer, error) {
	var fail bool
	r.once.Do(func() { fail = true })
	if fail {
		return RuntimeContainer{}, errors.New("injected runtime create failure")
	}
	return r.Runtime.Create(ctx, req)
}

type captureCreateRuntime struct {
	Runtime
	last RuntimeCreate
}

func (r *captureCreateRuntime) Create(ctx context.Context, req RuntimeCreate) (RuntimeContainer, error) {
	r.last = req
	return r.Runtime.Create(ctx, req)
}

type recordingObserver struct {
	events []OperationEvent
}

func (o *recordingObserver) Observe(_ context.Context, event OperationEvent) {
	o.events = append(o.events, event)
}

func (o *recordingObserver) stages() []string {
	out := make([]string, 0, len(o.events))
	for _, event := range o.events {
		out = append(out, event.Stage)
	}
	return out
}

type eventRuntime struct {
	Runtime
	events *[]string
}

func (r *eventRuntime) Stop(ctx context.Context, id string, opts StopOptions) error {
	*r.events = append(*r.events, "runtime.stop")
	return r.Runtime.Stop(ctx, id, opts)
}

func (r *eventRuntime) Remove(ctx context.Context, id string) error {
	*r.events = append(*r.events, "runtime.remove")
	return r.Runtime.Remove(ctx, id)
}

type eventStorage struct {
	Storage
	events *[]string
}

func (s *eventStorage) EnsureTracked(ctx context.Context, instanceID string, labels map[string]string) (Volume, bool, error) {
	return s.Storage.(trackedStorage).EnsureTracked(ctx, instanceID, labels)
}

func (s *eventStorage) Destroy(ctx context.Context, instanceID string, purge bool) error {
	*s.events = append(*s.events, "storage.destroy")
	return s.Storage.Destroy(ctx, instanceID, purge)
}

type stopFailRuntime struct {
	Runtime
	removed bool
}

func (r *stopFailRuntime) Stop(context.Context, string, StopOptions) error {
	return errors.New("injected stop failure")
}

func (r *stopFailRuntime) Remove(ctx context.Context, id string) error {
	r.removed = true
	return r.Runtime.Remove(ctx, id)
}

type cancelingStopRuntime struct {
	Runtime
	cancel           context.CancelFunc
	removeContextErr error
}

func (r *cancelingStopRuntime) Stop(context.Context, string, StopOptions) error {
	r.cancel()
	return context.Canceled
}

func (r *cancelingStopRuntime) Remove(ctx context.Context, id string) error {
	r.removeContextErr = ctx.Err()
	return r.Runtime.Remove(ctx, id)
}

type releaseTrackingNetwork struct {
	Network
	released bool
}

func (n *releaseTrackingNetwork) Release(ctx context.Context, userID string) error {
	n.released = true
	return n.Network.Release(ctx, userID)
}

type destroyFailStorage struct{ Storage }

func (s *destroyFailStorage) EnsureTracked(ctx context.Context, instanceID string, labels map[string]string) (Volume, bool, error) {
	return s.Storage.(trackedStorage).EnsureTracked(ctx, instanceID, labels)
}

func (s *destroyFailStorage) Destroy(context.Context, string, bool) error {
	return errors.New("injected storage failure")
}

type ensureFailStorage struct{ Storage }

func (s *ensureFailStorage) EnsureTracked(context.Context, string, map[string]string) (Volume, bool, error) {
	return Volume{}, false, errors.New("injected storage ensure failure")
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

type canceledRuntime struct{}

func (canceledRuntime) Create(ctx context.Context, _ RuntimeCreate) (RuntimeContainer, error) {
	return RuntimeContainer{}, ctx.Err()
}
func (canceledRuntime) Start(context.Context, string) error { return nil }
func (canceledRuntime) Stop(context.Context, string, StopOptions) error {
	return nil
}
func (canceledRuntime) Remove(context.Context, string) error { return nil }
func (canceledRuntime) Inspect(context.Context, string) (RuntimeContainer, error) {
	return RuntimeContainer{}, sandboxerr.ErrNotFound
}
func (canceledRuntime) Exec(context.Context, string, []string) error { return nil }

type cancelAwareStorage struct {
	Storage
	destroyContextErr error
}

func (s *cancelAwareStorage) EnsureTracked(ctx context.Context, instanceID string, labels map[string]string) (Volume, bool, error) {
	return s.Storage.(trackedStorage).EnsureTracked(ctx, instanceID, labels)
}

func (s *cancelAwareStorage) Destroy(ctx context.Context, instanceID string, purge bool) error {
	s.destroyContextErr = ctx.Err()
	return s.Storage.Destroy(ctx, instanceID, purge)
}

type failDestroyOnceStorage struct {
	Storage
	once sync.Once
}

func (s *failDestroyOnceStorage) EnsureTracked(ctx context.Context, instanceID string, labels map[string]string) (Volume, bool, error) {
	return s.Storage.(trackedStorage).EnsureTracked(ctx, instanceID, labels)
}

func (s *failDestroyOnceStorage) Destroy(ctx context.Context, instanceID string, purge bool) error {
	var fail bool
	s.once.Do(func() { fail = true })
	if fail {
		return errors.New("injected volume cleanup failure")
	}
	return s.Storage.Destroy(ctx, instanceID, purge)
}

type blockingRuntime struct {
	Runtime
	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
	calls         int32
	active        int32
	maxActive     int32
}

func newBlockingRuntime(runtime Runtime) *blockingRuntime {
	return &blockingRuntime{
		Runtime:       runtime,
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
}

func (r *blockingRuntime) Create(ctx context.Context, req RuntimeCreate) (RuntimeContainer, error) {
	call := atomic.AddInt32(&r.calls, 1)
	active := atomic.AddInt32(&r.active, 1)
	defer atomic.AddInt32(&r.active, -1)
	for {
		max := atomic.LoadInt32(&r.maxActive)
		if active <= max || atomic.CompareAndSwapInt32(&r.maxActive, max, active) {
			break
		}
	}
	if call == 1 {
		close(r.firstEntered)
		<-r.releaseFirst
	} else if call == 2 {
		close(r.secondEntered)
	}
	return r.Runtime.Create(ctx, req)
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
