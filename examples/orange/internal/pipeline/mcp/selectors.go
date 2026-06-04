package mcp

import (
	"fmt"
	"regexp"

	"github.com/dio/transit/examples/orange/internal/config"
)

type toolSelector struct {
	include      map[string]struct{}
	includeRegex []*regexp.Regexp
	exclude      map[string]struct{}
	excludeRegex []*regexp.Regexp
}

func compileToolSelector(routeName, backendName string, raw config.MCPToolSelector) (*toolSelector, error) {
	s := &toolSelector{
		include: make(map[string]struct{}, len(raw.Include)),
		exclude: make(map[string]struct{}, len(raw.Exclude)),
	}
	for _, tool := range raw.Include {
		s.include[tool] = struct{}{}
	}
	for _, tool := range raw.Exclude {
		s.exclude[tool] = struct{}{}
	}
	var err error
	s.includeRegex, err = compileToolRegexps(routeName, backendName, "include", raw.IncludeRegex)
	if err != nil {
		return nil, err
	}
	s.excludeRegex, err = compileToolRegexps(routeName, backendName, "exclude", raw.ExcludeRegex)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func compileToolRegexps(routeName, backendName, kind string, exprs []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(exprs))
	for _, expr := range exprs {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("compile %s tool selector regex %q for mcp route %q backend %q: %w", kind, expr, routeName, backendName, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (s *toolSelector) allows(tool string) bool {
	if s == nil {
		return true
	}
	if _, ok := s.exclude[tool]; ok {
		return false
	}
	for _, re := range s.excludeRegex {
		if re.MatchString(tool) {
			return false
		}
	}
	if len(s.include) == 0 && len(s.includeRegex) == 0 {
		return true
	}
	if _, ok := s.include[tool]; ok {
		return true
	}
	for _, re := range s.includeRegex {
		if re.MatchString(tool) {
			return true
		}
	}
	return false
}
