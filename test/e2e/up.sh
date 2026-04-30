#!/usr/bin/env bash
# test/e2e/up.sh — bootstrap a kind cluster with a chosen CNI and the kube-vnet operator.
#
# Usage:  ./test/e2e/up.sh [kube-router|calico]   (default: kube-router)
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

# Resolve the script's own directory and the repo root.
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT"

echo "==> creating kind cluster $CLUSTER_NAME (CNI=$CNI)"
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
  kind create cluster --name "$CLUSTER_NAME" --config "$SCRIPT_DIR/kind-config.yaml"
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
    kubectl apply -f "$SCRIPT_DIR/calico-installation.yaml"
    # calico-system + its workloads materialize after the Installation CR is
    # reconciled. Wait for both the deployment and the daemonset to exist.
    for i in $(seq 1 60); do
      if kubectl -n calico-system get deployment calico-kube-controllers >/dev/null 2>&1 \
         && kubectl -n calico-system get ds csi-node-driver >/dev/null 2>&1; then
        break
      fi
      echo "waiting for calico-system to materialize ($i)"
      sleep 5
    done
    # Workload-level readiness, not pod-level. `kubectl wait pods --all` races
    # against the operator's rolling pod replacements during startup; rollout
    # status watches the workload's generation/replica counts and is immune.
    kubectl rollout status -n calico-system deploy/calico-kube-controllers --timeout=300s
    kubectl rollout status -n calico-system deploy/calico-typha            --timeout=300s
    kubectl rollout status -n calico-system ds/calico-node                 --timeout=300s
    kubectl rollout status -n calico-system ds/csi-node-driver             --timeout=300s
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
