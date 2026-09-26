#!/usr/bin/env python3
"""Generate the SPDX 2.3 SBOM of the kube-vnet Helm chart.

syft sees nothing inside a Helm chart tarball, so the release workflow builds
the chart SBOM here instead. The document describes:

  - the chart package itself (name, version, SHA-256 of the tgz, and its OCI
    location as a purl; SPDX's downloadLocation cannot hold an oci:// URL, so
    it is the release asset URL when there is one),
  - every container image the chart can deploy, found by rendering the chart
    twice: with default values, and with every optional feature switched on.
    An image in the default render is a DEPENDS_ON of the chart; one that only
    the all-features render uses is an OPTIONAL_DEPENDENCY_OF it.
  - for images that have an SBOM of their own (our operator image), an
    externalDocumentRef to that SBOM and a DESCRIBED_BY relationship into it.

The chart does not contain the images (CONTAINS would be wrong) and does not
build them (GENERATES would be wrong); it references them and cannot work
without them, which is what DEPENDS_ON means.

Only the Python standard library is used. Output is deterministic for a given
set of inputs: pass --created (the workflow uses its build date) to pin the
timestamp.

Usage (see .github/workflows/release.yaml):

  scripts/chart-sbom.py --chart kube-vnet-0.8.0.tgz \\
      --chart-ref oci://ghcr.io/lhns/charts/kube-vnet \\
      --chart-digest sha256:... \\
      --digest ghcr.io/lhns/kube-vnet:v0.8.0=sha256:... \\
      --image-sbom ghcr.io/lhns/kube-vnet:v0.8.0=kube-vnet-image.sbom.spdx.json \\
      --resolve-digests --created 2026-09-25T12:00:00Z \\
      -o kube-vnet-chart.sbom.spdx.json
"""

import argparse
import hashlib
import json
import re
import shutil
import subprocess
import sys
import tarfile
from datetime import datetime, timezone
from urllib.parse import quote

# Values the chart has no default for; rendering fails without them. Their
# choice does not change which images are deployed.
REQUIRED_VALUES = ["operator.clusterBaseline.ingressIsolationLevel=namespace"]

# Every optional feature that can add a workload or change an image. Keep in
# sync with charts/kube-vnet/values.yaml when a feature is added.
ALL_FEATURES = [
    "webhook.enabled=true",
    "webhook.networkWait.enabled=true",
    "cleanup.enabled=true",
    "podMonitor.enabled=true",
    "metricsService.enabled=true",
    "dnsCarveout.enabled=true",
]

IMAGE_RE = re.compile(r'^\s*(?:-\s+)?image:\s*["\']?([^"\'\s]+)["\']?\s*$')
# Images passed to the operator as flags (the network-wait init container the
# webhook injects into application pods).
IMAGE_FLAG_RE = re.compile(r'--[a-z-]*image=["\']?([^"\'\s]+)')
KIND_RE = re.compile(r"^kind:\s*(\S+)", re.M)
NAME_RE = re.compile(r"^  name:\s*(\S+)", re.M)


def sha(path, algo):
    h = hashlib.new(algo)
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 16), b""):
            h.update(chunk)
    return h.hexdigest()


def chart_meta(tgz):
    """name, version and declared license from Chart.yaml inside the tgz."""
    with tarfile.open(tgz) as tf:
        member = next(m for m in tf.getmembers() if re.fullmatch(r"[^/]+/Chart\.yaml", m.name))
        text = tf.extractfile(member).read().decode()

    def field(pattern):
        m = re.search(pattern, text, re.M)
        return m.group(1).strip().strip("\"'") if m else None

    return {
        "name": field(r"^name:\s*(.+)$"),
        "version": field(r"^version:\s*(.+)$"),
        "appVersion": field(r"^appVersion:\s*(.+)$"),
        "license": field(r"^\s+artifacthub\.io/license:\s*(.+)$"),
    }


def render(helm, tgz, extra):
    cmd = [helm, "template", "kube-vnet", tgz,
           "--namespace", "kube-vnet-system",
           "--kube-version", "1.31.0",
           "--api-versions", "monitoring.coreos.com/v1"]
    for v in REQUIRED_VALUES + extra:
        cmd += ["--set", v]
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout


