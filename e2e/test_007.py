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
    SVS Chat E2E test using BIER (PIT-tandem architecture) + alo-latest snapshot.

    5 chat participants on the 52-node Sprint topology; remaining nodes are transit BFRs.
    SVS sync interests travel through the PIT at each BFR hop:
      - BFIR: twoPhaseLookup returns >1 egress routers → bit-string built → BierStrategy replicates
      - BFR:  Interest enters full pipeline (PIT insert, DNL check) → BierStrategy replicates via BIFT
      - BFER: local delivery, local bit cleared, BierStrategy forwards remaining bits
      - Data: PIT out-records track return path; consumers fetch via unicast
    alo-latest (SnapshotNodeLatest, threshold=5) ensures late-joining consumers
    receive the latest state snapshot even if they missed the initial sync.
    """
    hosts = ndn.net.hosts
    if len(hosts) < 3:
        raise Exception('BIER SVS test requires at least 3 nodes')

    bier_map = _assign_bier_indices(hosts)

    info('Starting ndnd forwarder on all nodes\n')
    for host in hosts:
        AppManager(ndn, [host], NDNd_FW, network=network, bier_index=bier_map[host])

    dv_util.setup(ndn, network=network)
    dv_util.converge(ndn.net.hosts, network=network)
    dv_util.populate_bift(hosts, bier_map, network=network)

    # 5 active chat participants (sorted for determinism); rest are transit BFRs.
    sorted_hosts = sorted(hosts, key=lambda h: h.name)
    chat_nodes = sorted_hosts[:5]
    producer   = chat_nodes[0]
    consumers  = chat_nodes[1:]

    sync_prefix = f'{network}/svs/32=svs'
    msg = f'Hello BIER from {producer.name}'

    info(f'--- SVS Chat: producer={producer.name}  consumers={[c.name for c in consumers]} ---\n')
    info(f'  {len(hosts) - 5} transit BFR nodes, {len(hosts)}-node Sprint topology\n')

    # Step 1: Start consumers — they announce their prefixes so BIER bit-string can be built.
    info('Starting SVS Chat consumers (alo-latest)...\n')
    for consumer in consumers:
        consumer.cmd(
            f'ndnd svs-chat --prefix "{network}/svs" --name "/{consumer.name}" --wait 300'
            f' > /tmp/chat-recv-{consumer.name}.log 2>&1 &'
        )

    # Step 2: Wait for all consumer prefixes to appear in the PET on every node.
    # The BFIR builds its BIER bit-string from the PET egress list — this must be complete first.
    consumer_prefixes = {sync_prefix} | {f'{network}/svs/{c.name}' for c in consumers}
    info('Waiting for consumer PET prefix replication on all nodes...\n')
    dv_util.wait_prefix_pet_ready({node: consumer_prefixes for node in hosts}, deadline=180)

    # Step 3: Start producer. All consumer egress routers are now in the PET.
    # --delay 20: SVS state-vector exchange before publishing.
    info(f'Starting SVS Chat producer ({producer.name})...\n')
    producer.cmd(
        f'ndnd svs-chat --prefix "{network}/svs" --name "/{producer.name}"'
        f' --msg "{msg}" --delay 20 --wait 120'
        f' > /tmp/chat-prod.log 2>&1 &'
    )

    # Step 4: Wait for producer data prefix to propagate so consumers can unicast-fetch data.
    producer_prefixes = {sync_prefix, f'{network}/svs/{producer.name}'}
    dv_util.wait_prefix_pet_ready({node: producer_prefixes for node in consumers}, deadline=60)

    beg = time.time()
    # Step 5: Wait for publish → BIER sync interest propagation → unicast data fetch.
    # 20s delay + 30s SVS sync cycle + 20s fetch buffer = 70s total.
    info(f'Waiting for message propagation from {producer.name} (70s)...\n')
    time.sleep(70)

    info('Verifying SVS Chat message propagation...\n')
    for node in chat_nodes:
        node.cmd("pkill -f 'ndnd svs-chat' 2>/dev/null; true")
    time.sleep(1)  # let log buffers flush
    print(time.time() - beg, "seconds elapsed")

    failures = []
    for consumer in consumers:
        log_output = consumer.cmd(f'cat /tmp/chat-recv-{consumer.name}.log').strip()
        if msg not in log_output:
            info(f'  [FAIL] {consumer.name} did not receive message\n')
            failures.append(f'{consumer.name} missing message. Log:\n{log_output}')
        else:
            info(f'  [OK]   {consumer.name}  "{msg}"\n')

    if failures:
        prod_log = producer.cmd('cat /tmp/chat-prod.log')
        # Copy logs out of container for inspection
        for node in chat_nodes:
            node.cmd(f'mkdir -p /ndnd/e2e/logs/{node.name}')
            node.cmd(f'cp /tmp/minindn/{node.name}/log/yanfd.log /ndnd/e2e/logs/{node.name}/yanfd.log 2>/dev/null || true')
        raise Exception(
            f'SVS Chat E2E failed: {len(failures)}/{len(consumers)} consumers\n'
            + '\n'.join(failures)
            + f'\nProducer log:\n{prod_log}'
            + dv_util.dump_bier_logs(chat_nodes, label='chat nodes')
        )

    info('SVS Chat passed: PIT-tandem BIER multicast sync + unicast data fetch (alo-latest)\n')
