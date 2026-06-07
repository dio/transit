// Package kms defines the MasterKEKProvider interface for MASTER_KEK retrieval.
package kms

import "context"

// LatestVersion is the sentinel passed to MasterKEK when the caller wants
// the current active version without knowing the version number.
const LatestVersion = 0

// MasterKEKProvider retrieves raw MASTER_KEK bytes. The key material must
// be exactly 32 bytes. Implementations must never store the bytes in
// plaintext beyond the lifetime of the returned slice.
type MasterKEKProvider interface {
	// MasterKEK returns the raw 32-byte MASTER_KEK for the given version.
	// Pass LatestVersion (0) to get the current active version.
	MasterKEK(ctx context.Context, version int) (key []byte, version_ int, err error)
}
