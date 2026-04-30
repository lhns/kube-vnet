#!/usr/bin/env bash
# Tear down the e2e kind cluster. Pass the CNI name to match e2e-up.sh, or
# omit it to delete the kube-router cluster.
set -euo pipefail
CNI=${1:-${CNI:-kube-router}}
CLUSTER_NAME=${CLUSTER_NAME:-kube-vnet-e2e-${CNI}}
kind delete cluster --name "$CLUSTER_NAME"
