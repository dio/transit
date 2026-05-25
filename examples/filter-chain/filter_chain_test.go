package filterchain_test

import (
	"testing"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/fake"
	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared/mocks"
	"go.uber.org/mock/gomock"

	filterchain "github.com/dio/transit/examples/filter-chain"
	"github.com/dio/transit/up"
)

type mockHandle struct{ *mocks.MockHttpFilterHandle }

func (h *mockHandle) GetAttributeNumber(_ shared.AttributeID) (float64, bool) { return 0, false }

func newMockHandle(ctrl *gomock.Controller) *mockHandle {
	return &mockHandle{mocks.NewMockHttpFilterHandle(ctrl)}
}

// TestWithRequiredHeader_absent verifies that a missing x-api-key results in a 401.
func TestWithRequiredHeader_absent(t *testing.T) {
	ctrl := gomock.NewController(t)

	handle := newMockHandle(ctrl)
	handle.EXPECT().
		SendLocalResponse(uint32(401), gomock.Any(), gomock.Any(), gomock.Any()).
		Times(1)

	nextCalled := false
	nextFn := up.HandlerFunc(func(_ *up.Writer, _ *up.Request) { nextCalled = true })

	filterchain.WithRequiredHeader("x-api-key")(nextFn)(up.NewWriter(handle), &up.Request{})

	if nextCalled {
		t.Fatal("next should not be called when header is absent")
	}
}

// TestWithRequiredHeader_present verifies that a present x-api-key lets the request through.
func TestWithRequiredHeader_present(t *testing.T) {
	ctrl := gomock.NewController(t)

	handle := newMockHandle(ctrl)
	// SendLocalResponse must NOT be called.

	nextCalled := false
	nextFn := up.HandlerFunc(func(_ *up.Writer, _ *up.Request) { nextCalled = true })

	headers := fake.NewFakeHeaderMap(map[string][]string{
		"x-api-key": {"secret"},
	})
	req := up.NewRequest(headers, "filter-chain")

	filterchain.WithRequiredHeader("x-api-key")(nextFn)(up.NewWriter(handle), req)

	if !nextCalled {
		t.Fatal("next should be called when header is present")
	}
}

// TestWithStampHeader_setsAfterNext verifies that SetRequestHeader is called after next runs.
func TestWithStampHeader_setsAfterNext(t *testing.T) {
	ctrl := gomock.NewController(t)

	handle := newMockHandle(ctrl)
	mockHeaderMap := mocks.NewMockHeaderMap(ctrl)

	var order []string

	handle.EXPECT().RequestHeaders().Return(mockHeaderMap).Times(1)
	mockHeaderMap.EXPECT().Set("x-stamp", "ok").DoAndReturn(func(_, _ string) {
		order = append(order, "set")
	}).Times(1)

	nextFn := up.HandlerFunc(func(_ *up.Writer, _ *up.Request) {
		order = append(order, "next")
	})

	filterchain.WithStampHeader("x-stamp", "ok")(nextFn)(up.NewWriter(handle), &up.Request{})

	if len(order) != 2 || order[0] != "next" || order[1] != "set" {
		t.Fatalf("want [next set], got %v", order)
	}
}

// TestWithLogging_logsRequest verifies that the logging middleware logs at Info level.
func TestWithLogging_logsRequest(t *testing.T) {
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
		}).Times(1)

	noopFn := up.HandlerFunc(func(_ *up.Writer, _ *up.Request) {})

	filterchain.WithLogging()(noopFn)(up.NewWriter(handle), &up.Request{Method: "GET", Path: "/test"})

	if gotLevel != shared.LogLevelInfo {
		t.Fatalf("want LogLevelInfo, got %v", gotLevel)
	}
	if gotFormat != "filter-chain: %s %s" {
		t.Fatalf("want format 'filter-chain: %%s %%s', got %q", gotFormat)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "GET" || gotArgs[1] != "/test" {
		t.Fatalf("want args [GET /test], got %v", gotArgs)
	}
}
