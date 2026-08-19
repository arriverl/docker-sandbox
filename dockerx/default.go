package dockerx

import (
	"github.com/gitproduct/docker-sandbox/sandbox"
)

// DefaultConfig 返回适合本机 Docker Desktop / Engine 的客户端配置。
// 默认关闭 HTTP 代理，避免 Windows npipe 被 HTTP_PROXY 劫持。
func DefaultConfig() Config {
	return Config{DisableProxy: true}
}

// DefaultSandboxConfig 返回推荐配额、峰值上限和日志 Observer。
func DefaultSandboxConfig() UserSandboxConfig {
	return UserSandboxConfig{
		DefaultMemory:   sandbox.RecommendedDefaultMemory,
		DefaultNanoCPUs: sandbox.RecommendedDefaultNanoCPUs,
		MaxInstances:    sandbox.DesignPeakInstances,
		Observer:        sandbox.LogObserver{},
	}
}

// OpenDefault 装配禁用代理的 Docker 客户端和推荐配额编排器。
func OpenDefault() (*Client, *sandbox.Service, error) {
	client, err := New(DefaultConfig())
	if err != nil {
		return nil, nil, err
	}
	orch, err := NewOrchestrator(client, DefaultSandboxConfig())
	if err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return client, orch, nil
}
