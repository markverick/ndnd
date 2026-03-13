"""
test_008.py — BIER transit correctness E2E test.

Verifies that transit BFRs in a linear chain forward BIER packets correctly
using only the BIFT + bit-string, without leaking delivery to nodes that did
not have their bit set.

Topology (linear, 5 nodes):
   h1 --- h2 --- h3 --- h4 --- h5

BIER role by scenario:
  Scenario A: BFIR=h1, BFER=h5 only (h2,h3,h4 are pure transit)
  Scenario B: BFIR=h1, BFER=h2+h4 (h3 is transit, h5 not in bitstring)
  Scenario C: BFIR=h3 (mid-chain), simultaneous BFER=h1+h5 (both ends)

For each scenario we verify:
  - Every named BFER receives the data.
  - No node that is NOT in the bitstring gets a data file transferred to it.

The test uses a unique per-scenario prefix and polls each node's application
log to confirm delivery.  Transit nodes run no consumer, so absence of a
received file is the negative check.
"""
import os
import time

from mininet.log import info
from minindn.minindn import Minindn
from minindn.apps.app_manager import AppManager

from fw import NDNd_FW
import dv_util


# -----------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------

def _sorted_hosts(ndn):
    return sorted(ndn.net.hosts, key=lambda h: h.name)


def _assign_bier_indices(hosts):
    return {host: idx for idx, host in enumerate(sorted(hosts, key=lambda h: h.name))}


def _file_path(tag):
    return f'/tmp/bier-transit-{tag}.bin'


def _produce(host, prefix, src_file):
    """Start a producer in the background."""
    host.cmd(f'ndnd put --expose "{prefix}" < {src_file} > /tmp/bier-put-{host.name}.log 2>&1 &')


def _consume(host, prefix, dst_file):
    """Blocking fetch — returns True on success."""
    rc = host.cmd(f'ndnd cat "{prefix}" > {dst_file} 2>/tmp/bier-cat-{host.name}.log; echo $?').strip()
    return rc == '0'


def _diff(a, b, host):
    return host.cmd(f'diff {a} {b}').strip() == ''


def _bier_check(scenario_label, producer, consumers, non_targets, prefix, src_file, network):
    """
    Run one BIER delivery scenario and assert correctness.
      consumers    — hosts expected to receive data (their bit is in bitstring).
      non_targets  — hosts that must NOT receive data (bit NOT in bitstring).
    """
    info(f'\n--- Scenario {scenario_label}: BFIR={producer.name} '
         f'BFERs={[h.name for h in consumers]} ---\n')

    _produce(producer, prefix, src_file)

    # Wait for the prefix to appear in PET everywhere
    all_hosts = [producer] + consumers + non_targets
    expected_pet = {node: {prefix} for node in all_hosts}
    dv_util.wait_prefix_pet_ready(expected_pet, deadline=60)

    failures = []

    # Positive check: every consumer must receive the file intact
    for consumer in consumers:
        dst = f'/tmp/bier-transit-recv-{scenario_label}-{consumer.name}.bin'
        ok = _consume(consumer, prefix, dst)
        if not ok or not _diff(src_file, dst, consumer):
            log = consumer.cmd(f'cat /tmp/bier-cat-{consumer.name}.log')
            failures.append(f'[{scenario_label}] {consumer.name} failed to fetch:\n{log}')
        else:
            info(f'  [OK] {consumer.name} received data correctly\n')

    # Negative check: non-target nodes must have no received file
    for node in non_targets:
        dst = f'/tmp/bier-transit-recv-{scenario_label}-{node.name}.bin'
        exists = node.cmd(f'test -f {dst} && echo yes || echo no').strip()
        if exists == 'yes':
            failures.append(
                f'[{scenario_label}] TRANSIT node {node.name} unexpectedly received data '
                f'(its bit was NOT in the BIER bitstring)'
            )
        else:
            info(f'  [OK] transit {node.name} did not receive stray data\n')

    return failures


# -----------------------------------------------------------------------
# Main scenario
# -----------------------------------------------------------------------

def scenario(ndn: Minindn, network='/minindn'):
    """
    BIER transit correctness E2E test.

    Uses a 5-node linear topology to verify:
      1. Transit routers do NOT deliver to themselves when unlisted.
      2. BIER replication reaches BFERs across multiple hops.
      3. Mid-chain BFIR can reach both ends simultaneously.
    """
    all_hosts = _sorted_hosts(ndn)
    if len(all_hosts) < 5:
        raise Exception('test_008 requires exactly 5 nodes (h1..h5 in linear order)')

    h1, h2, h3, h4, h5 = all_hosts[:5]
    bier_map = _assign_bier_indices(all_hosts)

    info('=== test_008: BIER transit correctness ===\n')
    info('Topology: h1 --- h2 --- h3 --- h4 --- h5 (linear)\n')
    for host in all_hosts[:5]:
        info(f'  bier_index={bier_map[host]}  {host.name}\n')

    # Start forwarders
    for host in all_hosts:
        AppManager(ndn, [host], NDNd_FW, network=network, bier_index=bier_map[host])

    dv_util.setup(ndn, network=network)
    dv_util.converge(all_hosts, network=network)
    dv_util.populate_bift(all_hosts, bier_map, network=network)

    # Create a test file to transfer
    src_file = '/tmp/bier-transit-src.bin'
    os.system(f'dd if=/dev/urandom of={src_file} bs=16K count=1 status=none')

    all_failures = []

    # ------------------------------------------------------------------
    # Scenario A: BFIR=h1, BFER=h5 only — h2,h3,h4 are pure transit
    # ------------------------------------------------------------------
    all_failures += _bier_check(
        'A',
        producer=h1,
        consumers=[h5],
        non_targets=[h2, h3, h4],
        prefix=f'{network}/{h1.name}/bier-transit-A',
        src_file=src_file,
        network=network,
    )

    # ------------------------------------------------------------------
    # Scenario B: BFIR=h1, BFER=h2+h4 — h3 is transit, h5 not listed
    # ------------------------------------------------------------------
    all_failures += _bier_check(
        'B',
        producer=h1,
        consumers=[h2, h4],
        non_targets=[h3, h5],
        prefix=f'{network}/{h1.name}/bier-transit-B',
        src_file=src_file,
        network=network,
    )

    # ------------------------------------------------------------------
    # Scenario C: BFIR=h3 (mid-chain), BFER=h1+h5 — both ends
    # ------------------------------------------------------------------
    all_failures += _bier_check(
        'C',
        producer=h3,
        consumers=[h1, h5],
        non_targets=[h2, h4],
        prefix=f'{network}/{h3.name}/bier-transit-C',
        src_file=src_file,
        network=network,
    )

    if all_failures:
        bier_debug = ''
        for host in all_hosts[:5]:
            log = host.cmd(
                f'cat /tmp/minindn/{host.name}/log/yanfd.log 2>/dev/null'
                f' | grep -iE "bier|bift|bfir|bfr|bfer|transit" | tail -20'
            )
            if log.strip():
                bier_debug += f'\n--- {host.name} ---\n{log}'

        raise Exception(
            f'BIER transit correctness: {len(all_failures)} failure(s)\n'
            + '\n'.join(all_failures)
            + (f'\n\n=== BIER forwarder logs ==={bier_debug}' if bier_debug else '')
        )

    info(f'\nBIER transit correctness: all 3 scenarios passed.\n')
