package bodytransform_test

import (
	"encoding/json"
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/mocks"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	bodytransform "github.com/dio/transit/examples/body-transform"
	"github.com/dio/transit/up"
)

type mockHandle struct{ *mocks.MockHttpFilterHandle }

func (h *mockHandle) GetAttributeNumber(_ shared.AttributeID) (float64, bool) { return 0, false }

func newMockHandle(ctrl *gomock.Controller) *mockHandle {
	return &mockHandle{mocks.NewMockHttpFilterHandle(ctrl)}
}

// TestOnReq_logsMethodAndPath verifies the request handler logs at Info level.
func TestOnReq_logsMethodAndPath(t *testing.T) {
	ctrl := gomock.NewController(t)

	var gotLevel shared.LogLevel
	var gotFormat string
	var gotArgs []any

	handle := newMockHandle(ctrl)
	handle.EXPECT().
		Log(shared.LogLevelInfo, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(level shared.LogLevel, format string, args ...any) {
			gotLevel = level
			gotFormat = format
			gotArgs = args
		})

	bodytransform.OnReq(up.NewWriter(handle), &up.Request{Method: "POST", Path: "/foo"})

	require.Equal(t, shared.LogLevelInfo, gotLevel)
	require.Equal(t, "body-transform: %s %s", gotFormat)
	require.Equal(t, []any{"POST", "/foo"}, gotArgs)
}

// TestOnBody_renamesMessageToText checks that {"message":"hello"} becomes {"text":"hello"}.
func TestOnBody_renamesMessageToText(t *testing.T) {
	out, ok := bodytransform.TransformBody([]byte(`{"message":"hello"}`))
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal(out, &m))
	require.Equal(t, "hello", m["text"])
	_, hasMessage := m["message"]
	require.False(t, hasMessage)
}

// TestOnBody_noMessageField_unchanged checks that bodies without "message" are not transformed.
func TestOnBody_noMessageField_unchanged(t *testing.T) {
	_, ok := bodytransform.TransformBody([]byte(`{"other":"val"}`))
	require.False(t, ok)
}

// TestOnBody_nonJSON_unchanged checks that non-JSON bodies are not transformed.
func TestOnBody_nonJSON_unchanged(t *testing.T) {
	_, ok := bodytransform.TransformBody([]byte(`plaintext`))
	require.False(t, ok)
}