def images_in(rendered):
    """{image ref: sorted list of "Kind/name" that reference it}."""
    found = {}
    for doc in re.split(r"^---\s*$", rendered, flags=re.M):
        kind = KIND_RE.search(doc)
        name = NAME_RE.search(doc)
        where = f"{kind.group(1)}/{name.group(1)}" if kind and name else "unknown"
        for line in doc.splitlines():
            if line.lstrip().startswith("#"):
                continue
            m = IMAGE_RE.match(line)
            if m:
                found.setdefault(m.group(1), set()).add(where)
            for m in IMAGE_FLAG_RE.finditer(line):
                found.setdefault(m.group(1), set()).add(f"{where} (--{line.split('--', 1)[1].split('=')[0]})")
    return {k: sorted(v) for k, v in found.items()}


def split_ref(ref):
    """ghcr.io/lhns/kube-vnet:v1 -> (ghcr.io/lhns/kube-vnet, v1, None)."""
    digest = None
    if "@" in ref:
        ref, digest = ref.split("@", 1)
    repo, tag = ref, None
    if ":" in ref.rsplit("/", 1)[-1]:
        repo, tag = ref.rsplit(":", 1)
    return repo, tag, digest


def resolve_digest(ref):
    """Manifest (index) digest of ref via crane or docker buildx; None if neither works."""
    attempts = []
    if shutil.which("crane"):
        attempts.append(["crane", "digest", ref])
    if shutil.which("docker"):
        attempts.append(["docker", "buildx", "imagetools", "inspect", ref,
                         "--format", "{{json .Manifest}}"])
    for cmd in attempts:
        try:
            out = subprocess.run(cmd, check=True, capture_output=True, text=True, timeout=120).stdout.strip()
        except (subprocess.SubprocessError, OSError):
            continue
        if out.startswith("{"):
            out = json.loads(out).get("digest", "")
        if re.fullmatch(r"sha256:[0-9a-f]{64}", out):
            return out
    return None


def spdx_id(prefix, value):
    return f"SPDXRef-{prefix}-" + re.sub(r"[^A-Za-z0-9.-]+", "-", value).strip("-")


def purl_oci(repo, digest, tag):
    name = repo.rsplit("/", 1)[-1]
    purl = f"pkg:oci/{name}"
    if digest:
        purl += "@" + quote(digest, safe="")
    qualifiers = [f"repository_url={repo}"]
    if tag:
        qualifiers.append(f"tag={tag}")
    return purl + "?" + "&".join(qualifiers)


