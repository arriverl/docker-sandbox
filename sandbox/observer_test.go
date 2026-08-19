package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestLogObserverWritesJSONEvent(t *testing.T) {
	var buf bytes.Buffer
	obs := LogObserver{Writer: &buf}
	obs.Observe(context.Background(), OperationEvent{
		Operation:  "create",
		Stage:      "operation.end",
		InstanceID: "box1",
		UserID:     "alice",
		Duration:   12 * time.Millisecond,
		Kind:       sandboxerr.KindConflict,
		Err:        sandboxerr.ErrConflict,
	})
	line := strings.TrimSpace(buf.String())
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("json: %v line=%q", err, line)
	}
	if got["operation"] != "create" || got["stage"] != "operation.end" || got["instance_id"] != "box1" {
		t.Fatalf("event=%v", got)
	}
	if got["kind"] != string(sandboxerr.KindConflict) {
		t.Fatalf("kind=%v", got["kind"])
	}
}
