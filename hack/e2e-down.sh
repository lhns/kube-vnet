#!/usr/bin/env bash
set -euo pipefail
CLUSTER_NAME=${CLUSTER_NAME:-kube-vnet-e2e}
kind delete cluster --name "$CLUSTER_NAME"
