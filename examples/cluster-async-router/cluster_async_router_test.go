package clusterasyncrouter

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dio/transit/up"
)

func TestExtractTarget(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		v, err := ExtractTarget([]byte(`{"target":"a"}`))
		require.NoError(t, err)
		require.Equal(t, "a", v)
	})
	t.Run("missing", func(t *testing.T) {
		_, err := ExtractTarget([]byte(`{}`))
		require.Error(t, err)
	})
	t.Run("invalid json", func(t *testing.T) {
		_, err := ExtractTarget([]byte(`not-json`))
		require.Error(t, err)
	})
	t.Run("empty", func(t *testing.T) {
		_, err := ExtractTarget(nil)
		require.Error(t, err)
	})
}

func TestPendingResolveOnce(t *testing.T) {
	p := newPending()
	require.True(t, p.Resolve(Result{Upstream: "a"}))
	require.False(t, p.Resolve(Result{Upstream: "b"}))
	r, ok := p.Result()
	require.True(t, ok)
	require.Equal(t, "a", r.Upstream)
}

func TestPendingRegistry(t *testing.T) {
	p1 := Register("tok-1")
	require.NotNil(t, p1)
	require.Same(t, p1, Lookup("tok-1"))
	Delete("tok-1")
	require.Nil(t, Lookup("tok-1"))
}

func TestBodyHandlerKeepsPendingUntilHostSelectionConsumesIt(t *testing.T) {
	token := "tok-body-lifetime"
	p := Register(token)
	t.Cleanup(func() { Delete(token) })

	ctx := any(&streamState{token: token, p: p})
	bodyHandler(nil, &up.BodyChunk{
		Context:   &ctx,
		Data:      []byte(`{"target":"plain"}`),
		EndStream: true,
	})

	require.Same(t, p, Lookup(token))
	res, ok := p.Result()
	require.True(t, ok)
	require.Equal(t, "plain", res.Upstream)
}

func TestWaitAndCompleteDeletesPendingAfterResolve(t *testing.T) {
	token := "tok-complete-lifetime"
	p := Register(token)
	p.Resolve(Result{Err: "stop"})

	l := &lb{waiters: make(map[*up.ClusterLBCompletion]struct{})}
	l.waitAndComplete(token, p, nil)

	require.Nil(t, Lookup(token))
}
