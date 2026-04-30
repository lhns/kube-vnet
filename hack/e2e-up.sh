#!/usr/bin/env bash
# e2e-up.sh — bootstrap a kind cluster with Calico CNI and the kube-vnet operator.
#
# This is the local-dev entry point. CI uses the helm/kind-action steps in
# .github/workflows/e2e.yaml directly, but mirrors the behavior here.
set -euo pipefail

CLUSTER_NAME=${CLUSTER_NAME:-kube-vnet-e2e}
IMG=${IMG:-kube-vnet:e2e}
CALICO_VERSION=${CALICO_VERSION:-v3.28.0}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO_ROOT"

echo "==> creating kind cluster $CLUSTER_NAME"
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  kind create cluster --name "$CLUSTER_NAME" --config hack/kind-config.yaml
fi

echo "==> installing Calico (tigera operator $CALICO_VERSION)"
kubectl apply --server-side -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/tigera-operator.yaml"
kubectl apply -f hack/calico-installation.yaml

echo "==> waiting for Calico to be ready"
kubectl rollout status -n tigera-operator deploy/tigera-operator --timeout=180s
kubectl wait --for=condition=Ready pods --all -n calico-system --timeout=300s
kubectl wait --for=condition=Ready node --all --timeout=180s

echo "==> building operator image $IMG"
docker build -t "$IMG" .

echo "==> loading image into kind"
kind load docker-image "$IMG" --name "$CLUSTER_NAME"

echo "==> deploying operator"
# Patch the manager Deployment to use the locally-built image with imagePullPolicy=Never.
TMP=$(mktemp)
sed -e "s|ghcr.io/lhns/kube-vnet:latest|${IMG}|" \
    -e 's|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|' \
    config/manager/manager.yaml > "$TMP"
mv "$TMP" config/manager/manager.yaml.local
trap 'mv config/manager/manager.yaml.local config/manager/manager.yaml.local.bak 2>/dev/null || true' EXIT
mv config/manager/manager.yaml config/manager/manager.yaml.bak
mv config/manager/manager.yaml.local config/manager/manager.yaml

kubectl apply -k config/default

# Restore original manifest.
mv config/manager/manager.yaml.bak config/manager/manager.yaml

echo "==> waiting for operator to be Available"
kubectl wait --for=condition=Available deploy/kube-vnet-controller \
  -n kube-vnet-system --timeout=180s

echo "==> ready"
kubectl get crd virtualnetworks.kube-vnet.lhns.de
kubectl get pods -n kube-vnet-system
