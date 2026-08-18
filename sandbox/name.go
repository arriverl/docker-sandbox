package sandbox

import (
	"strings"
	"unicode"
)

// ContainerName 返回稳定容器名 {prefix}-{instanceId}。
func ContainerName(prefix, instanceID string) string {
	return sanitizeName(prefixOrDefault(prefix)) + "-" + sanitizeName(instanceID)
}

func prefixOrDefault(prefix string) string {
	if strings.TrimSpace(prefix) == "" {
		return DefaultPrefix
	}
	return strings.TrimSpace(prefix)
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i, r := range s {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-'
		if ok && (i > 0 || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "id"
	}
	return out
}
