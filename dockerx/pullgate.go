package dockerx

import (
	"context"
	"sync"
)

type pullCall struct {
	done chan struct{}
	err  error
}

type pullGate struct {
	mu sync.Mutex
	in map[string]*pullCall
}

func newPullGate() *pullGate {
	return &pullGate{in: map[string]*pullCall{}}
}

func (g *pullGate) Do(ctx context.Context, key string, fn func() error) error {
	if g == nil {
		return fn()
	}
	g.mu.Lock()
	if g.in == nil {
		g.in = map[string]*pullCall{}
	}
	if c, ok := g.in[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &pullCall{done: make(chan struct{})}
	g.in[key] = c
	g.mu.Unlock()

	c.err = fn()

	g.mu.Lock()
	delete(g.in, key)
	close(c.done)
	g.mu.Unlock()
	return c.err
}
