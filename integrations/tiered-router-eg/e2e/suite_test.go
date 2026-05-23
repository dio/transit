package e2e

import (
	"context"
	"testing"

	"github.com/dio/transit/integrations/internal/egtest"
)

const (
	systemNamespace    = "transit-system"
	dataplaneNamespace = "transit-dataplane"
)

type envoyGatewaySuite struct {
	egtest.Suite
}

func (s *envoyGatewaySuite) SetupSuite() {
	s.Name = "tiered-router-eg"
	s.Cluster = "transit-tiered-router-eg"
	s.SystemNamespace = systemNamespace
	s.DataplaneNamespace = dataplaneNamespace
	s.SetupSuiteBase(s.InstallEnvoyGateway)
}

func (s *envoyGatewaySuite) verifyEnvoyGatewayInstall() {
	s.VerifyEnvoyGatewayInstall("GatewayNamespace", "XDSNameSchemeV2")
}

// ── lowercase shims so e2e_test.go call sites are unchanged ──────────────────

func apply(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	egtest.Apply(ctx, t, path)
}

func renderApply(ctx context.Context, t *testing.T, path string, data any) {
	t.Helper()
	egtest.RenderApply(ctx, t, path, data)
}

func waitReady(ctx context.Context, t *testing.T, namespace, label string) {
	t.Helper()
	egtest.WaitReady(ctx, t, namespace, label)
}

func waitDeployment(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	egtest.WaitDeployment(ctx, t, namespace, name)
}

func waitEnvoyPatchPolicyProgrammed(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	egtest.WaitEnvoyPatchPolicyProgrammed(ctx, t, namespace, name)
}

func generatedResourceName(ctx context.Context, t *testing.T, namespace, gateway, kind string) string {
	t.Helper()
	return egtest.GeneratedResourceName(ctx, t, namespace, gateway, kind)
}

func portForward(ctx context.Context, t *testing.T, target string, remotePort int) (string, func()) {
	t.Helper()
	return egtest.PortForwardIn(ctx, t, dataplaneNamespace, target, remotePort)
}

func eventually(ctx context.Context, t *testing.T, check func() error) {
	t.Helper()
	egtest.Eventually(ctx, t, check)
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
