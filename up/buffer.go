package up

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// Buffer is a borrowed buffer owned by Envoy.
//
// Copy the data with Bytes or String before retaining it beyond the current
// callback. UnsafeBytes and UnsafeString avoid the copy and are only valid while
// Envoy keeps the underlying memory alive.
type Buffer struct {
	raw shared.UnsafeEnvoyBuffer

	// Len is the number of bytes available in the buffer.
	Len uint64
}

func newBuffer(raw shared.UnsafeEnvoyBuffer) Buffer {
	return Buffer{raw: raw, Len: raw.Len}
}

// Bytes returns a Go-owned copy of the buffer bytes.
func (b Buffer) Bytes() []byte {
	return b.raw.ToBytes()
}

// String returns a Go-owned copy of the buffer as a string.
func (b Buffer) String() string {
	return b.raw.ToString()
}

// ToBytes returns a Go-owned copy of the buffer bytes.
func (b Buffer) ToBytes() []byte {
	return b.Bytes()
}

// ToString returns a Go-owned copy of the buffer as a string.
func (b Buffer) ToString() string {
	return b.String()
}

// UnsafeBytes returns a borrowed byte slice backed by Envoy memory.
func (b Buffer) UnsafeBytes() []byte {
	return b.raw.ToUnsafeBytes()
}

// UnsafeString returns a borrowed string backed by Envoy memory.
func (b Buffer) UnsafeString() string {
	return b.raw.ToUnsafeString()
}
