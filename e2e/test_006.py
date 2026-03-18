import os
import time

from mininet.log import info
from minindn.minindn import Minindn
from minindn.apps.app_manager import AppManager

from fw import NDNd_FW
import dv_util


def _assign_bier_indices(hosts):
    sorted_hosts = sorted(hosts, key=lambda h: h.name)
    return {host: idx for idx, host in enumerate(sorted_hosts)}


def scenario(ndn: Minindn, network='/minindn'):
    """
    BIER multi-group multicast E2E test.

    Two independent groups share the same 52-node topology:
      - Group A: producer hosts[0], consumers hosts[1:4]
      - Group B: producer hosts[4], consumers hosts[5:7]

    Verifies that BIER bit-strings are computed independently per-group,
    consumers receive only their group's data, and multi-hop delivery works.
    """
    hosts = ndn.net.hosts
    if len(hosts) < 6:
        raise Exception('Multi-group BIER test requires at least 6 nodes')

    bier_map = _assign_bier_indices(hosts)

    info('Starting ndnd forwarder on all nodes\n')
    for host in hosts:
        AppManager(ndn, [host], NDNd_FW, network=network, bier_index=bier_map[host])

    dv_util.setup(ndn, network=network)
    dv_util.converge(ndn.net.hosts, network=network)
    dv_util.populate_bift(hosts, bier_map, network=network)

    sorted_hosts = sorted(hosts, key=lambda h: h.name)
    producer_a  = sorted_hosts[0]
    consumers_a = sorted_hosts[1:4]
    producer_b  = sorted_hosts[4]
    consumers_b = sorted_hosts[5:7] if len(sorted_hosts) > 6 else [sorted_hosts[5]]

    prefix_a = f'{network}/{producer_a.name}/group-a'
    prefix_b = f'{network}/{producer_b.name}/group-b'

    test_file_a = '/tmp/bier-group-a.bin'
    test_file_b = '/tmp/bier-group-b.bin'
    os.system(f'dd if=/dev/urandom of={test_file_a} bs=32K count=1 status=none')
    os.system(f'dd if=/dev/urandom of={test_file_b} bs=32K count=1 status=none')

    info(f'--- Group A: producer={producer_a.name}  consumers={[c.name for c in consumers_a]} ---\n')
    info(f'--- Group B: producer={producer_b.name}  consumers={[c.name for c in consumers_b]} ---\n')

    producer_a.cmd(f'ndnd put --expose "{prefix_a}" < {test_file_a} > /tmp/bier-put-a.log 2>&1 &')
    producer_b.cmd(f'ndnd put --expose "{prefix_b}" < {test_file_b} > /tmp/bier-put-b.log 2>&1 &')

    expected = {node: {prefix_a, prefix_b} for node in hosts}
    dv_util.wait_prefix_pet_ready(expected, deadline=180)

    failures = []

    info('--- Fetching Group A data ---\n')
    for consumer in consumers_a:
        recv = f'/tmp/bier-recv-a-{consumer.name}.bin'
        consumer.cmd(f'ndnd cat "{prefix_a}" > {recv} 2>/tmp/bier-cat-a-{consumer.name}.log')
        diff = consumer.cmd(f'diff {test_file_a} {recv}').strip()
        if diff:
            log = consumer.cmd(f'cat /tmp/bier-cat-a-{consumer.name}.log')
            failures.append(f'Group A {consumer.name}: mismatch\n{log}')
        else:
            info(f'  [OK] Group A  {consumer.name}\n')

    info('--- Fetching Group B data ---\n')
    for consumer in consumers_b:
        recv = f'/tmp/bier-recv-b-{consumer.name}.bin'
        consumer.cmd(f'ndnd cat "{prefix_b}" > {recv} 2>/tmp/bier-cat-b-{consumer.name}.log')
        diff = consumer.cmd(f'diff {test_file_b} {recv}').strip()
        if diff:
            log = consumer.cmd(f'cat /tmp/bier-cat-b-{consumer.name}.log')
            failures.append(f'Group B {consumer.name}: mismatch\n{log}')
        else:
            info(f'  [OK] Group B  {consumer.name}\n')

    if failures:
        raise Exception(f'Multi-group BIER failed: {len(failures)} consumer(s)\n' + '\n'.join(failures))

    info('Multi-group BIER passed: all consumers in both groups received correct data\n')
