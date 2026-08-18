package dockerx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/client"
)

type dockerConfigFile struct {
	CurrentContext string `json:"currentContext"`
}

type dockerContextMeta struct {
	Name      string                    `json:"Name"`
	Endpoints map[string]dockerEndpoint `json:"Endpoints"`
}

type dockerEndpoint struct {
	Host string `json:"Host"`
}

// ResolveHost 按优先级解析 Docker daemon 地址：
// Config.Host > DOCKER_HOST > Docker CLI current context > 平台默认值。
// Windows Docker Desktop 的 current context 通常指向
// npipe:////./pipe/dockerDesktopLinuxEngine，而不是 SDK 默认的 docker_engine。
func (c Config) ResolveHost() string {
	if host := strings.TrimSpace(c.Host); host != "" {
		return host
	}
	if host := strings.TrimSpace(os.Getenv("DOCKER_HOST")); host != "" {
		return host
	}
	if host := hostFromDockerContext(); host != "" {
		return host
	}
	return client.DefaultDockerHost
}

func (c Config) effectiveHost() string {
	return c.ResolveHost()
}

func hostFromDockerContext() string {
	configDir := os.Getenv("DOCKER_CONFIG")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configDir = filepath.Join(home, ".docker")
	}

	raw, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg dockerConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ""
	}
	name := strings.TrimSpace(cfg.CurrentContext)
	if name == "" || name == "default" {
		return ""
	}

	metaRoot := filepath.Join(configDir, "contexts", "meta")
	entries, err := os.ReadDir(metaRoot)
	if err != nil {
		return ""
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		metaRaw, err := os.ReadFile(filepath.Join(metaRoot, ent.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var meta dockerContextMeta
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			continue
		}
		if meta.Name != name {
			continue
		}
		if ep, ok := meta.Endpoints["docker"]; ok {
			return strings.TrimSpace(ep.Host)
		}
	}
	return ""
}
