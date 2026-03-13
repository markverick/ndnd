#!/usr/bin/env bash
# run-bier-mac.sh — Build Linux binaries and run e2e tests in Docker on macOS.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NDND_DIR="${ROOT_DIR}/ndnd"

ARCH="$(uname -m)"
GOARCH="${ARCH/x86_64/amd64}"
GOARCH="${GOARCH/arm64/arm64}"

echo "=== Building Linux binaries (GOARCH=${GOARCH}) ==="
cd "${NDND_DIR}"
CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" go build -o .bin/ndnd     ./cmd/ndnd/main.go
CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" go build -o .bin/svs-chat ./cmd/svs-chat/main.go
echo "Built: .bin/ndnd, .bin/svs-chat"

echo "=== Running e2e tests in Docker ==="
docker run --rm \
  --privileged \
  --sysctl net.ipv6.conf.all.disable_ipv6=0 \
  --entrypoint /bin/bash \
  -v "${NDND_DIR}":/ndnd \
  ghcr.io/named-data/mini-ndn:master \
  /ndnd/e2e-run.sh
