#!/usr/bin/env bash
# Build the reproducible Ubuntu 24.04 LTS base image on an LXD host and
# publish it locally as mea-base-24.04. Prints the fingerprint to pin in
# config/profiles.json ("image": {"fingerprint": "..."}).
#
# Run on the LXD host (or with an lxc remote): ./build-base-image.sh [source-alias]
set -euo pipefail

SRC=${1:-ubuntu:24.04}
ALIAS=${MEA_BASE_ALIAS:-mea-base-24.04}
BUILD=mea-base-build-$$
DIR=$(cd "$(dirname "$0")" && pwd)

cleanup() { lxc delete --force "$BUILD" >/dev/null 2>&1 || true; }
trap cleanup EXIT

lxc launch "$SRC" "$BUILD" -c security.privileged=false -c security.nesting=false \
  -c cloud-init.user-data="$(cat "$DIR/mea-base.cloud-init.yaml")"
lxc exec "$BUILD" -- cloud-init status --wait
lxc exec "$BUILD" -- bash -c 'apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* && truncate -s0 /etc/machine-id && cloud-init clean --logs'
lxc stop "$BUILD"
lxc image delete "$ALIAS" >/dev/null 2>&1 || true
lxc publish "$BUILD" --alias "$ALIAS" description="Mise En Abyme base, Ubuntu 24.04 LTS ($(date -u +%Y-%m-%d))"
FP=$(lxc image info "$ALIAS" | awk '/^Fingerprint:/ {print $2}')
echo "published $ALIAS fingerprint=$FP"
