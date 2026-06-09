package filters

import (
	"fmt"

	"github.com/dio/transit/e2e/internal/grpctestproto"
	"github.com/dio/transit/up"
)

func init() {
	up.Register("e2e-grpc-callout", grpcCalloutBodyHeaders, up.WithMutableBody(grpcCalloutBodyHandler))
}

func grpcCalloutBodyHeaders(_ *up.Writer, r *up.Request) {
	*r.Context = r.Path
}

func grpcCalloutBodyHandler(w *up.Writer, chunk *up.BodyChunk) {
	if !chunk.EndStream {
		return
	}
	req := grpctestproto.EchoRequest{
		Text:     string(chunk.Data),
		Sequence: uint32(len(chunk.Data)),
	}
	initResult, err := w.GRPCCallout(up.GRPCCalloutRequest{
		Cluster:       "grpc-callout-upstream",
		Method:        "/e2e.Echo/Echo",
		Message:       req.MarshalProto(nil),
		TimeoutMillis: 1000,
	}, func(resp up.GRPCCalloutResponse) {
		if resp.Result != up.HTTPCalloutSuccess {
			w.SendLocalResponse(503, []byte(fmt.Sprintf("grpc callout failed result=%d", resp.Result)), [2]string{"content-type", "text/plain"})
			return
		}
		if resp.GRPCStatus != 0 {
			w.SendLocalResponse(502, []byte(fmt.Sprintf("grpc error %d: %s", resp.GRPCStatus, resp.GRPCMessage)),
				[2]string{"content-type", "text/plain"})
			return
		}
		var echoResp grpctestproto.EchoResponse
		if err := echoResp.UnmarshalProto(resp.Body); err != nil {
			w.SendLocalResponse(502, []byte(fmt.Sprintf("grpc decode error: %s", err.Error())), [2]string{"content-type", "text/plain"})
			return
		}
		w.SendLocalResponse(200, []byte(echoResp.Text),
			[2]string{"content-type", "text/plain"},
			[2]string{"x-grpc-status", "0"},
			[2]string{"x-grpc-sequence", fmt.Sprintf("%d", echoResp.Sequence)},
		)
	})
	if err != nil {
		w.SendLocalResponse(503, []byte(fmt.Sprintf("grpc init=%d err=%s", initResult, err.Error())), [2]string{"content-type", "text/plain"})
	}
}
