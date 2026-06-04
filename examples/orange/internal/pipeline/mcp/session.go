package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

const (
	capTools uint16 = 1 << iota
	capToolsListChanged
	capPrompts
	capPromptsListChanged
	capLogging
	capResources
	capResourcesListChanged
	capResourcesSubscribe
	capCompletions

	capAll = capTools | capToolsListChanged |
		capPrompts | capPromptsListChanged |
		capLogging |
		capResources | capResourcesListChanged | capResourcesSubscribe |
		capCompletions
)

type capabilities struct {
	Tools                bool `json:"tools,omitempty"`
	ToolsListChanged     bool `json:"tools_list_changed,omitempty"`
	Prompts              bool `json:"prompts,omitempty"`
	PromptsListChanged   bool `json:"prompts_list_changed,omitempty"`
	Logging              bool `json:"logging,omitempty"`
	Resources            bool `json:"resources,omitempty"`
	ResourcesListChanged bool `json:"resources_list_changed,omitempty"`
	ResourcesSubscribe   bool `json:"resources_subscribe,omitempty"`
	Completions          bool `json:"completions,omitempty"`
}

type backendSession struct {
	Backend      string       `json:"backend"`
	SessionID    string       `json:"session_id,omitempty"`
	Capabilities capabilities `json:"capabilities,omitempty"`
}

type sessionEnvelope struct {
	Route    string           `json:"route"`
	Subject  string           `json:"subject,omitempty"`
	Backends []backendSession `json:"backends"`
}

type eventEnvelope struct {
	Backend string `json:"backend"`
	EventID string `json:"event_id,omitempty"`
}

func (e sessionEnvelope) backend(name string) (backendSession, bool) {
	for _, backend := range e.Backends {
		if backend.Backend == name {
			return backend, true
		}
	}
	return backendSession{}, false
}

func (e sessionEnvelope) backendNames() []string {
	names := make([]string, 0, len(e.Backends))
	for _, backend := range e.Backends {
		names = append(names, backend.Backend)
	}
	sort.Strings(names)
	return names
}

func encodeSecureSessionID(c sessionCrypto, e sessionEnvelope) (string, error) {
	if e.Route == "" {
		return "", fmt.Errorf("session envelope route is empty")
	}
	if len(e.Backends) == 0 {
		return "", fmt.Errorf("session envelope has no backends")
	}
	for _, backend := range e.Backends {
		if backend.Backend == "" {
			return "", fmt.Errorf("session envelope has backend with empty name")
		}
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return c.Encrypt(raw)
}

func decodeSecureSessionID(c sessionCrypto, token, subject string) (sessionEnvelope, error) {
	raw, err := c.Decrypt(token)
	if err != nil {
		return sessionEnvelope{}, fmt.Errorf("decrypt session envelope: %w", err)
	}
	var e sessionEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return sessionEnvelope{}, fmt.Errorf("decode session envelope: %w", err)
	}
	if e.Subject != subject {
		return sessionEnvelope{}, fmt.Errorf("session subject mismatch")
	}
	if e.Route == "" {
		return sessionEnvelope{}, fmt.Errorf("session envelope route is empty")
	}
	if len(e.Backends) == 0 {
		return sessionEnvelope{}, fmt.Errorf("session envelope has no backends")
	}
	for _, backend := range e.Backends {
		if backend.Backend == "" {
			return sessionEnvelope{}, fmt.Errorf("session envelope has backend with empty name")
		}
	}
	return e, nil
}

func encodeSecureLastEventID(c sessionCrypto, e eventEnvelope) (string, error) {
	if e.Backend == "" {
		return "", fmt.Errorf("event envelope backend is empty")
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return c.Encrypt(raw)
}

func decodeSecureLastEventID(c sessionCrypto, token string) (eventEnvelope, error) {
	raw, err := c.Decrypt(token)
	if err != nil {
		return eventEnvelope{}, fmt.Errorf("decrypt event envelope: %w", err)
	}
	var e eventEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return eventEnvelope{}, fmt.Errorf("decode event envelope: %w", err)
	}
	if e.Backend == "" {
		return eventEnvelope{}, fmt.Errorf("event envelope backend is empty")
	}
	return e, nil
}

func encodeCapabilities(c capabilities) string {
	var bits uint16
	if c.Tools {
		bits |= capTools
	}
	if c.ToolsListChanged {
		bits |= capToolsListChanged
	}
	if c.Prompts {
		bits |= capPrompts
	}
	if c.PromptsListChanged {
		bits |= capPromptsListChanged
	}
	if c.Logging {
		bits |= capLogging
	}
	if c.Resources {
		bits |= capResources
	}
	if c.ResourcesListChanged {
		bits |= capResourcesListChanged
	}
	if c.ResourcesSubscribe {
		bits |= capResourcesSubscribe
	}
	if c.Completions {
		bits |= capCompletions
	}
	return fmt.Sprintf("%03x", bits)
}

func decodeCapabilities(hex string) capabilities {
	bits64, err := strconv.ParseUint(hex, 16, 16)
	if err != nil || hex == "" {
		bits64 = uint64(capAll)
	}
	bits := uint16(bits64)
	return capabilities{
		Tools:                bits&capTools != 0,
		ToolsListChanged:     bits&capToolsListChanged != 0,
		Prompts:              bits&capPrompts != 0,
		PromptsListChanged:   bits&capPromptsListChanged != 0,
		Logging:              bits&capLogging != 0,
		Resources:            bits&capResources != 0,
		ResourcesListChanged: bits&capResourcesListChanged != 0,
		ResourcesSubscribe:   bits&capResourcesSubscribe != 0,
		Completions:          bits&capCompletions != 0,
	}
}
