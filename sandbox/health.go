package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

var errProbeNotReady = errors.New("probe not ready")

type probeNotReadyError struct{ err error }

func (e *probeNotReadyError) Error() string { return e.err.Error() }
func (e *probeNotReadyError) Unwrap() error { return e.err }
func (e *probeNotReadyError) Is(target error) bool {
	return target == errProbeNotReady
}

// ProbeNotReady 标记可重试的“尚未就绪”结果，供自定义 Health 插件使用。
func ProbeNotReady(err error) error {
	if err == nil {
		return nil
	}
	return &probeNotReadyError{err: err}
}

// IsProbeNotReady 判断探针错误是否可重试。
func IsProbeNotReady(err error) bool {
	return errors.Is(err, errProbeNotReady)
}

// StdHealth 用 HTTP / TCP / Exec 探测实例就绪。
type StdHealth struct {
	HTTP   *http.Client
	Dialer *net.Dialer
	Exec   func(ctx context.Context, containerID string, cmd []string) error
}

// NewStdHealth 构造默认健康检查插件。
func NewStdHealth(exec func(ctx context.Context, containerID string, cmd []string) error) *StdHealth {
	return &StdHealth{
		HTTP:   &http.Client{Timeout: 2 * time.Second},
		Dialer: &net.Dialer{Timeout: 2 * time.Second},
		Exec:   exec,
	}
}

func (h *StdHealth) Check(ctx context.Context, inst Instance, probe Probe) error {
	if h == nil {
		return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
			fmt.Errorf("%w: health plugin is nil", sandboxerr.ErrInvalid))
	}
	if err := ValidateProbe(inst, probe); err != nil {
		return err
	}
	switch probe.Kind {
	case ProbeHTTP, "":
		return h.checkHTTP(ctx, inst, probe)
	case ProbeTCP:
		return h.checkTCP(ctx, inst, probe)
	case ProbeExec:
		return h.checkExec(ctx, inst, probe)
	default:
		return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
			fmt.Errorf("%w: unknown probe %q", sandboxerr.ErrInvalid, probe.Kind))
	}
}

// ValidateProbe 在开始轮询前验证不会随时间变化的探针配置。
func ValidateProbe(inst Instance, probe Probe) error {
	switch probe.Kind {
	case ProbeHTTP, "", ProbeTCP:
		_, err := probePort(inst, probe)
		return err
	case ProbeExec:
		if inst.ContainerID == "" {
			return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
				fmt.Errorf("%w: instance is not running", sandboxerr.ErrInvalid))
		}
		if len(probe.Command) == 0 {
			return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
				fmt.Errorf("%w: exec command is empty", sandboxerr.ErrInvalid))
		}
		return nil
	default:
		return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
			fmt.Errorf("%w: unknown probe %q", sandboxerr.ErrInvalid, probe.Kind))
	}
}

func (h *StdHealth) checkHTTP(ctx context.Context, inst Instance, probe Probe) error {
	port, err := probePort(inst, probe)
	if err != nil {
		return err
	}
	path := probe.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	client := h.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return ProbeNotReady(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ProbeNotReady(fmt.Errorf("http %d", resp.StatusCode))
	}
	return nil
}

func (h *StdHealth) checkTCP(ctx context.Context, inst Instance, probe Probe) error {
	port, err := probePort(inst, probe)
	if err != nil {
		return err
	}
	d := h.Dialer
	if d == nil {
		d = &net.Dialer{Timeout: 2 * time.Second}
	}
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return ProbeNotReady(err)
	}
	return c.Close()
}

func (h *StdHealth) checkExec(ctx context.Context, inst Instance, probe Probe) error {
	if h.Exec == nil {
		return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
			fmt.Errorf("%w: exec probe is not configured", sandboxerr.ErrInvalid))
	}
	if inst.ContainerID == "" {
		return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
			fmt.Errorf("%w: instance is not running", sandboxerr.ErrInvalid))
	}
	if len(probe.Command) == 0 {
		return sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
			fmt.Errorf("%w: exec command is empty", sandboxerr.ErrInvalid))
	}
	err := h.Exec(ctx, inst.ContainerID, probe.Command)
	if err != nil && sandboxerr.IsInvalid(err) {
		return ProbeNotReady(err)
	}
	return err
}

func probePort(inst Instance, probe Probe) (int, error) {
	if probe.Port > 0 {
		return probe.Port, nil
	}
	if len(inst.HostPorts) > 0 {
		return inst.HostPorts[0], nil
	}
	return 0, sandboxerr.WrapKind("健康检查", sandboxerr.KindInvalid,
		fmt.Errorf("%w: no host port for probe", sandboxerr.ErrInvalid))
}
