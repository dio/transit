package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	"github.com/dio/transit/integrations/internal/egtest"
)

func TestEnvoyGatewayInstallOnly(t *testing.T) {
	s := &installSuite{}
	s.Cluster = "transit-tiered-ws-proxy-eg-install"
	s.Timeout = 7 * time.Minute
	egtest.RunEnvoyGatewayInstallSmokeTest(t, "RUN_TIERED_WS_PROXY_EG_INSTALL", s)
}

type installSuite struct {
	envoyGatewaySuite
}

func (s *installSuite) TestEnvoyGatewayInstallOnly() {
	s.verifyEnvoyGatewayInstall()
}

var _ suite.TestingSuite = (*installSuite)(nil)
