package dockerx

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

const (
	// PortPoolSize 是宿主机端口池容量上限。
	PortPoolSize = 500
	// DefaultPortBase 是端口池起始端口（含）。池范围为 [Base, Base+PortPoolSize)。
	DefaultPortBase = 20000
)

// PortChecker 检测宿主机端口是否已被占用。
type PortChecker interface {
	Occupied(port int) bool
}

// TCPPortChecker 通过尝试 Listen 判断 TCP 端口是否空闲。
type TCPPortChecker struct{}

// Occupied 在本机绑定失败时视为占用。
func (TCPPortChecker) Occupied(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	_ = ln.Close()
	return false
}

// PortPoolConfig 描述端口池。
type PortPoolConfig struct {
	Base     int
	Checker  PortChecker
	LockPath string
}

// PortPool 从固定窗口动态分配/回收宿主机端口，支持并发。
type PortPool struct {
	mu       sync.Mutex
	base     int
	size     int
	used     map[int]string
	checker  PortChecker
	lockPath string
}

// DefaultPortLockPath 返回跨进程端口池锁文件路径。
func DefaultPortLockPath(prefix string, base int) string {
	if prefix == "" {
		prefix = DefaultNetworkPrefix
	}
	if base <= 0 {
		base = DefaultPortBase
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("docker-sandbox-%s-%d.ports", prefix, base))
}

// NewPortPool 构造容量为 PortPoolSize 的端口池。
func NewPortPool(cfg PortPoolConfig) *PortPool {
	base := cfg.Base
	if base <= 0 {
		base = DefaultPortBase
	}
	checker := cfg.Checker
	if checker == nil {
		checker = TCPPortChecker{}
	}
	return &PortPool{
		base:     base,
		size:     PortPoolSize,
		used:     make(map[int]string, PortPoolSize),
		checker:  checker,
		lockPath: cfg.LockPath,
	}
}

// Allocate 分配一个空闲且未被占用的宿主机端口。
func (p *PortPool) Allocate(owner string) (int, error) {
	var port int
	err := p.withPersist(func() error {
		var e error
		port, e = p.allocateLocked(owner)
		return e
	})
	return port, err
}

// AllocateN 连续分配 n 个端口；失败时回滚本次分配。
func (p *PortPool) AllocateN(owner string, n int) ([]int, error) {
	if n <= 0 {
		return nil, nil
	}
	var out []int
	err := p.withPersist(func() error {
		out = make([]int, 0, n)
		for i := 0; i < n; i++ {
			port, err := p.allocateLocked(owner)
			if err != nil {
				for _, got := range out {
					delete(p.used, got)
				}
				out = nil
				return err
			}
			out = append(out, port)
		}
		return nil
	})
	return out, err
}

func (p *PortPool) allocateLocked(owner string) (int, error) {
	if owner == "" {
		owner = "_"
	}
	for i := 0; i < p.size; i++ {
		port := p.base + i
		if _, taken := p.used[port]; taken {
			continue
		}
		if p.checker != nil && p.checker.Occupied(port) {
			continue
		}
		p.used[port] = owner
		return port, nil
	}
	return 0, sandboxerr.WrapKind("port.allocate", sandboxerr.KindConflict,
		fmt.Errorf("%w: host port pool exhausted (%d)", sandboxerr.ErrConflict, p.size))
}

// Reserve 将已占用端口标为已分配（用于从 Docker 重建）。超出池范围或已占用则返回 false。
func (p *PortPool) Reserve(port int, owner string) bool {
	var ok bool
	_ = p.withPersist(func() error {
		ok = p.reserveLocked(port, owner)
		return nil
	})
	return ok
}

// Release 归还单个端口。
func (p *PortPool) Release(port int) {
	_ = p.withPersist(func() error {
		delete(p.used, port)
		return nil
	})
}

// ReleaseOwner 归还某所有者占用的全部端口。
func (p *PortPool) ReleaseOwner(owner string) {
	_ = p.withPersist(func() error {
		for port, got := range p.used {
			if got == owner {
				delete(p.used, port)
			}
		}
		return nil
	})
}

// InUse 返回当前已分配端口数。
func (p *PortPool) InUse() int {
	n := 0
	_ = p.withPersist(func() error {
		n = len(p.used)
		return nil
	})
	return n
}
