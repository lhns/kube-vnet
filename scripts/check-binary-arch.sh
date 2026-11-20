#!/usr/bin/env bash
# Verify the /manager binary built for each platform has the matching CPU
# architecture. Guards against a Dockerfile that cross-compiles every platform
# for the same arch (arm64 images shipped x86-64 binaries up to v0.8.1).
#
# Usage: check-binary-arch.sh <dir> <platform>...   e.g. out linux/amd64 linux/arm64
# <dir> is the `--output type=local,dest=<dir>,platform-split=true` result:
# <dir>/linux_amd64/manager, <dir>/linux_arm64/manager. A single-platform
# build may come out flat (<dir>/manager); that is accepted for one platform.
set -euo pipefail

dir=${1:?usage: $0 <dir> <platform>...}
shift
[ $# -gt 0 ] || { echo "no platforms given" >&2; exit 2; }

fail=0
for platform in "$@"; do
  case "$platform" in
    linux/amd64) want=3e; name=x86-64 ;;
    linux/arm64) want=b7; name=aarch64 ;;
    *) echo "unsupported platform: $platform" >&2; exit 2 ;;
  esac
  bin="$dir/${platform//\//_}/manager"
  if [ ! -f "$bin" ] && [ $# -eq 1 ] && [ -f "$dir/manager" ]; then
    bin="$dir/manager"
  fi
  if [ ! -f "$bin" ]; then
    echo "FAIL $platform: binary not found at $bin" >&2
    fail=1
    continue
  fi
  # ELF e_machine: 2 bytes at offset 18 (little-endian for both targets).
  found=$(od -An -tx1 -j18 -N2 "$bin" | tr -d ' \n')
  if [ "$found" = "${want}00" ]; then
    echo "ok   $platform: $name ($bin)"
  else
    echo "FAIL $platform: expected e_machine 0x${want} ($name), found 0x${found:0:2}${found:2:2} in $bin" >&2
    fail=1
  fi
done
exit $fail
