#!/usr/bin/env bash
# Render the chart's CRD templates from the controller-gen output.
#
# The chart ships each CRD as a *templated* resource (not under the special
# `crds/` directory) so `helm upgrade` propagates schema changes; see the
# header comment each generated file carries. That means the chart holds a
# second copy of every CRD, which used to be hand-maintained -- and silently
# drifted from `config/crd/bases/` whenever the API changed. A stale copy is
# not a cosmetic problem: it made `helm install` fail outright when the chart's
# own seeded ClusterVirtualNetworkBaseline stopped setting a field the stale
# CRD still marked required.
#
# The chart copy is exactly the generated CRD plus:
#   1. the Helm header comment (why it's templated, not in crds/), and
#   2. a `helm.sh/resource-policy: keep` annotation, so `helm uninstall`
#      doesn't cascade-delete every existing CR along with the CRD.
#
# The annotation is inserted textually, right after controller-gen's own
# annotation, so controller-gen's exact formatting (block scalars, key order)
# is preserved byte-for-byte and the diff stays reviewable.
#
# Run after `make manifests` (which invokes this). CI regenerates and
# `git diff --exit-code`s, so the copies can never drift again.
set -euo pipefail

cd "$(dirname "$0")/.."

python3 - "config/crd/bases" "charts/kube-vnet/templates" <<'PY'
import glob, os, sys

crd_dir, out_dir = sys.argv[1], sys.argv[2]

HEADER = """{{- /*
CRD shipped as a templated resource (not under `charts/kube-vnet/crds/`) so
`helm upgrade` updates it on chart bumps. Helm's special crds/ directory is
install-once-only and does not propagate updates -- see
https://helm.sh/docs/chart_best_practices/custom_resource_definitions/.
The `helm.sh/resource-policy: keep` annotation prevents `helm uninstall`
from cascading-deleting every existing CR alongside the CRD. ADR 0030.

Generated from config/crd/bases/ by `make manifests`
(scripts/gen-chart-crds.sh). Do not edit by hand -- edit the API types under
api/v1alpha1/ and regenerate. A stale copy breaks `helm install`.
*/ -}}
"""

ANNOTATION = "helm.sh/resource-policy: keep"

for src in sorted(glob.glob(os.path.join(crd_dir, "*.yaml"))):
    plural = os.path.basename(src).split("_", 1)[1]          # ...lhns.de_virtualnetworks.yaml
    out = os.path.join(out_dir, f"crd-{plural}")

    lines = open(src, encoding="utf-8").read().splitlines()
    body, inserted = [], False
    for ln in lines:
        body.append(ln)
        # controller-gen always emits its version annotation first under
        # metadata.annotations; hang the Helm policy off the same block.
        if not inserted and ln.strip().startswith("controller-gen.kubebuilder.io/version:"):
            indent = " " * (len(ln) - len(ln.lstrip()))
            body.append(f"{indent}{ANNOTATION}")
            inserted = True
    if not inserted:
        sys.exit(f"{src}: no controller-gen annotation found; cannot place {ANNOTATION}")

    with open(out, "w", encoding="utf-8", newline="\n") as fh:
        fh.write(HEADER)
        fh.write("\n".join(body) + "\n")
    print(f"  wrote {out}")
PY
