package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type journalEntry struct {
	InstanceID   string `json:"instance_id"`
	UserID       string `json:"user_id,omitempty"`
	NetworkOwner string `json:"network_owner,omitempty"`
	Volume       bool   `json:"volume,omitempty"`
	Network      bool   `json:"network,omitempty"`
}

type fileJournal struct {
	path string
	mu   sync.Mutex
}

func newFileJournal(path string) *fileJournal {
	return &fileJournal{path: path}
}

func (j *fileJournal) load() ([]journalEntry, error) {
	if j == nil || j.path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var out []journalEntry
	for {
		var entry journalEntry
		if err := dec.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if entry.InstanceID == "" {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func (j *fileJournal) put(entry journalEntry) error {
	if j == nil || j.path == "" || entry.InstanceID == "" {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := j.loadUnlocked()
	if err != nil {
		return err
	}
	replaced := false
	for i, item := range entries {
		if item.InstanceID == entry.InstanceID {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	return j.writeUnlocked(entries)
}

func (j *fileJournal) remove(instanceID string) error {
	if j == nil || j.path == "" || instanceID == "" {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := j.loadUnlocked()
	if err != nil {
		return err
	}
	filtered := entries[:0]
	for _, item := range entries {
		if item.InstanceID != instanceID {
			filtered = append(filtered, item)
		}
	}
	return j.writeUnlocked(filtered)
}

func (j *fileJournal) loadUnlocked() ([]journalEntry, error) {
	return j.load()
}

func (j *fileJournal) writeUnlocked(entries []journalEntry) error {
	dir := filepath.Dir(j.path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, entry := range entries {
		if err := enc.Encode(entry); err != nil {
			return err
		}
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	_ = os.Remove(j.path)
	return os.Rename(tmp, j.path)
}
