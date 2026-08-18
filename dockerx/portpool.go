package dockerx

import (
	"errors"
	"fmt"
	"math"
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
	// PortPoolWarnRatio 是告警水位。默认 500 端口时对应 350 个。
	PortPoolWarnRatio = 0.70
	// PortPoolRestrictRatio 是多端口实例准入水位。默认 500 端口时对应 425 个。
	PortPoolRestrictRatio = 0.85
)

// ErrMultiPortRestricted 表示端口池已达 85% 水位，拒绝新的多端口实例。
var ErrMultiPortRestricted = errors.New("host port pool refuses new multi-port instance")

// PortPoolStats 描述端口池占用与运维水位。
type PortPoolStats struct {
	Size              int
	InUse             int
	WarnMark          int
	RestrictMark      int
	Warn              bool
	RestrictMultiPort bool
}

// PortPoolWarnMark 返回告警占用数。
func PortPoolWarnMark(size int) int {
	return ratioMark(size, PortPoolWarnRatio)
}

// PortPoolRestrictMark 返回停止接收多端口新实例的占用数。
func PortPoolRestrictMark(size int) int {
	return ratioMark(size, PortPoolRestrictRatio)
}

func ratioMark(size int, ratio float64) int {
	if size <= 0 {
		return 0
	}
	mark := int(math.Ceil(float64(size) * ratio))
	if mark < 1 {
		mark = 1
	}
	if mark > size {
		return size
	}
	return mark
}

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
	if err := p.Admit(n); err != nil {
		return nil, err
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
	return p.Stats().InUse
}

// Stats 返回端口池占用与水位。
func (p *PortPool) Stats() PortPoolStats {
	if p == nil {
		return PortPoolStats{}
	}
	var st PortPoolStats
	_ = p.withPersist(func() error {
		st = p.statsLocked()
		return nil
	})
	return st
}

func (p *PortPool) statsLocked() PortPoolStats {
	size := p.size
	inUse := len(p.used)
	warnMark := PortPoolWarnMark(size)
	restrictMark := PortPoolRestrictMark(size)
	return PortPoolStats{
		Size:              size,
		InUse:             inUse,
		WarnMark:          warnMark,
		RestrictMark:      restrictMark,
		Warn:              inUse >= warnMark && warnMark > 0,
		RestrictMultiPort: inUse >= restrictMark && restrictMark > 0,
	}
}

// Admit 在分配前检查运维准入。n>1 且占用已达 85% 时拒绝多端口新实例。
func (p *PortPool) Admit(n int) error {
	if p == nil || n <= 1 {
		return nil
	}
	if !p.Stats().RestrictMultiPort {
		return nil
	}
	return sandboxerr.WrapKind("port.allocate", sandboxerr.KindConflict,
		fmt.Errorf("%w: %w", sandboxerr.ErrConflict, ErrMultiPortRestricted))
}
