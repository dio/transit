package up

import (
	"encoding/binary"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// GRPCCalloutRequest carries parameters for an outbound gRPC unary callout.
// Message is the serialised proto body — no gRPC framing needed; GRPCCallout
// prepends the 5-byte length-prefix frame before sending.
type GRPCCalloutRequest struct {
	Cluster       string
	Authority     string // host/:authority header; defaults to Cluster if empty
	Method        string // full gRPC path, e.g. "/envoy.service.ratelimit.v3.RateLimitService/ShouldRateLimit"
	Message       []byte // raw proto bytes, unframed
	TimeoutMillis uint64
}

// GRPCCalloutResponse is delivered to GRPCCalloutFunc after the callout completes.
// Body is the unframed proto bytes (5-byte gRPC header stripped), ready to unmarshal.
// GRPCStatus and GRPCMessage are parsed from grpc-status / grpc-message response
// headers or trailers; successful responses may omit them, in which case status
// 0 is implied by Result==HTTPCalloutSuccess.
type GRPCCalloutResponse struct {
	Result      HTTPCalloutResult
	GRPCStatus  uint32 // grpc-status value; 0 = OK
	GRPCMessage string // grpc-message value
	Body        []byte // unframed proto bytes
}

// GRPCCalloutFunc is invoked when a gRPC callout completes.
// The callback runs on the Envoy worker thread under the same constraints as HTTPCalloutFunc.
type GRPCCalloutFunc func(GRPCCalloutResponse)

// GRPCCallout issues a gRPC unary callout over an Envoy HTTP/2 cluster, pausing
// the current request until the response arrives. fn is called with the decoded
// response; it may queue mutations or call SendLocalResponse.
//
// Internally uses Envoy's streamable HTTP callout API so response trailers are
// observed separately; gRPC status normally lives in trailers.
//
// Returns a non-nil error if Envoy rejected the callout; fn will not be called.
func (w *Writer) GRPCCallout(req GRPCCalloutRequest, fn GRPCCalloutFunc) (HTTPCalloutInitResult, error) {
	if fn == nil {
		panic("up: GRPCCallout called with nil callback")
	}
	if w.calloutStarted || w.goStarted {
		panic("up: GRPCCallout cannot be started after Go or another callout")
	}
	authority := req.Authority
	if authority == "" {
		authority = req.Cluster
	}

	cb := &grpcStreamCallback{
		f:  w.f,
		fn: fn,
		resp: GRPCCalloutResponse{
			Result: HTTPCalloutSuccess,
		},
	}
	w.calloutStarted = true
	w.f.calloutState.Store(calloutStateActive)
	init, _ := w.f.handle.StartHttpStream(
		req.Cluster,
		[][2]string{
			{":method", "POST"},
			{":path", req.Method},
			{":scheme", "http"},
			{"host", authority},
			{"content-type", "application/grpc+proto"},
			{"te", "trailers"},
		},
		encodeGRPCFrame(req.Message),
		true,
		req.TimeoutMillis,
		cb,
	)
	calloutInit := HTTPCalloutInitResult(init)
	if calloutInit != HTTPCalloutInitSuccess {
		w.calloutStarted = false
		w.f.calloutState.Store(calloutStateActive)
		return calloutInit, errCalloutInitResult(calloutInit)
	}
	return HTTPCalloutInitSuccess, nil
}

type grpcStreamCallback struct {
	f  *filter
	fn GRPCCalloutFunc

	mu   sync.Mutex
	once atomic.Bool
	resp GRPCCalloutResponse
	body []byte
}

func (c *grpcStreamCallback) OnHttpStreamHeaders(_ uint64, headers [][2]shared.UnsafeEnvoyBuffer, _ bool) {
	c.mu.Lock()
	c.applyGRPCHeaders(headers)
	c.mu.Unlock()
}

func (c *grpcStreamCallback) OnHttpStreamData(_ uint64, body []shared.UnsafeEnvoyBuffer, _ bool) {
	c.mu.Lock()
	for _, chunk := range body {
		c.body = append(c.body, chunk.ToBytes()...)
	}
	c.mu.Unlock()
}

func (c *grpcStreamCallback) OnHttpStreamTrailers(_ uint64, trailers [][2]shared.UnsafeEnvoyBuffer) {
	c.mu.Lock()
	c.applyGRPCHeaders(trailers)
	c.mu.Unlock()
}

func (c *grpcStreamCallback) OnHttpStreamComplete(_ uint64) {
	c.finish(HTTPCalloutSuccess)
}

func (c *grpcStreamCallback) OnHttpStreamReset(_ uint64, _ shared.HttpStreamResetReason) {
	c.finish(HTTPCalloutReset)
}

func (c *grpcStreamCallback) applyGRPCHeaders(headers [][2]shared.UnsafeEnvoyBuffer) {
	status, msg := parseGRPCHeaders(headers)
	c.resp.GRPCStatus = status
	if msg != "" {
		c.resp.GRPCMessage = msg
	}
}

func (c *grpcStreamCallback) finish(result HTTPCalloutResult) {
	if c.once.Swap(true) {
		return
	}
	c.mu.Lock()
	resp := c.resp
	resp.Result = result
	if result == HTTPCalloutSuccess {
		resp.Body = decodeGRPCFrame(c.body)
	}
	c.mu.Unlock()

	if c.f.streamDone.Load() {
		return
	}
	c.fn(resp)
	if c.f.calloutState.CompareAndSwap(calloutStatePaused, calloutStateFlushed) {
		c.f.flush(true)
		return
	}
	c.f.calloutState.CompareAndSwap(calloutStateActive, calloutStateDone)
}

// encodeGRPCFrame prepends the 5-byte gRPC length-prefix to msg:
// [compression-flag (0)] [message-length (4 bytes big-endian)] [message bytes].
func encodeGRPCFrame(msg []byte) []byte {
	frame := make([]byte, 5+len(msg))
	frame[0] = 0 // no compression
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(msg)))
	copy(frame[5:], msg)
	return frame
}

// decodeGRPCBody concatenates body chunks and strips the 5-byte gRPC frame prefix.
// Returns nil if the body is missing or too short to contain a valid frame.
func decodeGRPCBody(chunks []shared.UnsafeEnvoyBuffer) []byte {
	var raw []byte
	for _, chunk := range chunks {
		raw = append(raw, chunk.ToBytes()...)
	}
	return decodeGRPCFrame(raw)
}

func decodeGRPCFrame(raw []byte) []byte {
	if len(raw) < 5 {
		return nil
	}
	if raw[0] != 0 {
		return nil
	}
	msgLen := binary.BigEndian.Uint32(raw[1:5])
	end := 5 + int(msgLen)
	if end < 5 || end > len(raw) {
		return nil
	}
	return raw[5:end]
}

// parseGRPCHeaders extracts grpc-status and grpc-message from response headers
// or trailers. Successful responses may omit them; status 0 is implied by
// HTTPCalloutSuccess.
func parseGRPCHeaders(headers [][2]shared.UnsafeEnvoyBuffer) (status uint32, msg string) {
	for _, h := range headers {
		switch h[0].ToString() {
		case "grpc-status":
			if v, err := strconv.ParseUint(h[1].ToString(), 10, 32); err == nil {
				status = uint32(v)
			}
		case "grpc-message":
			msg = decodeGRPCMessage(h[1].ToString())
		}
	}
	return
}

func decodeGRPCMessage(msg string) string {
	decoded, err := url.QueryUnescape(msg)
	if err != nil {
		return msg
	}
	return decoded
}
