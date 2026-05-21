package hello_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/mocks"

	"github.com/dio/transit/examples/hello"
	"github.com/dio/transit/up"
)

func TestHandler_logsMethodAndPath(t *testing.T) {
	ctrl := gomock.NewController(t)

	var gotLevel shared.LogLevel
	var gotFormat string
	var gotArgs []any

	handle := mocks.NewMockHttpFilterHandle(ctrl)
	handle.EXPECT().
		Log(shared.LogLevelWarn, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(level shared.LogLevel, format string, args ...any) {
			gotLevel = level
			gotFormat = format
			gotArgs = args
		})

	hello.Handler(up.NewWriter(handle), &up.Request{Method: "GET", Path: "/test"})

	require.Equal(t, shared.LogLevelWarn, gotLevel)
	require.Equal(t, "hello: %s %s", gotFormat)
	require.Equal(t, []any{"GET", "/test"}, gotArgs)
}
