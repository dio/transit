package kms

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
)

// EnvVarProvider reads a single MASTER_KEK from an environment variable.
// The variable must hold a base64url-encoded 32-byte key (no padding).
// Always reports version 1.
type EnvVarProvider struct {
	key []byte
}

// FromEnv constructs an EnvVarProvider by reading varName from the environment.
func FromEnv(varName string) (*EnvVarProvider, error) {
	raw := os.Getenv(varName)
	if raw == "" {
		return nil, fmt.Errorf("kms/envvar: %s is not set", varName)
	}
	key, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("kms/envvar: %s is not valid base64url: %w", varName, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("kms/envvar: %s must decode to 32 bytes, got %d", varName, len(key))
	}
	k := make([]byte, 32)
	copy(k, key)
	return &EnvVarProvider{key: k}, nil
}

// MasterKEK implements MasterKEKProvider. version is ignored; always returns version 1.
func (p *EnvVarProvider) MasterKEK(_ context.Context, _ int) ([]byte, int, error) {
	out := make([]byte, 32)
	copy(out, p.key)
	return out, 1, nil
}
