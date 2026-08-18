package dockerx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/client"

	"github.com/gitproduct/docker-sandbox/dockerx"
)

func TestConfig_ResolveHostPrefersExplicitHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://env-host:2375")
	got := dockerx.Config{Host: "tcp://explicit:2375"}.ResolveHost()
	if got != "tcp://explicit:2375" {
		t.Fatalf("ResolveHost = %q, want explicit host", got)
	}
}

func TestConfig_ResolveHostPrefersDOCKER_HOST(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://from-env:2375")
	got := dockerx.Config{}.ResolveHost()
	if got != "tcp://from-env:2375" {
		t.Fatalf("ResolveHost = %q, want DOCKER_HOST", got)
	}
}

func TestConfig_ResolveHostFromDockerContext(t *testing.T) {
	dir := t.TempDir()
	writeDockerContext(t, dir, "desktop-linux", "npipe:////./pipe/dockerDesktopLinuxEngine")
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_HOST", "")

	got := dockerx.Config{}.ResolveHost()
	if got != "npipe:////./pipe/dockerDesktopLinuxEngine" {
		t.Fatalf("ResolveHost = %q, want Docker Desktop npipe from current context", got)
	}
}

func TestConfig_ResolveHostFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_HOST", "")

	got := dockerx.Config{}.ResolveHost()
	if got != client.DefaultDockerHost {
		t.Fatalf("ResolveHost = %q, want %q", got, client.DefaultDockerHost)
	}
}

func writeDockerContext(t *testing.T, dockerConfigDir, name, host string) {
	t.Helper()
	cfg := []byte(`{"currentContext":"` + name + `"}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	metaDir := filepath.Join(dockerConfigDir, "contexts", "meta", "testhash")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `"}}}`)
	if err := os.WriteFile(filepath.Join(metaDir, "meta.json"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
}
