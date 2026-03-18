#!/usr/bin/env bash
# e2e-run.sh — Docker entrypoint: install binaries and run e2e tests.
set -euo pipefail

log() { echo "==> $*"; }

# OpenVSwitch is required for Mininet virtual switches.
# ovs-vsctl can hang on macOS linuxkit kernels, so both calls are guarded with timeout.
log "Starting OpenVSwitch..."
timeout 15 service openvswitch-switch start 2>/dev/null || true
timeout 5  ovs-vsctl set-manager ptcp:6640  2>/dev/null || true

log "Installing binaries..."
install -m 755 /ndnd/.bin/ndnd /usr/local/bin/ndnd
log "  ndnd: $(ndnd --version 2>&1 | head -1 || echo unknown)"

# Mini-NDN's NFD config template needs readvertise_nlsr enabled.
sed -i 's/readvertise_nlsr no/readvertise_nlsr yes/g' \
  /usr/local/etc/ndn/nfd.conf.sample 2>/dev/null || true

# runner.py calls `go build` to ensure fresh binaries.
# Pre-built binaries are already in .bin/ so we shim `go` to be a no-op.
mkdir -p /usr/local/fake-go
cat > /usr/local/fake-go/go << 'EOF'
#!/bin/bash
exit 0
EOF
chmod +x /usr/local/fake-go/go
export PATH="/usr/local/fake-go:$PATH"

log "Running e2e tests..."
cd /ndnd
python3 e2e/runner.py e2e/topo.sprint.conf
