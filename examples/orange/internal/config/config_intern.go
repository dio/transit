package config

import (
	"fmt"
	"strings"
	"sync"
)

// InternPool maps strings to stable uint32 handles. It is append-only and
// never compacted, so handles remain valid across config reloads — user
// records can hold intern IDs indefinitely without risking stale references.
type InternPool struct {
	mu      sync.RWMutex
	strToID map[string]uint32
	idToStr []string
}

// NewInternPool returns an empty InternPool. The first interned string
// receives handle 0; handles increment from there.
func NewInternPool() *InternPool {
	return &InternPool{strToID: make(map[string]uint32)}
}

// Intern returns the stable uint32 handle for s, assigning one if s is new.
// Safe for concurrent use. Uses a read-lock fast path so repeated lookups of
// already-interned strings do not contend on the write lock.
func (p *InternPool) Intern(s string) uint32 {
	p.mu.RLock()
	if id, ok := p.strToID[s]; ok {
		p.mu.RUnlock()
		return id
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Re-check after acquiring write lock: another goroutine may have interned
	// s between the RUnlock and Lock above.
	if id, ok := p.strToID[s]; ok {
		return id
	}
	id := uint32(len(p.idToStr))
	p.strToID[s] = id
	p.idToStr = append(p.idToStr, s)
	return id
}

// Lookup returns the string for handle id, or "" if id is out of range.
func (p *InternPool) Lookup(id uint32) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if int(id) >= len(p.idToStr) {
		return ""
	}
	return p.idToStr[id]
}

// ParsedID holds the interned segments of a workspace/user/name record ID.
type ParsedID struct {
	Workspace uint32
	User      uint32
	Name      uint32
	Raw       string // original unsplit string, preserved for error messages
}

// parseID splits id into exactly three non-empty slash-separated segments and
// interns each one. It uses SplitN(id, "/", 4) so that a four-segment string
// (e.g. "a/b/c/d") yields four parts and is rejected; SplitN(..., 3) would
// silently fold the tail into the third segment and accept it.
func parseID(id string, interns *InternPool) (ParsedID, error) {
	parts := strings.SplitN(id, "/", 4)
	if len(parts) != 3 {
		return ParsedID{}, fmt.Errorf(
			"invalid id %q: want workspace/user/name, got %d segment(s)",
			id, len(parts),
		)
	}
	for i, part := range parts {
		if part == "" {
			return ParsedID{}, fmt.Errorf("invalid id %q: segment %d is empty", id, i)
		}
	}
	return ParsedID{
		Workspace: interns.Intern(parts[0]),
		User:      interns.Intern(parts[1]),
		Name:      interns.Intern(parts[2]),
		Raw:       id,
	}, nil
}
