// Package buffer provides zero-allocation stream buffering for Envoy filters.
//
// The head+tail pattern targets streaming responses (SSE, NDJSON, chunked JSON)
// where relevant data appears at both ends: e.g. LLM token usage counts have
// input tokens near the START and output tokens near the END. HeadTail captures
// both regions without retaining the full stream.
package buffer

import "slices"

// Ring is a fixed-size circular buffer that retains the LAST n bytes written.
// When full, new writes overwrite the oldest data. Zero-allocation after
// construction. Not goroutine-safe: intended for single-threaded filter use.
type Ring struct {
	data []byte
	size int
	pos  int
	full bool
}

// NewRing creates a Ring with the given capacity in bytes.
func NewRing(size int) *Ring {
	return &Ring{data: make([]byte, size), size: size}
}

// Write appends p to the ring, overwriting the oldest data when full.
func (rb *Ring) Write(p []byte) {
	for len(p) > 0 {
		n := copy(rb.data[rb.pos:], p)
		rb.pos += n
		p = p[n:]
		if rb.pos >= rb.size {
			rb.pos = 0
			rb.full = true
		}
	}
}

// Bytes returns the buffered content in chronological order (oldest first).
// Linearises the ring in-place via three-reversal rotation: O(n) time, O(1) space.
// After Bytes() the ring is in a clean linearised state; subsequent Write calls
// continue normally.
func (rb *Ring) Bytes() []byte {
	if !rb.full {
		return rb.data[:rb.pos]
	}
	slices.Reverse(rb.data[:rb.pos])
	slices.Reverse(rb.data[rb.pos:rb.size])
	slices.Reverse(rb.data[:rb.size])
	rb.pos = 0
	rb.full = false
	return rb.data[:rb.size]
}

// Reset clears the ring without releasing memory.
func (rb *Ring) Reset() {
	rb.pos = 0
	rb.full = false
}

// Len returns the number of bytes currently stored.
func (rb *Ring) Len() int {
	if rb.full {
		return rb.size
	}
	return rb.pos
}

// HeadTail captures the first headSize bytes and the last tailSize bytes of a
// stream without retaining the middle.
//
// Input token counts appear near the START of an LLM SSE stream (Anthropic
// message_start, OpenAI first usage chunk); output counts appear near the END
// (message_delta, final usage chunk). HeadTail covers both with bounded memory.
//
// Typical usage in a response observer:
//
//	ht := buffer.NewHeadTail(8*1024, 64*1024)
//
//	// Per chunk:
//	ht.Write(chunk.Data)
//
//	// On EndStream:
//	input, output := extractTokens(ht.Head(), ht.Tail())
type HeadTail struct {
	head     []byte
	headN    int
	headSize int
	tail     *Ring
}

// NewHeadTail creates a HeadTail buffer with separate head and tail capacities.
func NewHeadTail(headSize, tailSize int) *HeadTail {
	return &HeadTail{
		head:     make([]byte, headSize),
		headSize: headSize,
		tail:     NewRing(tailSize),
	}
}

// Write feeds p into both the head slab and tail ring.
// Once the head slab is full, further bytes go to the tail only.
func (ht *HeadTail) Write(p []byte) {
	if ht.headN < ht.headSize {
		n := copy(ht.head[ht.headN:], p)
		ht.headN += n
	}
	ht.tail.Write(p)
}

// Head returns the first headSize bytes received, or fewer if the stream was
// shorter.
func (ht *HeadTail) Head() []byte {
	return ht.head[:ht.headN]
}

// Tail returns the last tailSize bytes received, linearised in chronological
// order. Calls Ring.Bytes() which linearises in-place.
func (ht *HeadTail) Tail() []byte {
	return ht.tail.Bytes()
}

// Reset clears both head and tail without releasing memory. Safe to reuse from
// a pool between requests.
func (ht *HeadTail) Reset() {
	ht.headN = 0
	ht.tail.Reset()
}
