// Package vtprotocodec provides a Connect codec that uses vtproto's
// MarshalVT/UnmarshalVT fast-path when available, falling back to the
// standard google.golang.org/protobuf encoding.
package vtprotocodec

import "google.golang.org/protobuf/proto"

type vtMarshaler interface{ MarshalVT() ([]byte, error) }
type vtUnmarshaler interface{ UnmarshalVT([]byte) error }

// Codec implements connect.Codec with the wire name "proto".
type Codec struct{}

func (Codec) Name() string { return "proto" }

func (Codec) Marshal(msg any) ([]byte, error) {
	if m, ok := msg.(vtMarshaler); ok {
		return m.MarshalVT()
	}
	return proto.Marshal(msg.(proto.Message))
}

func (Codec) Unmarshal(data []byte, msg any) error {
	if m, ok := msg.(vtUnmarshaler); ok {
		return m.UnmarshalVT(data)
	}
	return proto.Unmarshal(data, msg.(proto.Message))
}
