package e2e

import (
	"context"
	"testing"

	"github.com/dio/transit/integrations/internal/egtest"
)

// tiered-ws-proxy-eg uses the default EG install shape: controller in
// envoy-gateway-system, data plane in default. No Gateway Namespace mode.

type envoyGatewaySuite struct {
	egtest.Suite
}

func (s *envoyGatewaySuite) SetupSuite() {
	s.Name = "tiered-ws-proxy-eg"
	s.Cluster = "transit-tiered-ws-proxy-eg"
	s.SetupSuiteBase(s.InstallEnvoyGateway)
}

func (s *envoyGatewaySuite) verifyEnvoyGatewayInstall() {
	s.VerifyEnvoyGatewayInstall("XDSNameSchemeV2")
}

func apply(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	egtest.Apply(ctx, t, path)
}

func renderApply(ctx context.Context, t *testing.T, path string, data any) {
	t.Helper()
	egtest.RenderApply(ctx, t, path, data)
}

func waitDeployment(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	egtest.WaitDeployment(ctx, t, namespace, name)
}

func waitEPP(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	egtest.WaitEnvoyPatchPolicyProgrammed(ctx, t, "default", name)
}

func portForward(ctx context.Context, t *testing.T, namespace, target string, port int) (string, func()) {
	t.Helper()
	return egtest.PortForwardIn(ctx, t, namespace, target, port)
}

func run(ctx context.Context, t *testing.T, stdin, name string, args ...string) {
	t.Helper()
	egtest.Run(ctx, t, stdin, name, args...)
}

func liveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	egtest.LiveLogf(t, format, args...)
}
