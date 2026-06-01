package e2e

import (
	"context"
	"testing"

	"github.com/dio/transit/integrations/internal/egtest"
)

// cluster-async-router-eg uses the default Envoy Gateway install shape:
// controller in envoy-gateway-system, data plane in default. No Gateway
// Namespace mode.

type envoyGatewaySuite struct {
	egtest.Suite
}

func (s *envoyGatewaySuite) SetupSuite() {
	s.Name = "cluster-async-router-eg"
	s.Cluster = "transit-cluster-async-router-eg"
	s.SetupSuiteBase(s.InstallEnvoyGateway)
}

func (s *envoyGatewaySuite) verifyEnvoyGatewayInstall() {
	s.VerifyEnvoyGatewayInstall("XDSNameSchemeV2")
}

// ── lowercase shims so e2e_test.go reads cleanly ─────────────────────────────

func apply(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	egtest.Apply(ctx, t, path)
}

func renderApply(ctx context.Context, t *testing.T, path string, data any) {
	t.Helper()
	egtest.RenderApply(ctx, t, path, data)
}

func waitReady(ctx context.Context, t *testing.T, label string) {
	t.Helper()
	egtest.WaitReady(ctx, t, "default", label)
}

func waitDeployment(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	egtest.WaitDeployment(ctx, t, namespace, name)
}

func waitEnvoyPatchPolicyProgrammed(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	egtest.WaitEnvoyPatchPolicyProgrammed(ctx, t, "default", name)
}

func portForward(ctx context.Context, t *testing.T, namespace, target string, remotePort int) (string, func()) {
	t.Helper()
	return egtest.PortForwardIn(ctx, t, namespace, target, remotePort)
}

func run(ctx context.Context, t *testing.T, stdin, name string, args ...string) {
	t.Helper()
	egtest.Run(ctx, t, stdin, name, args...)
}

func liveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	egtest.LiveLogf(t, format, args...)
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	return egtest.RequireEnv(t, name)
}
