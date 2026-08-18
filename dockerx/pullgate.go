package dockerx

import "sync"

type pullCall struct {
	wg  sync.WaitGroup
	err error
}

type pullGate struct {
	mu sync.Mutex
	in map[string]*pullCall
}

func newPullGate() *pullGate {
	return &pullGate{in: map[string]*pullCall{}}
}

func (g *pullGate) Do(key string, fn func() error) error {
	if g == nil {
		return fn()
	}
	g.mu.Lock()
	if g.in == nil {
		g.in = map[string]*pullCall{}
	}
	if c, ok := g.in[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.err
	}
	c := &pullCall{}
	c.wg.Add(1)
	g.in[key] = c
	g.mu.Unlock()

	c.err = fn()

	g.mu.Lock()
	delete(g.in, key)
	g.mu.Unlock()
	c.wg.Done()
	return c.err
}
