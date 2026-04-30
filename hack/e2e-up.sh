#!/usr/bin/env bash
# e2e-up.sh — bootstrap a kind cluster with a chosen CNI and the kube-vnet operator.
#
# Usage:  ./hack/e2e-up.sh [kube-router|calico]   (default: kube-router)
#
# CI uses the helm/kind-action steps in .github/workflows/e2e.yaml directly,
# but the local-dev path mirrors that flow.
set -euo pipefail

CNI=${1:-${CNI:-kube-router}}
case "$CNI" in
  kube-router|calico) ;;
  *) echo "unknown CNI: $CNI (want: kube-router | calico)" >&2; exit 2 ;;
esac

CLUSTER_NAME=${CLUSTER_NAME:-kube-vnet-e2e-${CNI}}
IMG=${IMG:-kube-vnet:e2e}
CALICO_VERSION=${CALICO_VERSION:-v3.28.0}
KUBE_ROUTER_MANIFEST=${KUBE_ROUTER_MANIFEST:-https://raw.githubusercontent.com/cloudnativelabs/kube-router/master/daemonset/kubeadm-kuberouter.yaml}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$REPO_ROOT"

echo "==> creating kind cluster $CLUSTER_NAME (CNI=$CNI)"
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  kind create cluster --name "$CLUSTER_NAME" --config hack/kind-config.yaml
fi

case "$CNI" in
  kube-router)
    echo "==> installing kube-router"
    kubectl apply -f "$KUBE_ROUTER_MANIFEST"
    echo "==> waiting for kube-router DaemonSet"
    kubectl rollout status -n kube-system ds/kube-router --timeout=180s
    kubectl wait --for=condition=Ready node --all --timeout=180s
    ;;
  calico)
    echo "==> installing Calico (tigera operator $CALICO_VERSION)"
    kubectl apply --server-side -f "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/tigera-operator.yaml"
    kubectl rollout status -n tigera-operator deploy/tigera-operator --timeout=180s
    kubectl apply -f hack/calico-installation.yaml
    # calico-system pods take a moment to materialize after the Installation CR.
    for i in $(seq 1 60); do
      if kubectl -n calico-system get deployment calico-kube-controllers >/dev/null 2>&1; then
        break
      fi
      echo "waiting for calico-system to materialize ($i)"
      sleep 5
    done
    kubectl wait --for=condition=Ready pods --all -n calico-system --timeout=300s
    kubectl wait --for=condition=Ready node --all --timeout=180s
    ;;
esac

echo "==> building operator image $IMG"
docker build -t "$IMG" .

echo "==> loading image into kind"
kind load docker-image "$IMG" --name "$CLUSTER_NAME"

echo "==> deploying operator"
mv config/manager/manager.yaml config/manager/manager.yaml.bak
sed -e "s|ghcr.io/lhns/kube-vnet:latest|${IMG}|" \
    -e 's|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|' \
    config/manager/manager.yaml.bak > config/manager/manager.yaml
trap 'mv config/manager/manager.yaml.bak config/manager/manager.yaml 2>/dev/null || true' EXIT

kubectl apply -k config/default

echo "==> waiting for operator to be Available"
kubectl wait --for=condition=Available deploy/kube-vnet-controller \
  -n kube-vnet-system --timeout=180s

echo "==> ready"
kubectl get crd virtualnetworks.kube-vnet.lhns.de
kubectl get pods -n kube-vnet-system
