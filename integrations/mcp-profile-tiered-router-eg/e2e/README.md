# MCP Profile Tiered Router E2E

This directory is intentionally empty for the skeleton phase.

The first real suite should follow the existing Envoy Gateway integration
pattern:

- create an isolated k3d cluster
- install Envoy Gateway with EnvoyPatchPolicy enabled
- apply the L1/L2 manifests under `../k8s`
- exercise the README test matrix through the L1 Gateway
