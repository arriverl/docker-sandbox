package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestParseLimitsReturnInvalidKind(t *testing.T) {
	if _, err := parseMemory("bad"); !sandboxerr.IsInvalid(err) {
		t.Fatalf("parseMemory err=%v want invalid", err)
	}
	if _, err := parseCPUs("-1"); !sandboxerr.IsInvalid(err) {
		t.Fatalf("parseCPUs err=%v want invalid", err)
	}
}

func TestParseLimitsAcceptRecommendedDefaults(t *testing.T) {
	mem, err := parseMemory("256m")
	if err != nil || mem != 256<<20 {
		t.Fatalf("parseMemory(256m)=%d err=%v", mem, err)
	}
	cpus, err := parseCPUs("0.5")
	if err != nil || cpus != 500_000_000 {
		t.Fatalf("parseCPUs(0.5)=%d err=%v", cpus, err)
	}
}

func TestRunBuildMissingDockerfileReturnsNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.Dockerfile")
	_, err := runBuild(context.Background(), nil, []string{
		"-tag", "example:test",
		"-dockerfile", missing,
	})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("runBuild err=%v want not_found", err)
	}
}
