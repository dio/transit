package llmdecider

import (
	"context"
	"fmt"

	"github.com/dio/switchyard-capi/go/jev"

	"github.com/dio/transit/up"
)

// judgeDecider serves the routing core's judge call over Envoy's own egress.
//
// It satisfies decision.Decider. The Decider is per request because the callout
// belongs to one stream; the Router is shared, because it holds the latch.
type judgeDecider struct {
	w      *up.Writer
	config Config
}

func (d judgeDecider) Call(ctx context.Context, request jev.DecisionRequest) (jev.DecisionResponse, error) {
	body, err := jev.EncodeRequest(request)
	if err != nil {
		return jev.DecisionResponse{}, fmt.Errorf("encoding judge request: %w", err)
	}

	// Writer.Do panics outside Go, and it blocks. The caller runs inside w.Go, so
	// the judge round trip never occupies the Envoy worker. Envoy makes the call:
	// the cluster owns TLS, the credentials and the retry policy.
	response, err := d.w.Do(ctx, up.HTTPCalloutRequest{
		Cluster: d.config.JudgeCluster,
		Headers: [][2]string{
			{":method", "POST"},
			{":path", d.config.JudgePath},
			{":scheme", "http"},
			{"host", d.config.JudgeHost},
			{"content-type", "application/json"},
		},
		Body:          body,
		TimeoutMillis: uint64(d.config.JudgeTimeout.Milliseconds()),
	})
	if err != nil {
		return jev.DecisionResponse{}, fmt.Errorf("judge callout: %w", err)
	}
	if response.Result != up.HTTPCalloutSuccess {
		return jev.DecisionResponse{}, fmt.Errorf("judge callout failed with result %d", response.Result)
	}

	// The reply is chunked, and the buffers are only valid during the callback, so
	// join the copies rather than holding the originals.
	var reply []byte
	for _, chunk := range response.Body {
		reply = append(reply, chunk.ToBytes()...)
	}

	// Decoding validates the reply against the request that produced it, so the
	// original request is carried through.
	decoded, err := jev.DecodeResponse(reply, request)
	if err != nil {
		return jev.DecisionResponse{}, fmt.Errorf("decoding judge response: %w", err)
	}
	return decoded, nil
}
