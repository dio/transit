package grpctestproto

import (
	"fmt"

	"github.com/VictoriaMetrics/easyproto"
)

// EchoRequest is the proto payload sent by the e2e GRPCCallout filter.
//
//	message EchoRequest {
//	  string text = 1;
//	  uint32 sequence = 2;
//	}
type EchoRequest struct {
	Text     string
	Sequence uint32
}

func (r EchoRequest) MarshalProto(dst []byte) []byte {
	var m easyproto.Marshaler
	mm := m.MessageMarshaler()
	mm.AppendString(1, r.Text)
	mm.AppendUint32(2, r.Sequence)
	return m.Marshal(dst)
}

func (r *EchoRequest) UnmarshalProto(src []byte) error {
	*r = EchoRequest{}
	var fc easyproto.FieldContext
	for len(src) > 0 {
		var err error
		src, err = fc.NextField(src)
		if err != nil {
			return fmt.Errorf("cannot read EchoRequest field: %w", err)
		}
		switch fc.FieldNum {
		case 1:
			text, ok := fc.String()
			if !ok {
				return fmt.Errorf("cannot read EchoRequest.text")
			}
			r.Text = text
		case 2:
			sequence, ok := fc.Uint32()
			if !ok {
				return fmt.Errorf("cannot read EchoRequest.sequence")
			}
			r.Sequence = sequence
		}
	}
	return nil
}

// EchoResponse is the proto payload returned by the e2e gRPC upstream.
//
//	message EchoResponse {
//	  string text = 1;
//	  uint32 sequence = 2;
//	}
type EchoResponse struct {
	Text     string
	Sequence uint32
}

func (r EchoResponse) MarshalProto(dst []byte) []byte {
	var m easyproto.Marshaler
	mm := m.MessageMarshaler()
	mm.AppendString(1, r.Text)
	mm.AppendUint32(2, r.Sequence)
	return m.Marshal(dst)
}

func (r *EchoResponse) UnmarshalProto(src []byte) error {
	*r = EchoResponse{}
	var fc easyproto.FieldContext
	for len(src) > 0 {
		var err error
		src, err = fc.NextField(src)
		if err != nil {
			return fmt.Errorf("cannot read EchoResponse field: %w", err)
		}
		switch fc.FieldNum {
		case 1:
			text, ok := fc.String()
			if !ok {
				return fmt.Errorf("cannot read EchoResponse.text")
			}
			r.Text = text
		case 2:
			sequence, ok := fc.Uint32()
			if !ok {
				return fmt.Errorf("cannot read EchoResponse.sequence")
			}
			r.Sequence = sequence
		}
	}
	return nil
}
