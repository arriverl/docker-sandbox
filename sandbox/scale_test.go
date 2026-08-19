package sandbox

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"
)

func TestScale_300ListAndCreateP95(t *testing.T) {
	world := newMemWorld()
	svc, err := New(Config{Prefix: "sandbox", MaxInstances: DesignPeakInstances}, world.plugins())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	createDurations := make([]time.Duration, 0, DesignPeakInstances)
	for i := 0; i < DesignPeakInstances; i++ {
		spec := Spec{
			InstanceID: fmt.Sprintf("box-%03d", i),
			UserID:     "alice",
			Image:      "alpine:3.20",
		}
		started := time.Now()
		if _, err := svc.Create(ctx, spec); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		createDurations = append(createDurations, time.Since(started))
	}
	listDurations := make([]time.Duration, 0, 50)
	for i := 0; i < 50; i++ {
		started := time.Now()
		got, err := svc.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != DesignPeakInstances {
			t.Fatalf("list len=%d want %d", len(got), DesignPeakInstances)
		}
		listDurations = append(listDurations, time.Since(started))
	}
	createP95 := percentileDuration(createDurations, 0.95)
	listP95 := percentileDuration(listDurations, 0.95)
	t.Logf("in-memory 300-instance create p95=%s list p95=%s", createP95, listP95)
	if createP95 <= 0 || listP95 <= 0 {
		t.Fatal("p95 must be measured")
	}
}

func percentileDuration(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
