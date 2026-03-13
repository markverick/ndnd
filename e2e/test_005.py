import os
import time

from mininet.log import info
from minindn.minindn import Minindn
from minindn.apps.app_manager import AppManager

from fw import NDNd_FW
import dv_util


def _assign_bier_indices(hosts):
    """Assign deterministic BIER indices to hosts (0-based, alphabetical order)."""
    sorted_hosts = sorted(hosts, key=lambda h: h.name)
    return {host: idx for idx, host in enumerate(sorted_hosts)}


def scenario(ndn: Minindn, network='/minindn'):
    """
    BIER multicast file transfer E2E test.

    One producer exposes a random binary file; every other node in the 52-node
    Sprint topology fetches it via BIER multicast replication.  Verifies:
      - BIER bit-string construction at the BFIR
      - Multi-hop replication through transit BFRs
      - BFER local delivery and unicast data return
      - File integrity (byte-for-byte diff)
    """
    hosts = ndn.net.hosts
    if len(hosts) < 3:
        raise Exception('BIER test requires at least 3 nodes')

    bier_map = _assign_bier_indices(hosts)

    info('--- BIER setup: assigning bit indices ---\n')
    for host in sorted(hosts, key=lambda h: h.name):
        info(f'  bier_index={bier_map[host]:3d}  {host.name}\n')

    info('Starting ndnd forwarder on all nodes\n')
    for host in hosts:
        AppManager(ndn, [host], NDNd_FW, network=network, bier_index=bier_map[host])

    dv_util.setup(ndn, network=network)
    dv_util.converge(ndn.net.hosts, network=network)
    dv_util.populate_bift(hosts, bier_map, network=network)

    producer = sorted(hosts, key=lambda h: h.name)[0]
    consumers = [h for h in hosts if h != producer]

    prefix = f'{network}/{producer.name}/bier-test'
    test_file = '/tmp/bier-e2e-test.bin'
    os.system(f'dd if=/dev/urandom of={test_file} bs=64K count=1 status=none')
    info(f'--- BIER file transfer: producer={producer.name} prefix={prefix} ---\n')
    info(f'  {len(consumers)} consumers on {len(hosts)}-node topology\n')

    producer.cmd(f'ndnd put --expose "{prefix}" < {test_file} > /tmp/bier-put.log 2>&1 &')

    # Wait for prefix to appear in PET on every node (confirms BIER egress table is ready)
    expected = {node: {prefix} for node in hosts}
    dv_util.wait_prefix_pet_ready(expected, deadline=180)

    info('--- Fetching data via BIER multicast on all consumers ---\n')
    failures = []
    for consumer in consumers:
        recv_file = f'/tmp/bier-recv-{consumer.name}.bin'
        consumer.cmd(f'ndnd cat "{prefix}" > {recv_file} 2>/tmp/bier-cat-{consumer.name}.log')
        diff = consumer.cmd(f'diff {test_file} {recv_file}').strip()
        if diff:
            log = consumer.cmd(f'cat /tmp/bier-cat-{consumer.name}.log')
            failures.append(f'{consumer.name}: file mismatch\n{log}')
        else:
            info(f'  [OK] {consumer.name}\n')

    if failures:
        # Dump BIER-relevant forwarder logs from the producer node for diagnosis
        bier_debug = producer.cmd(
            'cat /tmp/minindn/' + producer.name + '/log/yanfd.log'
            ' | grep -iE "bier|bift|bfir|bfr|bfer|strategy" | tail -60'
        )
        raise Exception(
            f'BIER file transfer failed: {len(failures)}/{len(consumers)} consumers\n'
            + '\n'.join(failures)
            + f'\n--- BIER forwarder log ({producer.name}) ---\n{bier_debug}'
        )

    info(f'BIER file transfer passed: {len(consumers)}/{len(consumers)} consumers OK\n')
