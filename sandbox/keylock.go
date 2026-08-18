package sandbox

import "sync"

// keyedLocker serializes mutations for one key while allowing unrelated keys
// to proceed independently. Entries are reference-counted so idle keys do not
// accumulate for the lifetime of the service.
type keyedLocker struct {
	mu      sync.Mutex
	entries map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

func (l *keyedLocker) lock(key string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*keyLock)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &keyLock{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()

		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}
