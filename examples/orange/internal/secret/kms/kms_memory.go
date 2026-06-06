package kms

import (
	"context"
	"fmt"
	"sync"
)

// MemoryProvider holds a versioned map of MASTER_KEK bytes.
// All versions are retained so rotation-path tests can wrap old and new material.
type MemoryProvider struct {
	mu      sync.RWMutex
	current int
	keys    map[int][]byte
}

// NewMemoryProvider creates a MemoryProvider seeded with an initial 32-byte key at version 1.
func NewMemoryProvider(initialKey []byte) (*MemoryProvider, error) {
	if len(initialKey) != 32 {
		return nil, fmt.Errorf("kms/memory: MASTER_KEK must be 32 bytes, got %d", len(initialKey))
	}
	k := make([]byte, 32)
	copy(k, initialKey)
	return &MemoryProvider{current: 1, keys: map[int][]byte{1: k}}, nil
}

// AddVersion registers a new key version. Provider copies the bytes.
func (p *MemoryProvider) AddVersion(version int, key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("kms/memory: MASTER_KEK must be 32 bytes, got %d", len(key))
	}
	k := make([]byte, 32)
	copy(k, key)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[version] = k
	if version > p.current {
		p.current = version
	}
	return nil
}

// MasterKEK implements MasterKEKProvider.
func (p *MemoryProvider) MasterKEK(_ context.Context, version int) ([]byte, int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if version == 0 {
		version = p.current
	}
	key, ok := p.keys[version]
	if !ok {
		return nil, 0, fmt.Errorf("kms/memory: MASTER_KEK version %d not found", version)
	}
	out := make([]byte, 32)
	copy(out, key)
	return out, version, nil
}
