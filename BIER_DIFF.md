# BIER Integration: Diff vs upstream named-data/ndnd@dv2

This repo adds BIER (Bit Index Explicit Replication) multicast support on top of
the upstream `dv2` branch. All changes are backward-compatible; non-BIER traffic
is unaffected.

---

## New Files (not in upstream)

| File | Purpose |
|------|---------|
| `fw/fw/bier.go` (296 lines) | BIFT table, bit-string helpers (`BierClone`, `BierClearBit`, `BierIsZero`, `BuildBierBitString`), `BuildFromFib()` rebuilds BIFT from FIB state |
| `fw/fw/bier_strategy.go` | `BierStrategy` — PIT-tandem BIER forwarding; replicates Interest per-bit via `SendInterest`; shared `bierReplicate()` function used by both `BierStrategy` and `Multicast` |
| `fw/mgmt/bift.go` (95 lines) | Management face handler for BIER Forwarding Information Base Table (BIFT) |
| `cmd/svs-chat/main.go` | SVS ALO chat CLI demo that exercises BIER multicast sync |
| `fw/bier_tests/` | Unit + integration tests for BIER (5 files, including `bier_fib_rebuild_test.go`) |
| `e2e/test_005.py` | E2E test: BIER multicast on 52-node Sprint topology (51/51 consumers OK) |
| `e2e/test_006.py` | E2E test: multi-group BIER (two concurrent prefixes) |
| `e2e/test_007.py` | E2E test: SVS Chat over BIER (4/4 consumers receive message) |
| `e2e/test_008.py` | E2E test: BIER end-to-end correctness |
| `e2e-run.sh` | Docker entrypoint: installs binaries, fake `go` shim, runs Mininet tests |
| `run-bier-e2e.sh` | macOS helper: cross-compiles Linux binaries, launches Docker |

---

## Modified Files

### `fw/core/config.go`
- Added `BierIndex int` field (`bier_index`) to `Fw` config struct
- Added `RouterName string` field (`router_name`) to `Fw` config struct
- Default value `-1` (disabled) set in `DefaultConfig()`
- Operators set `bier_index: N` in YAML config to enable BIER on a node

### `fw/yanfd.sample.yml`
- Added `bier_index` and `router_name` fields under `fw:`

### `fw/face/ndnlp-link-service.go`
- `sendPacket()` (line 262): copies `packet.Bier` onto the NDNLPv2 fragment TLV `0x035a` for outgoing Interests
- `handleIncomingFrame()` (line 356): reads `LP.Bier` from incoming frame and attaches it to the parsed packet

### `fw/fw/multicast.go`
- `AfterReceiveInterest()`: when `packet.Bier != nil`, delegates to the shared `bierReplicate()` function (BIER-targeted replication). Falls back to flood-to-all nexthops with retransmission suppression for non-BIER interests (needed by DV routing advertisement sync which carries no bit-string).

### `fw/fw/bier_strategy.go`
- Extracted `bierReplicate()` as a package-level function shared by both `BierStrategy` and `Multicast`, taking a `sendInterest` callback so either strategy can drive replication through its own PIT machinery.
- `BierStrategy.AfterReceiveInterest()` fallback (no bit-string): floods to all nexthops with retransmission suppression, matching `Multicast` behavior — making the two strategies interchangeable.
- `BierStrategy.replicateBier()` delegates to `bierReplicate()`.

### `fw/fw/thread.go`
Four logical additions in `processInterest` / `twoPhaseLookup`:

1. **BFER+BFR combined delivery** (line 372): after local PIT match, clear the local bit from the bit-string and fall through to network replication (so a node that is both a consumer and a transit BFR still forwards to remaining bits)

2. **BFIR encoding** (line 403): when `twoPhaseLookup` returns >1 egress router, `IsBierEnabled()`, and `isMulticast=true`, call `Bift.BuildBierBitString()` to pre-encode a bit-string on the outgoing packet

3. **Auto-strategy selection** (line 412): when `packet.Bier != nil`, override the strategy to `BierStrategy` so bit-level replication runs through the PIT (PIT-tandem design)

4. **Multicast flag from PET** (`twoPhaseLookup`): returns a 4th value `isMulticast bool` from `petEntry.Multicast`, so the BFIR condition is gated on whether the prefix is a Sync group prefix rather than a multihomed producer prefix

### `dv/dv/table_algo.go`
- After FIB updates, calls `fw.Bift.BuildFromFib()` when BIER is enabled, keeping the BIFT consistent with routing changes

### `std/ndn/client.go`
- Added `Multicast bool` field to `Announcement` struct, allowing clients to explicitly mark Sync group prefixes for BIER multicast

### `std/object/client_announce.go`
- Encodes `Announcement.Multicast` as `Flags` bit 0 in `ControlArgs` when announcing via local routing daemon

### `cmd/svs-chat/main.go`
- Uses `Multicast: true` on the sync group prefix announcement (replaces the previous `Cost: 1` heuristic)

### `dv/dv/insertion.go`
- Prefix insertion calls `pfx.Announce()` (non-multicast) — insertion is for producer prefixes

### `dv/dv/mgmt.go`
- Reads `Flags` bit 0 from management command to determine multicast; calls `AnnounceSync()` for Sync group prefixes, `Announce()` otherwise

### `dv/dv/prefix.go`
- Split into `Announce()` (multicast=false) and `AnnounceSync()` (multicast=true), with shared `announce()` implementation
- PET egress operations signal multicast to the forwarder via `Flags` bit 0 (replaces `Cost: 1` convention)
- Multicast flag for remote prefixes is set via `processUpdate()` from the DV protocol TLV

### `fw/mgmt/pet.go`
- `addEgress()` reads `Flags` bit 0 for multicast detection (replaces `Cost==1` convention)

### `e2e/runner.py`
- `ensure_local_binaries()` skips building binaries that already exist in `.bin/` or PATH

---

## Architecture in One Paragraph

BIER piggybacks on NDNLPv2 via TLV `0x035a`. The BFIR (first-hop router) detects
a Sync group prefix (PET entry with `Multicast=true`) with >1 egress router and
stamps a bit-string onto the Interest. `BierStrategy` (and `Multicast` strategy,
which is now BIER-aware) then replicates the Interest once per set bit, sending
one copy per downstream router through the normal PIT machinery. Each BFR clears
its own bit before forwarding, so no router receives a copy destined only for it;
the Interest naturally terminates when all bits are cleared. BFER nodes deliver
locally and then re-forward any remaining bits. No bypass of the PIT occurs.
Multihomed producer prefix announcements (`Multicast=false`) are unaffected and
never trigger BIER encoding. The BIFT is rebuilt from FIB state (`BuildFromFib()`)
whenever the FIB changes in `table_algo.go`, keeping BIER forwarding paths
consistent with the routing table.
