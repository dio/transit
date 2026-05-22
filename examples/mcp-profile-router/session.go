package mcpprofilerouter

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
)

var sessionCounter atomic.Uint64

type compositeSession struct {
	Route    string
	Profile  string
	Subject  string
	Backends map[string]string
}

func newSessionID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, sessionCounter.Add(1))
}

func encodeCompositeSession(route, profile, subject string, backends map[string]string) string {
	var b strings.Builder
	b.WriteString(route)
	b.WriteString("@")
	b.WriteString(profile)
	b.WriteString("@")
	b.WriteString(subject)
	b.WriteString("@")
	ids := sortedStringKeys(backends)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(id)
		b.WriteString(":")
		b.WriteString(base64.StdEncoding.EncodeToString([]byte(backends[id])))
	}
	return b.String()
}

func decodeCompositeSession(raw string) (compositeSession, error) {
	firstAt := strings.Index(raw, "@")
	if firstAt < 0 {
		return compositeSession{}, fmt.Errorf("invalid session ID")
	}
	secondAtRel := strings.Index(raw[firstAt+1:], "@")
	if secondAtRel < 0 {
		return compositeSession{}, fmt.Errorf("invalid session ID")
	}
	secondAt := firstAt + 1 + secondAtRel
	lastAt := strings.LastIndex(raw, "@")
	if lastAt <= secondAt {
		return compositeSession{}, fmt.Errorf("invalid session ID")
	}
	out := compositeSession{
		Route:    raw[:firstAt],
		Profile:  raw[firstAt+1 : secondAt],
		Subject:  raw[secondAt+1 : lastAt],
		Backends: make(map[string]string),
	}
	backendPart := raw[lastAt+1:]
	if backendPart == "" {
		return out, nil
	}
	for _, entry := range strings.Split(backendPart, ",") {
		backend, encoded, ok := strings.Cut(entry, ":")
		if !ok || backend == "" {
			return compositeSession{}, fmt.Errorf("invalid backend session entry")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return compositeSession{}, fmt.Errorf("decode backend session %s: %w", backend, err)
		}
		out.Backends[backend] = string(decoded)
	}
	return out, nil
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
