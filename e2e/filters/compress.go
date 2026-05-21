// e2e-compress is a buffered response-body filter that decodes a gzip-compressed
// upstream response and replaces it with the plain-text body. It also calls
// RequestIdentity to hint to the upstream that identity encoding is preferred,
// and strips the Content-Encoding response header so Go's http.Transport does
// not attempt a second decompress pass.
//
// Used by CodecSuite to verify the full RequestIdentity → Decode → SetResponseBody
// pipeline and the Content-Encoding stripping behaviour.
package filters

import (
	"github.com/dio/transit/up"
	"github.com/dio/transit/up/compress"
)

func init() {
	// nil request body handler: only the response body is of interest.
	up.RegisterWithMutableBody("e2e-compress", compressOnRequest, nil, compressOnResponse)
}

// compressOnRequest negotiates identity encoding so the upstream is asked not to
// compress. If the upstream ignores the hint (some CDNs always compress),
// compressOnResponse decodes the body regardless.
func compressOnRequest(w *up.Writer, _ *up.Request) {
	compress.RequestIdentity(w)
}

// compressOnResponse has two phases:
//
//  1. Headers phase (StatusCode != 0): remove Content-Encoding so the client
//     does not attempt to decompress an already-decoded body.  ContentEncoding
//     is captured into the filter before this handler is called, so the body
//     phase still has the original value to pass to compress.Decode.
//
//  2. Body phase (StatusCode == 0, EndStream == true): decode the buffered body
//     and replace it with the plain-text version.
func compressOnResponse(w *up.Writer, chunk *up.ResponseChunk) {
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
	decoded, err := compress.Decode(chunk.ContentEncoding, chunk.Data)
	if err != nil {
		w.Log(up.LogError, "e2e-compress: decode error: %v", err)
		return
	}
	w.SetResponseBody(decoded)
}
