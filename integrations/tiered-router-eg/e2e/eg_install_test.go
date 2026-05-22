package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

func TestEnvoyGatewayInstallOnly(t *testing.T) {
	if os.Getenv("RUN_TIERED_ROUTER_EG_INSTALL") != "1" {
		t.Skip("set RUN_TIERED_ROUTER_EG_INSTALL=1 to run the Envoy Gateway install smoke test")
	}
	suite.Run(t, &envoyGatewayInstallSuite{
		envoyGatewaySuite: envoyGatewaySuite{
			cluster: "transit-tiered-eg-install",
			timeout: 7 * time.Minute,
		},
	})
}

type envoyGatewayInstallSuite struct {
	envoyGatewaySuite
}

func (s *envoyGatewayInstallSuite) TestEnvoyGatewayInstallOnly() {
	s.verifyEnvoyGatewayInstall()
}
