package main

import (
	"strings"
	"testing"

	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestRejectCreateWithoutInstance(t *testing.T) {
	err := rejectCreateWithoutInstance()
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("err=%v want invalid", err)
	}
	if !strings.Contains(err.Error(), "legacy-create") {
		t.Fatalf("want legacy-create hint, got %q", err)
	}
}

func TestFormatInstanceIncludesAccessURL(t *testing.T) {
	got := formatInstanceHuman(sandbox.Instance{
		ID:        "box1",
		Status:    sandbox.StatusRunning,
		Image:     "alpine:3.20",
		HostPorts: []int{20000},
	})
	if !strings.Contains(got, "box1") || !strings.Contains(got, "running") {
		t.Fatalf("identity missing: %q", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:20000") {
		t.Fatalf("want access URL, got %q", got)
	}
}

func TestFormatWaitHeartbeat(t *testing.T) {
	got := formatWaitHeartbeat(1200, 3, "connection refused")
	if !strings.Contains(got, "elapsed=") || !strings.Contains(got, "attempt=3") || !strings.Contains(got, "connection refused") {
		t.Fatalf("heartbeat=%q", got)
	}
}

func TestUsageMentionsLifecycleChinese(t *testing.T) {
	got := usageText()
	for _, want := range []string{"下课保数据", "删容器", "清空作业", "-json", "legacy-create"} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage missing %q in %s", want, got)
		}
	}
}
