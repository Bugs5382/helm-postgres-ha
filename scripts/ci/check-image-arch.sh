#!/usr/bin/env bash
# check-image-arch.sh IMAGE PLATFORM...
#
# For each platform (linux/amd64, linux/arm64), copies pgha and wal-g out of
# IMAGE's variant for that platform and checks both are statically linked ELF
# executables for that CPU. Nothing is run, so no emulation is needed. The
# image must be in the local Docker store for each platform.
set -euo pipefail
IMAGE=${1:?image}; shift
[ $# -gt 0 ] || { echo "check-image-arch: no platform given" >&2; exit 2; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# ELF e_machine values (little-endian, offset 18).
machine_for() {
  case "$1" in
    linux/amd64) echo 3e00 ;;
    linux/arm64) echo b700 ;;
    *) echo "check-image-arch: unsupported platform $1" >&2; exit 2 ;;
  esac
}

fail=0
for platform in "$@"; do
  want=$(machine_for "$platform")
  cid=$(docker create --platform "$platform" "$IMAGE")
  for bin in pgha wal-g; do
    out="$work/${platform//\//-}-$bin"
    docker cp -q "$cid:/usr/local/bin/$bin" "$out"
    magic=$(od -An -tx1 -N4 "$out" | tr -d ' \n')
    got=$(od -An -tx1 -j18 -N2 "$out" | tr -d ' \n')
    interp=$(grep -c 'ld-linux' "$out" || true)
    if [ "$magic" != "7f454c46" ] || [ "$got" != "$want" ]; then
      echo "FAIL: $platform $bin is not a $platform ELF executable (machine $got, want $want)"; fail=1
    elif [ "$interp" != "0" ]; then
      echo "FAIL: $platform $bin is dynamically linked"; fail=1
    else
      echo "ok: $platform $bin"
    fi
  done
  docker rm -f "$cid" >/dev/null
done
exit $fail
