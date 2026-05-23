#!/usr/bin/env bash
# Install k3d, kubectl, and helm to /usr/local/bin.
# Versions are read from environment variables set in the workflow:
#   K3D_CLI_VERSION, KUBECTL_VERSION, HELM_VERSION
set -euxo pipefail

curl -fsSL -o /tmp/k3d "https://github.com/k3d-io/k3d/releases/download/${K3D_CLI_VERSION}/k3d-linux-amd64"
sudo install -m 0755 /tmp/k3d /usr/local/bin/k3d

curl -fsSL -o /tmp/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
sudo install -m 0755 /tmp/kubectl /usr/local/bin/kubectl

curl -fsSL -o /tmp/helm.tgz "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz"
tar -C /tmp -xzf /tmp/helm.tgz
sudo install -m 0755 /tmp/linux-amd64/helm /usr/local/bin/helm

k3d version
kubectl version --client=true
helm version