def kv(pairs, what):
    out = {}
    for p in pairs:
        if "=" not in p:
            sys.exit(f"--{what} expects REF=VALUE, got {p!r}")
        k, v = p.split("=", 1)
        out[k] = v
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--chart", required=True, help="chart tarball from `helm package`")
    ap.add_argument("--chart-ref", required=True, help="OCI location, e.g. oci://ghcr.io/lhns/charts/kube-vnet")
    ap.add_argument("--chart-digest", help="manifest digest `helm push` reported")
    ap.add_argument("--download-location", help="https URL of the tgz (the release asset); default NOASSERTION")
    ap.add_argument("--digest", action="append", default=[], metavar="REF=DIGEST",
                    help="known digest of an image the chart deploys (repeatable)")
    ap.add_argument("--image-sbom", action="append", default=[], metavar="REF=PATH",
                    help="SPDX JSON SBOM of an image; linked via externalDocumentRefs (repeatable)")
    ap.add_argument("--resolve-digests", action="store_true",
                    help="look up digests of the remaining images with crane or docker buildx")
    ap.add_argument("--created", help="creation timestamp (RFC 3339, UTC); default now")
    ap.add_argument("--helm", default="helm")
    ap.add_argument("-o", "--output", required=True)
    args = ap.parse_args()

    known_digests = kv(args.digest, "digest")
    image_sboms = kv(args.image_sbom, "image-sbom")

    meta = chart_meta(args.chart)
    chart_sha256 = sha(args.chart, "sha256")
    default_images = images_in(render(args.helm, args.chart, []))
    all_images = images_in(render(args.helm, args.chart, ALL_FEATURES))
    for ref in default_images:
        all_images.setdefault(ref, default_images[ref])
    for ref in list(known_digests) + list(image_sboms):
        if ref not in all_images:
            sys.exit(f"{ref} is not deployed by the chart; rendered images: {sorted(all_images)}")

    created = args.created or datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    chart_id = spdx_id("Package-chart", meta["name"])
    chart_pkg = {
        "SPDXID": chart_id,
        "name": meta["name"],
        "versionInfo": meta["version"],
        "packageFileName": args.chart.replace("\\", "/").rsplit("/", 1)[-1],
        # SPDX only accepts URLs and VCS locations here, not oci://; the OCI
        # reference is carried by the purl below.
        "downloadLocation": args.download_location or "NOASSERTION",
        "filesAnalyzed": False,
        "checksums": [{"algorithm": "SHA256", "checksumValue": chart_sha256}],
        "licenseConcluded": "NOASSERTION",
        "licenseDeclared": meta["license"] or "NOASSERTION",
        "copyrightText": "NOASSERTION",
        "primaryPackagePurpose": "APPLICATION",
        "comment": f"Helm chart, appVersion {meta['appVersion']}. Install with "
                   f"`helm install kube-vnet {args.chart_ref} --version {meta['version']}`.",
    }
    chart_pkg["externalRefs"] = [{
        "referenceCategory": "PACKAGE-MANAGER",
        "referenceType": "purl",
        "referenceLocator": purl_oci(args.chart_ref.removeprefix("oci://"), args.chart_digest, meta["version"]),
    }]

    packages = [chart_pkg]
    relationships = [{
        "spdxElementId": "SPDXRef-DOCUMENT",
        "relationshipType": "DESCRIBES",
        "relatedSpdxElement": chart_id,
    }]
    ext_refs = []

    for ref in sorted(all_images):
        repo, tag, digest = split_ref(ref)
        digest = digest or known_digests.get(ref)
        if not digest and args.resolve_digests:
            digest = resolve_digest(ref)
        optional = ref not in default_images
        img_id = spdx_id("Package-image", ref)
        comment = ("Deployed only when optional features are enabled" if optional
                   else "Deployed with default values") + "; referenced by " + ", ".join(all_images[ref]) + "."
        if not digest:
            comment += " Digest not resolved at build time; the tag is mutable."
        pkg = {
            "SPDXID": img_id,
            "name": repo,
            "versionInfo": tag or digest or "NOASSERTION",
            "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": "NOASSERTION",
            "copyrightText": "NOASSERTION",
            "primaryPackagePurpose": "CONTAINER",
            "externalRefs": [{
                "referenceCategory": "PACKAGE-MANAGER",
                "referenceType": "purl",
                "referenceLocator": purl_oci(repo, digest, tag),
            }],
            "comment": comment,
        }
        if digest:
            algo, value = digest.split(":", 1)
            pkg["checksums"] = [{"algorithm": algo.upper(), "checksumValue": value}]
        packages.append(pkg)
        if optional:
            relationships.append({"spdxElementId": img_id, "relationshipType": "OPTIONAL_DEPENDENCY_OF",
                                  "relatedSpdxElement": chart_id})
        else:
            relationships.append({"spdxElementId": chart_id, "relationshipType": "DEPENDS_ON",
                                  "relatedSpdxElement": img_id})

        if ref in image_sboms:
            path = image_sboms[ref]
            with open(path, encoding="utf-8") as f:
                ext_ns = json.load(f)["documentNamespace"]
            doc_ref = "DocumentRef-" + re.sub(r"[^A-Za-z0-9.-]+", "-", repo.rsplit("/", 1)[-1]) + "-image"
            ext_refs.append({
                "externalDocumentId": doc_ref,
                "spdxDocument": ext_ns,
                # SPDX 2.3 section 6.6: the checksum of an external document is SHA1.
                "checksum": {"algorithm": "SHA1", "checksumValue": sha(path, "sha1")},
            })
            relationships.append({"spdxElementId": img_id, "relationshipType": "DESCRIBED_BY",
                                  "relatedSpdxElement": f"{doc_ref}:SPDXRef-DOCUMENT"})
            pkg["comment"] += f" Contents: {path.replace(chr(92), '/').rsplit('/', 1)[-1]} ({doc_ref})."

    doc = {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"{meta['name']}-chart-{meta['version']}",
        "documentNamespace": f"https://github.com/lhns/kube-vnet/spdx/{meta['name']}-chart-{meta['version']}-{chart_sha256}",
        "creationInfo": {
            "created": created,
            "creators": ["Organization: lhns", "Tool: kube-vnet-scripts/chart-sbom.py"],
        },
        "packages": packages,
        "relationships": relationships,
    }
    if ext_refs:
        doc["externalDocumentRefs"] = ext_refs

    with open(args.output, "w", encoding="utf-8", newline="\n") as f:
        json.dump(doc, f, indent=2)
        f.write("\n")


if __name__ == "__main__":
    main()
