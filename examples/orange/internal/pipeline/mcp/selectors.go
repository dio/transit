package mcp

import (
	"github.com/dio/transit/examples/orange/internal/config"
)

type toolSelector struct {
	include map[string]struct{}
}

// compileToolSelector builds a toolSelector from a ToolFilter. The ToolFilter
// carries only an explicit Include allowlist; an empty list means allow-all.
func compileToolSelector(_ /*routeName*/, _ /*backendName*/ string, raw config.ToolFilter) (*toolSelector, error) {
	s := &toolSelector{
		include: make(map[string]struct{}, len(raw.Include)),
	}
	for _, tool := range raw.Include {
		s.include[tool] = struct{}{}
	}
	return s, nil
}

func (s *toolSelector) allows(tool string) bool {
	if s == nil {
		return true
	}
	if len(s.include) == 0 {
		return true
	}
	_, ok := s.include[tool]
	return ok
}
