package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

// LogObserver 把 OperationEvent 写成一行 JSON，供日志采集。
type LogObserver struct {
	Writer io.Writer
}

type logEvent struct {
	Operation  string `json:"operation"`
	Stage      string `json:"stage"`
	InstanceID string `json:"instance_id"`
	UserID     string `json:"user_id"`
	DurationMS int64  `json:"duration_ms"`
	Kind       string `json:"kind,omitempty"`
	Error      string `json:"error,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Current    int64  `json:"current,omitempty"`
	Total      int64  `json:"total,omitempty"`
}

func (o LogObserver) Observe(_ context.Context, event OperationEvent) {
	w := o.Writer
	if w == nil {
		w = os.Stderr
	}
	payload := logEvent{
		Operation:  event.Operation,
		Stage:      event.Stage,
		InstanceID: event.InstanceID,
		UserID:     event.UserID,
		DurationMS: event.Duration.Milliseconds(),
		Attempt:    event.Attempt,
		Detail:     event.Detail,
		Current:    event.Current,
		Total:      event.Total,
	}
	if event.Err != nil {
		payload.Kind = string(event.Kind)
		if payload.Kind == "" {
			payload.Kind = string(sandboxerr.Classify(event.Err))
		}
		payload.Error = event.Err.Error()
	} else if event.Kind != "" && event.Kind != sandboxerr.KindUnknown {
		payload.Kind = string(event.Kind)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}
