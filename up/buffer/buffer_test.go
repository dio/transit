package buffer_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up/buffer"
)

// ── Ring ─────────────────────────────────────────────────────────────────────

func TestRing_Bytes_empty(t *testing.T) {
	rb := buffer.NewRing(8)
	require.Empty(t, rb.Bytes())
}

func TestRing_Len_empty(t *testing.T) {
	rb := buffer.NewRing(8)
	require.Equal(t, 0, rb.Len())
}

func TestRing_Write_lessThanCapacity(t *testing.T) {
	rb := buffer.NewRing(8)
	rb.Write([]byte("abc"))
	require.Equal(t, []byte("abc"), rb.Bytes())
	require.Equal(t, 3, rb.Len())
}

func TestRing_Write_exactCapacity(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcd"))
	require.Equal(t, 4, rb.Len()) // check before Bytes() — Bytes() linearises in-place
	require.Equal(t, []byte("abcd"), rb.Bytes())
}

func TestRing_Write_overflow_retainsLastN(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcde"))     // 5 bytes into a 4-byte ring
	require.Equal(t, 4, rb.Len()) // check before Bytes()
	require.Equal(t, []byte("bcde"), rb.Bytes())
}

func TestRing_Write_doubleCapacity_retainsLastN(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcdefgh")) // 2× capacity
	require.Equal(t, []byte("efgh"), rb.Bytes())
}

func TestRing_Write_multipleCallsWithOverflow(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abc"))
	rb.Write([]byte("de")) // total "abcde", ring holds last 4 = "bcde"
	require.Equal(t, []byte("bcde"), rb.Bytes())
}

func TestRing_Write_singleByteCapacity(t *testing.T) {
	rb := buffer.NewRing(1)
	rb.Write([]byte("hello"))
	require.Equal(t, []byte("o"), rb.Bytes()) // only last byte
}

func TestRing_Bytes_linearisesInPlace_thenWriteContinues(t *testing.T) {
	// Bytes() linearises a full ring in-place (pos=0, full=false after the call).
	// Subsequent writes must continue from that clean state.
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcde"))
	require.Equal(t, []byte("bcde"), rb.Bytes())
	// Ring is now in clean state (pos=0, full=false). Writing more must work.
	rb.Write([]byte("fg"))
	require.Equal(t, []byte("fg"), rb.Bytes())
}

func TestRing_Reset_clearsContent(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcd"))
	rb.Reset()
	require.Empty(t, rb.Bytes())
	require.Equal(t, 0, rb.Len())
}

func TestRing_Reset_allowsReuse(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcd"))
	rb.Reset()
	rb.Write([]byte("xy"))
	require.Equal(t, []byte("xy"), rb.Bytes())
}

func TestRing_Len_partialFill(t *testing.T) {
	rb := buffer.NewRing(8)
	rb.Write([]byte("abc"))
	require.Equal(t, 3, rb.Len())
}

func TestRing_Len_full(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte("abcde")) // overflow
	require.Equal(t, 4, rb.Len())
}

func TestRing_Write_emptySlice(t *testing.T) {
	rb := buffer.NewRing(4)
	rb.Write([]byte{})
	require.Empty(t, rb.Bytes())
	require.Equal(t, 0, rb.Len())
}

func TestRing_Write_appendsAcrossMultipleCalls(t *testing.T) {
	rb := buffer.NewRing(6)
	rb.Write([]byte("ab"))
	rb.Write([]byte("cd"))
	rb.Write([]byte("ef"))
	require.Equal(t, []byte("abcdef"), rb.Bytes())
}

// ── HeadTail ─────────────────────────────────────────────────────────────────

func TestHeadTail_Head_empty(t *testing.T) {
	ht := buffer.NewHeadTail(4, 4)
	require.Empty(t, ht.Head())
	require.Empty(t, ht.Tail())
}

func TestHeadTail_Write_shorterThanHead(t *testing.T) {
	ht := buffer.NewHeadTail(8, 8)
	ht.Write([]byte("abc"))
	require.Equal(t, []byte("abc"), ht.Head())
	require.Equal(t, []byte("abc"), ht.Tail())
}

func TestHeadTail_Write_exactlyHead(t *testing.T) {
	ht := buffer.NewHeadTail(4, 8)
	ht.Write([]byte("abcd"))
	require.Equal(t, []byte("abcd"), ht.Head())
	require.Equal(t, []byte("abcd"), ht.Tail())
}

func TestHeadTail_Write_longerThanHead_headIsCapped(t *testing.T) {
	ht := buffer.NewHeadTail(4, 8)
	ht.Write([]byte("abcdefgh"))
	// Head captures only the first 4 bytes.
	require.Equal(t, []byte("abcd"), ht.Head())
}

func TestHeadTail_Write_longerThanHead_tailHasLast(t *testing.T) {
	ht := buffer.NewHeadTail(4, 4)
	ht.Write([]byte("abcdefgh")) // 8 bytes; tail ring size 4 → last 4
	require.Equal(t, []byte("efgh"), ht.Tail())
}

func TestHeadTail_Write_multipleChunks_headCaptures(t *testing.T) {
	ht := buffer.NewHeadTail(4, 8)
	ht.Write([]byte("ab"))
	ht.Write([]byte("cd"))
	ht.Write([]byte("ef")) // head full after "abcd"
	require.Equal(t, []byte("abcd"), ht.Head())
	require.Equal(t, []byte("abcdef"), ht.Tail())
}

func TestHeadTail_Write_tailOverflows(t *testing.T) {
	ht := buffer.NewHeadTail(2, 3)
	ht.Write([]byte("abcde")) // tail ring size 3 → keeps last 3 = "cde"
	require.Equal(t, []byte("ab"), ht.Head())
	require.Equal(t, []byte("cde"), ht.Tail())
}

func TestHeadTail_Reset_clearsHeadAndTail(t *testing.T) {
	ht := buffer.NewHeadTail(4, 4)
	ht.Write([]byte("abcdefgh"))
	ht.Reset()
	require.Empty(t, ht.Head())
	require.Empty(t, ht.Tail())
}

func TestHeadTail_Reset_allowsReuse(t *testing.T) {
	ht := buffer.NewHeadTail(4, 4)
	ht.Write([]byte("abcdefgh"))
	ht.Reset()
	ht.Write([]byte("xy"))
	require.Equal(t, []byte("xy"), ht.Head())
	require.Equal(t, []byte("xy"), ht.Tail())
}

func TestHeadTail_Write_headAndTailOverlap_shortStream(t *testing.T) {
	// Stream shorter than head — Head and Tail return the same content.
	ht := buffer.NewHeadTail(8, 8)
	ht.Write([]byte("hi"))
	require.Equal(t, ht.Head(), ht.Tail())
}

func TestHeadTail_Write_tailLargerThanStream(t *testing.T) {
	// Tail ring bigger than the stream — Tail returns everything.
	ht := buffer.NewHeadTail(2, 16)
	ht.Write([]byte("hello world"))
	require.Equal(t, []byte("he"), ht.Head()) // head capped at 2
	require.Equal(t, []byte("hello world"), ht.Tail())
}
