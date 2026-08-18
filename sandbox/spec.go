package sandbox

import (
	"encoding/json"
	"strconv"
	"strings"
)

type compactSpec struct {
	InstanceID string            `json:"instance_id"`
	UserID     string            `json:"user_id"`
	Image      string            `json:"image"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        []string          `json:"env,omitempty"`
	Memory     int64             `json:"memory,omitempty"`
	NanoCPUs   int64             `json:"nano_cpus,omitempty"`
	Expose     []string          `json:"expose,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// EncodeSpec 把规格编码为可写入 Docker label 的 JSON。
func EncodeSpec(spec Spec) string {
	raw, err := json.Marshal(compactSpec{
		InstanceID: strings.TrimSpace(spec.InstanceID),
		UserID:     strings.TrimSpace(spec.UserID),
		Image:      strings.TrimSpace(spec.Image),
		Cmd:        spec.Cmd,
		Env:        spec.Env,
		Memory:     spec.Memory,
		NanoCPUs:   spec.NanoCPUs,
		Expose:     spec.Expose,
		Labels:     userLabels(spec.Labels),
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

// DecodeSpec 从 label JSON 恢复规格。
func DecodeSpec(raw string) (Spec, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Spec{}, false
	}
	var c compactSpec
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Spec{}, false
	}
	if strings.TrimSpace(c.InstanceID) == "" || strings.TrimSpace(c.Image) == "" {
		return Spec{}, false
	}
	return Spec{
		InstanceID: strings.TrimSpace(c.InstanceID),
		UserID:     strings.TrimSpace(c.UserID),
		Image:      strings.TrimSpace(c.Image),
		Cmd:        c.Cmd,
		Env:        c.Env,
		Memory:     c.Memory,
		NanoCPUs:   c.NanoCPUs,
		Expose:     c.Expose,
		Labels:     c.Labels,
	}, true
}

func userLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if strings.HasPrefix(k, "sandbox.") {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseHostPorts(raw string) []int {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

func joinHostPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+8)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
