#!/usr/bin/env bash
# Verify each platform's /manager has the matching CPU architecture. Guards
# against cross-compiling every platform for one arch (arm64 images shipped
# x86-64 binaries up to v0.8.1).
#
# Usage: check-binary-arch.sh <dir> <platform>...
# <dir> is a `--output type=local,dest=<dir>,platform-split=true` result,
# holding <dir>/linux_amd64/manager, <dir>/linux_arm64/manager.
set -euo pipefail

dir=${1:?usage: $0 <dir> <platform>...}
shift
[ $# -gt 0 ] || { echo "no platforms given" >&2; exit 2; }

fail=0
for platform in "$@"; do
  case "$platform" in
    linux/amd64) want=3e00 name=x86-64 ;;
    linux/arm64) want=b700 name=aarch64 ;;
    *) echo "unsupported platform: $platform" >&2; exit 2 ;;
  esac
  bin="$dir/${platform//\//_}/manager"
  # ELF e_machine: 2 bytes at offset 18, little-endian for both targets.
  found=$(od -An -tx1 -j18 -N2 "$bin" 2>/dev/null | tr -d ' \n') || found=missing
  if [ "$found" = "$want" ]; then
    echo "ok   $platform: $name ($bin)"
  else
    echo "FAIL $platform: want $name (e_machine $want), found ${found:-missing} in $bin" >&2
    fail=1
  fi
done
exit $fail
