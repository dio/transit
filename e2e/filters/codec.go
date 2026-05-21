package filters

import (
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/codec"
)

func init() {
	// nil request body handler: only the response body is of interest.
	up.RegisterWithMutableBody("e2e-codec", codecOnRequest, nil, codecOnResponse)
}

// codecOnRequest negotiates identity encoding so the upstream is asked not to
// compress. If the upstream ignores the hint (some CDNs always compress),
// codecOnResponse decodes the body regardless.
func codecOnRequest(w *up.Writer, _ *up.Request) {
	codec.NegotiateIdentity(w)
}

// codecOnResponse has two phases:
//
//  1. Headers phase (StatusCode != 0): remove Content-Encoding so the client
//     does not attempt to decompress an already-decoded body.  ContentEncoding
//     is captured into the filter before this handler is called, so the body
//     phase still has the original value to pass to codec.Decode.
//
//  2. Body phase (StatusCode == 0, EndStream == true): decode the buffered body
//     and replace it with the plain-text version.
func codecOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
	if chunk.StatusCode != 0 {
		// headers phase: remove encoding header before it reaches the client
		if chunk.ContentEncoding != "" && chunk.ContentEncoding != "identity" {
			w.RemoveResponseHeader("content-encoding")
		}
		return
	}
	if !chunk.EndStream || len(chunk.Data) == 0 {
		return
	}
	decoded, err := codec.Decode(chunk.ContentEncoding, chunk.Data)
	if err != nil {
		w.Log(up.LogError, "e2e-codec: decode error: %v", err)
		return
	}
	w.SetResponseBody(decoded)
}
