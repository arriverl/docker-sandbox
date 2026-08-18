package dockerx_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

type fakeOccupancy struct {
	mu   sync.Mutex
	busy map[int]bool
}

func (f *fakeOccupancy) Occupied(port int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy[port]
}

func (f *fakeOccupancy) set(port int, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy == nil {
		f.busy = map[int]bool{}
	}
	f.busy[port] = busy
}

func TestPortPool_AllocatesUniquePorts(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{
		Base:    20000,
		Checker: &fakeOccupancy{},
	})
	a, err := pool.Allocate("c1")
	if err != nil {
		t.Fatalf("alloc a: %v", err)
	}
	b, err := pool.Allocate("c2")
	if err != nil {
		t.Fatalf("alloc b: %v", err)
	}
	if a == b {
		t.Fatalf("ports not unique: %d", a)
	}
	if a < 20000 || a >= 20000+dockerx.PortPoolSize {
		t.Fatalf("port %d out of pool", a)
	}
}

func TestPortPool_SkipsOccupiedHostPorts(t *testing.T) {
	occ := &fakeOccupancy{busy: map[int]bool{20000: true, 20001: true}}
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 20000, Checker: occ})
	got, err := pool.Allocate("c1")
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if got != 20002 {
		t.Fatalf("port = %d, want 20002", got)
	}
}

func TestPortPool_ReleaseAllowsReuse(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 30000, Checker: &fakeOccupancy{}})
	p, err := pool.Allocate("c1")
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	pool.Release(p)
	again, err := pool.Allocate("c2")
	if err != nil {
		t.Fatalf("realloc: %v", err)
	}
	if again != p {
		t.Fatalf("reuse = %d, want %d", again, p)
	}
}

func TestPortPool_ExhaustedAt500(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 40000, Checker: &fakeOccupancy{}})
	for i := 0; i < dockerx.PortPoolSize; i++ {
		if _, err := pool.Allocate("full"); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	_, err := pool.Allocate("overflow")
	if !sandboxerr.IsConflict(err) {
		t.Fatalf("err = %v, want conflict (exhausted)", err)
	}
	if pool.InUse() != dockerx.PortPoolSize {
		t.Fatalf("in use = %d, want %d", pool.InUse(), dockerx.PortPoolSize)
	}
}

func TestPortPool_ConcurrentAllocateUnique(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 21000, Checker: &fakeOccupancy{}})
	const n = 80
	got := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			p, err := pool.Allocate("conc")
			if err != nil {
				t.Errorf("alloc: %v", err)
				return
			}
			got[i] = p
		}()
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, p := range got {
		if p == 0 {
			t.Fatal("missing allocation")
		}
		if seen[p] {
			t.Fatalf("duplicate port %d", p)
		}
		seen[p] = true
	}
}

func TestPortPool_ReleaseOwner(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 22000, Checker: &fakeOccupancy{}})
	if _, err := pool.AllocateN("box", 3); err != nil {
		t.Fatalf("alloc n: %v", err)
	}
	if pool.InUse() != 3 {
		t.Fatalf("in use = %d", pool.InUse())
	}
	pool.ReleaseOwner("box")
	if pool.InUse() != 0 {
		t.Fatalf("after release owner in use = %d", pool.InUse())
	}
}

func TestPortPool_ReserveSkipsPortOnAllocate(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 23000, Checker: &fakeOccupancy{}})
	if !pool.Reserve(23000, "existing") {
		t.Fatal("reserve 23000")
	}
	if pool.InUse() != 1 {
		t.Fatalf("in use = %d, want 1", pool.InUse())
	}
	got, err := pool.Allocate("next")
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if got != 23001 {
		t.Fatalf("port = %d, want 23001", got)
	}
}

func TestPortPool_StatsWarnsAt70Percent(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 25000, Checker: &fakeOccupancy{}})
	wantWarn := dockerx.PortPoolWarnMark(dockerx.PortPoolSize)
	if wantWarn != 350 {
		t.Fatalf("warn mark=%d want 350", wantWarn)
	}
	for i := 0; i < wantWarn-1; i++ {
		if _, err := pool.Allocate("fill"); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	stats := pool.Stats()
	if stats.Warn || stats.RestrictMultiPort || stats.InUse != wantWarn-1 {
		t.Fatalf("below warn: %+v", stats)
	}
	if _, err := pool.Allocate("fill"); err != nil {
		t.Fatal(err)
	}
	stats = pool.Stats()
	if !stats.Warn || stats.RestrictMultiPort || stats.InUse != wantWarn {
		t.Fatalf("at warn: %+v", stats)
	}
}

func TestPortPool_RestrictsMultiPortAt85Percent(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 26000, Checker: &fakeOccupancy{}})
	wantRestrict := dockerx.PortPoolRestrictMark(dockerx.PortPoolSize)
	if wantRestrict != 425 {
		t.Fatalf("restrict mark=%d want 425", wantRestrict)
	}
	for i := 0; i < wantRestrict; i++ {
		if _, err := pool.Allocate("fill"); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	stats := pool.Stats()
	if !stats.Warn || !stats.RestrictMultiPort {
		t.Fatalf("at restrict: %+v", stats)
	}
	if err := pool.Admit(2); !sandboxerr.IsConflict(err) {
		t.Fatalf("Admit(2)=%v want conflict", err)
	}
	if _, err := pool.AllocateN("multi", 2); !sandboxerr.IsConflict(err) || !errors.Is(err, dockerx.ErrMultiPortRestricted) {
		t.Fatalf("AllocateN=%v want multi-port restrict", err)
	}
	if pool.InUse() != wantRestrict {
		t.Fatalf("restrict must not leak leases: in use=%d", pool.InUse())
	}
	if _, err := pool.Allocate("single"); err != nil {
		t.Fatalf("single-port alloc should still work: %v", err)
	}
}

func TestPortPool_AdmitAllowsMultiPortBelowRestrict(t *testing.T) {
	pool := dockerx.NewPortPool(dockerx.PortPoolConfig{Base: 27000, Checker: &fakeOccupancy{}})
	for i := 0; i < dockerx.PortPoolRestrictMark(dockerx.PortPoolSize)-1; i++ {
		if _, err := pool.Allocate("fill"); err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
	}
	if err := pool.Admit(2); err != nil {
		t.Fatalf("Admit below restrict: %v", err)
	}
	got, err := pool.AllocateN("multi", 2)
	if err != nil {
		t.Fatalf("AllocateN: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ports=%v", got)
	}
}

func TestPortPool_FileLockSharesReservations(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "ports.lock")
	cfg := func() dockerx.PortPoolConfig {
		return dockerx.PortPoolConfig{
			Base:     24000,
			Checker:  &fakeOccupancy{},
			LockPath: lock,
		}
	}
	a := dockerx.NewPortPool(cfg())
	b := dockerx.NewPortPool(cfg())
	pa, err := a.Allocate("a")
	if err != nil {
		t.Fatalf("alloc a: %v", err)
	}
	pb, err := b.Allocate("b")
	if err != nil {
		t.Fatalf("alloc b: %v", err)
	}
	if pa == pb {
		t.Fatalf("two pools with same lock allocated %d twice", pa)
	}
}
