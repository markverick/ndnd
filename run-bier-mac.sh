#!/usr/bin/env bash
# run-bier-mac.sh — Build Linux binaries and run e2e tests in Docker on macOS.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The script lives inside the ndnd/ repo root, so ROOT_DIR IS the ndnd dir.
NDND_DIR="${ROOT_DIR}"

ARCH="$(uname -m)"
GOARCH="${ARCH/x86_64/amd64}"
GOARCH="${GOARCH/arm64/arm64}"

echo "=== Building Linux binaries (GOARCH=${GOARCH}) ==="
cd "${NDND_DIR}"
CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" go build -o .bin/ndnd ./cmd/ndnd
echo "Built: .bin/ndnd"

echo "=== Running e2e tests in Docker ==="
docker run --rm \
  --privileged \
  --sysctl net.ipv6.conf.all.disable_ipv6=0 \
  --entrypoint /bin/bash \
  -v "${NDND_DIR}":/ndnd \
  ghcr.io/named-data/mini-ndn:master \
  /ndnd/e2e-run.sh
