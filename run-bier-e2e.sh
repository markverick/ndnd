#!/usr/bin/env bash
# run-bier-e2e.sh — Build Linux binaries and run BIER e2e tests in Mini-NDN Docker.
set -euo pipefail

REPO="$(cd "$(dirname "$0")" && pwd)"
BIN="$REPO/.bin"

log() { echo "==> $*"; }

mkdir -p "$BIN"

log "Building ndnd (linux/amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN/ndnd" ./cmd/ndnd/main.go
log "  $(du -sh "$BIN/ndnd" | cut -f1)  $BIN/ndnd"

log "Building svs-chat (linux/amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BIN/svs-chat" ./cmd/svs-chat/main.go
log "  $(du -sh "$BIN/svs-chat" | cut -f1)  $BIN/svs-chat"

log "Launching Mini-NDN container..."
docker run --rm \
  --privileged \
  --sysctl net.ipv6.conf.all.disable_ipv6=0 \
  --entrypoint /bin/bash \
  -v "$REPO":/ndnd \
  ghcr.io/named-data/mini-ndn:master \
  /ndnd/e2e-run.sh
