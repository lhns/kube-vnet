#!/usr/bin/env bash
# Generate standalone JSON Schemas from the kube-vnet CRDs so downstream
# projects can validate kube-vnet custom resources with kubeconform.
#
# Output layout matches the datreeio/CRDs-catalog convention (the de-facto
# kubeconform standard), so a consumer points kubeconform at:
#
#   -schema-location 'https://raw.githubusercontent.com/lhns/kube-vnet/<ref>/schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
#
# producing files at:  schemas/<group>/<kind-lowercased>_<version>.json
#
# Source of truth is config/crd/bases/*.yaml (controller-gen output). Run
# `make manifests` first if the API types changed, then `make schemas`.
# CI regenerates and `git diff --exit-code`s to catch drift.
#
# Requires: python3 with PyYAML (already used by the project's tooling).
set -euo pipefail

cd "$(dirname "$0")/.."

CRD_DIR="config/crd/bases"
OUT_DIR="schemas"

python3 - "$CRD_DIR" "$OUT_DIR" <<'PY'
import glob, json, os, sys

import yaml

crd_dir, out_dir = sys.argv[1], sys.argv[2]


def convert(node):
    """Turn a CRD openAPIV3Schema subtree into a plain JSON Schema:

    - Drop OpenAPI x-kubernetes-* keys — they're not JSON Schema keywords
      (list/map merge hints, CEL validations) and kubeconform can't use them.
      (kube-vnet CRD schemas are structural with no nullable / int-or-string,
      so no further keyword translation is needed.)
    - Inject `additionalProperties: false` on closed object structs so
      kubeconform catches misspelled field names, faithfully mirroring the
      apiserver's structural-schema pruning. Only where it's correct: an
      object that declares `properties`, doesn't already set
      `additionalProperties` (i.e. isn't a map like matchLabels), and isn't
      marked x-kubernetes-preserve-unknown-fields (checked BEFORE the strip).
      Objects without `properties` (e.g. metadata) stay open.
    """
    if isinstance(node, dict):
        preserve = node.get("x-kubernetes-preserve-unknown-fields") is True
        out = {
            k: convert(v)
            for k, v in node.items()
            if not k.startswith("x-kubernetes-")
        }
        if "properties" in out and "additionalProperties" not in out and not preserve:
            out["additionalProperties"] = False
        return out
    if isinstance(node, list):
        return [convert(v) for v in node]
    return node


count = 0
for path in sorted(glob.glob(os.path.join(crd_dir, "*.yaml"))):
    with open(path, encoding="utf-8") as fh:
        crd = yaml.safe_load(fh)
    group = crd["spec"]["group"]
    kind = crd["spec"]["names"]["kind"]
    group_dir = os.path.join(out_dir, group)
    os.makedirs(group_dir, exist_ok=True)
    for version in crd["spec"]["versions"]:
        vname = version["name"]
        schema = convert(version["schema"]["openAPIV3Schema"])
        schema["$schema"] = "http://json-schema.org/schema#"
        fname = f"{kind.lower()}_{vname}.json"
        out = os.path.join(group_dir, fname)
        # sort_keys → deterministic output for the CI drift-gate.
        with open(out, "w", encoding="utf-8", newline="\n") as fh:
            json.dump(schema, fh, indent=2, sort_keys=True)
            fh.write("\n")
        print(f"  {group}/{fname}")
        count += 1

print(f"generated {count} schema(s) under {out_dir}/")
PY
