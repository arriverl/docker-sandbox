package dockerx

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/docker/docker/client"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

const defaultTimeout = 30 * time.Second

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

func (c Config) validate() error {
	if c.DisableProxy && c.ProxyURL != "" {
		return sandboxerr.WrapKind("config", sandboxerr.KindInvalid,
			fmt.Errorf("%w: DisableProxy cannot be combined with ProxyURL", sandboxerr.ErrInvalid))
	}

	host := c.effectiveHost()
	if host != "" {
		u, err := client.ParseHostURL(host)
		if err != nil {
			return sandboxerr.WrapKind("config", sandboxerr.KindInvalid, fmt.Errorf("%w: %v", sandboxerr.ErrInvalid, err))
		}
		if err := checkScheme(u.Scheme); err != nil {
			return err
		}
		if c.ProxyURL != "" && !proxyCompatible(u.Scheme) {
			return sandboxerr.WrapKind("config", sandboxerr.KindInvalid,
				fmt.Errorf("%w: proxy is only supported for tcp/http/https hosts, got %s", sandboxerr.ErrInvalid, u.Scheme))
		}
	} else if c.ProxyURL != "" {
		return sandboxerr.WrapKind("config", sandboxerr.KindInvalid,
			fmt.Errorf("%w: proxy requires an explicit tcp/http/https docker host", sandboxerr.ErrInvalid))
	}
	if c.ProxyURL != "" {
		if _, err := parseProxyURL(c.ProxyURL); err != nil {
			return err
		}
	}
	return nil
}

func checkScheme(scheme string) error {
	switch scheme {
	case "unix", "npipe", "tcp", "http", "https", "fd", "ssh":
		return nil
	default:
		return sandboxerr.WrapKind("config", sandboxerr.KindInvalid,
			fmt.Errorf("%w: unsupported docker host scheme %q", sandboxerr.ErrInvalid, scheme))
	}
}

func proxyCompatible(scheme string) bool {
	switch scheme {
	case "tcp", "http", "https":
		return true
	default:
		return false
	}
}

func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, sandboxerr.WrapKind("config", sandboxerr.KindInvalid,
			fmt.Errorf("%w: invalid proxy url %q", sandboxerr.ErrInvalid, raw))
	}
	return u, nil
}

func noProxy(*http.Request) (*url.URL, error) {
	return nil, nil
}

func (c Config) hostScheme() string {
	u, err := client.ParseHostURL(c.ResolveHost())
	if err != nil {
		return ""
	}
	return u.Scheme
}

func (c Config) proxyFunc() (func(*http.Request) (*url.URL, error), error) {
	if c.DisableProxy {
		return noProxy, nil
	}
	if !proxyCompatible(c.hostScheme()) {
		// unix/npipe 等本地通道绝不能走 HTTP 代理，否则无法连上 daemon。
		return noProxy, nil
	}
	if c.ProxyURL != "" {
		proxy, err := parseProxyURL(c.ProxyURL)
		if err != nil {
			return nil, err
		}
		return http.ProxyURL(proxy), nil
	}
	return http.ProxyFromEnvironment, nil
}

// HTTPClient 构造带超时与代理策略的 HTTP 客户端。
func (c Config) HTTPClient() (*http.Client, error) {
	proxyFn, err := c.proxyFunc()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy: proxyFn,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   c.timeout(),
	}, nil
}
