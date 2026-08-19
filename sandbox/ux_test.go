package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestEnsureRunning_CreatesStartsAndWaits(t *testing.T) {
	world := newMemWorld()
	svc := testService(t, world)
	got, err := svc.EnsureRunning(context.Background(), sampleSpec(), Probe{Kind: ProbeTCP})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("status=%s want running", got.Status)
	}
}

func TestEnsureRunning_UsesExistingInstance(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	got, err := svc.EnsureRunning(context.Background(), sampleSpec(), Probe{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("status=%s want running", got.Status)
	}
}

func TestEnsureRunning_DestroyedRequiresRestore(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Destroy(context.Background(), "box1", DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.EnsureRunning(context.Background(), sampleSpec(), Probe{})
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err=%v want conflict", err)
	}
	if sandboxerr.ActionOf(err) != sandboxerr.ActionUseRestore {
		t.Fatalf("action=%q want %s", sandboxerr.ActionOf(err), sandboxerr.ActionUseRestore)
	}
}

func TestEnsureRunning_MaxInstancesKeepsRetryAfter(t *testing.T) {
	world := newMemWorld()
	svc, err := New(Config{Prefix: "sandbox", MaxInstances: 1}, world.plugins())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	second := sampleSpec()
	second.InstanceID = "box2"
	_, err = svc.EnsureRunning(context.Background(), second, Probe{})
	if sandboxerr.ActionOf(err) != sandboxerr.ActionRetryAfter {
		t.Fatalf("action=%q want %s err=%v", sandboxerr.ActionOf(err), sandboxerr.ActionRetryAfter, err)
	}
}

func TestCreate_DestroyedSetsUseRestoreAction(t *testing.T) {
	svc := testService(t, newMemWorld())
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Destroy(context.Background(), "box1", DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Create(context.Background(), sampleSpec())
	if sandboxerr.ActionOf(err) != sandboxerr.ActionUseRestore {
		t.Fatalf("action=%q want %s err=%v", sandboxerr.ActionOf(err), sandboxerr.ActionUseRestore, err)
	}
}

func TestCreate_MaxInstancesSetsRetryAfterAction(t *testing.T) {
	world := newMemWorld()
	svc, err := New(Config{Prefix: "sandbox", MaxInstances: 1}, world.plugins())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	second := sampleSpec()
	second.InstanceID = "box2"
	_, err = svc.Create(context.Background(), second)
	if sandboxerr.ActionOf(err) != sandboxerr.ActionRetryAfter {
		t.Fatalf("action=%q want %s err=%v", sandboxerr.ActionOf(err), sandboxerr.ActionRetryAfter, err)
	}
}

func TestWaitReady_EmitsRetryEvents(t *testing.T) {
	world := newMemWorld()
	world.health = func(_ Instance, _ Probe, n int) error {
		if n < 3 {
			return errors.New("connection refused")
		}
		return nil
	}
	observer := &recordingObserver{}
	svc, err := New(Config{Prefix: "sandbox", WaitInterval: 10 * time.Millisecond, Observer: observer}, world.plugins())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WaitReady(context.Background(), "box1", Probe{Kind: ProbeTCP}); err != nil {
		t.Fatal(err)
	}
	var retries []OperationEvent
	for _, event := range observer.events {
		if event.Stage == "wait.retry" {
			retries = append(retries, event)
		}
	}
	if len(retries) == 0 {
		t.Fatalf("stages=%v want wait.retry", observer.stages())
	}
	if retries[0].Attempt < 1 || retries[0].Detail == "" {
		t.Fatalf("retry event=%+v", retries[0])
	}
}

func TestCreate_PerInstanceNetworkIsolatesSameUser(t *testing.T) {
	world := newMemWorld()
	svc := testService(t, world)
	first := sampleSpec()
	first.NetworkIsolation = NetworkPerInstance
	second := sampleSpec()
	second.InstanceID = "box2"
	second.NetworkIsolation = NetworkPerInstance
	a, err := svc.Create(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Create(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if a.Network == "" || a.Network == b.Network {
		t.Fatalf("want distinct instance networks, got %q and %q", a.Network, b.Network)
	}
	if a.Network == "sandbox-net-alice" || b.Network == "sandbox-net-alice" {
		t.Fatalf("per-instance must not reuse user network: %q %q", a.Network, b.Network)
	}
	if err := svc.Destroy(context.Background(), "box1", DestroyOptions{Purge: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := world.nets[networkOwner(first)]; ok {
		t.Fatal("destroyed instance network must be released")
	}
	if _, ok := world.nets[networkOwner(second)]; !ok {
		t.Fatal("sibling instance network must remain")
	}
}

func TestEncodeDecodeSpecPreservesNetworkIsolation(t *testing.T) {
	spec := sampleSpec()
	spec.NetworkIsolation = NetworkPerInstance
	raw, err := EncodeSpecChecked(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := DecodeSpec(raw)
	if !ok || got.NetworkIsolation != NetworkPerInstance {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
}

func TestNew_UnreadableJournalPathIsInvalid(t *testing.T) {
	_, err := New(Config{Prefix: "sandbox", JournalPath: t.TempDir()}, newMemWorld().plugins())
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err=%v want invalid", err)
	}
}

func TestCreate_JournalWriteFailureIsSurfaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	world := newMemWorld()
	plugins := world.plugins()
	plugins.Storage = &failDestroyOnceStorage{Storage: plugins.Storage}
	plugins.Runtime = &failOnceRuntime{Runtime: plugins.Runtime}
	svc, err := New(Config{Prefix: "sandbox", JournalPath: path}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Create(context.Background(), sampleSpec())
	if err == nil {
		t.Fatal("first create should fail")
	}
	if !strings.Contains(err.Error(), "写入补偿日志失败") {
		t.Fatalf("want journal write error, got %q", err)
	}
}

func TestCreate_ReplaysCompensationJournalAfterNewService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	world := newMemWorld()
	plugins := world.plugins()
	plugins.Storage = &failDestroyOnceStorage{Storage: plugins.Storage}
	plugins.Runtime = &failOnceRuntime{Runtime: plugins.Runtime}
	svc, err := New(Config{Prefix: "sandbox", JournalPath: path}, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), sampleSpec()); err == nil {
		t.Fatal("first create should fail")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "box1") {
		t.Fatalf("journal missing instance: %s", raw)
	}
	svc2, err := New(Config{Prefix: "sandbox", JournalPath: path}, world.plugins())
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc2.Create(context.Background(), sampleSpec())
	if err != nil {
		t.Fatalf("create after journal replay: %v", err)
	}
	if got.Status != StatusCreated {
		t.Fatalf("status=%s", got.Status)
	}
	if leftover, err := os.ReadFile(path); err == nil && strings.Contains(string(leftover), `"instance_id":"box1"`) {
		t.Fatalf("journal should drop completed entry: %s", leftover)
	}
}
