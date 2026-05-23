package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/internal/egtest"
)

func TestEnvoyGatewayInstallOnly(t *testing.T) {
	s := &envoyGatewayInstallSuite{}
	s.Cluster = "transit-cr-eg-install"
	s.Timeout = 7 * time.Minute
	egtest.RunEnvoyGatewayInstallSmokeTest(t, "RUN_CLUSTER_ROUTER_EG_INSTALL", s)
}

type envoyGatewayInstallSuite struct {
	envoyGatewaySuite
}

func (s *envoyGatewayInstallSuite) TestEnvoyGatewayInstallOnly() {
	s.verifyEnvoyGatewayInstall()
}

var _ suite.TestingSuite = (*envoyGatewayInstallSuite)(nil)
