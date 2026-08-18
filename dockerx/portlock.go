package dockerx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func (p *PortPool) withPersist(fn func() error) error {
	if p.lockPath == "" {
		p.mu.Lock()
		defer p.mu.Unlock()
		return fn()
	}
	if err := os.MkdirAll(filepath.Dir(p.lockPath), 0o700); err != nil && !os.IsExist(err) {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: create port lock dir: %v", sandboxerr.ErrInvalid, err))
	}
	f, err := os.OpenFile(p.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: open port lock: %v", sandboxerr.ErrInvalid, err))
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: lock port file: %v", sandboxerr.ErrInvalid, err))
	}
	defer unlockFile(f)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.loadLocked(f); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return p.saveLocked(f)
}

func (p *PortPool) loadLocked(f *os.File) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: seek port lock: %v", sandboxerr.ErrInvalid, err))
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: read port lock: %v", sandboxerr.ErrInvalid, err))
	}
	if len(raw) == 0 {
		p.used = make(map[int]string, p.size)
		return nil
	}
	var st struct {
		Used map[string]string `json:"used"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: parse port lock: %v", sandboxerr.ErrInvalid, err))
	}
	used := make(map[int]string, len(st.Used))
	for k, owner := range st.Used {
		port, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		used[port] = owner
	}
	p.used = used
	return nil
}

func (p *PortPool) saveLocked(f *os.File) error {
	st := struct {
		Used map[string]string `json:"used"`
	}{Used: make(map[string]string, len(p.used))}
	for port, owner := range p.used {
		st.Used[strconv.Itoa(port)] = owner
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: encode port lock: %v", sandboxerr.ErrInvalid, err))
	}
	if err := f.Truncate(0); err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: truncate port lock: %v", sandboxerr.ErrInvalid, err))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: seek port lock: %v", sandboxerr.ErrInvalid, err))
	}
	if _, err := f.Write(raw); err != nil {
		return sandboxerr.WrapKind("port.allocate", sandboxerr.KindInvalid,
			fmt.Errorf("%w: write port lock: %v", sandboxerr.ErrInvalid, err))
	}
	return f.Sync()
}

func (p *PortPool) reserveLocked(port int, owner string) bool {
	if port < p.base || port >= p.base+p.size {
		return false
	}
	if _, taken := p.used[port]; taken {
		return false
	}
	if owner == "" {
		owner = "_"
	}
	p.used[port] = owner
	return true
}
