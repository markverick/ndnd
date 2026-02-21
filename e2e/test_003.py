import os
import random
import time

from mininet.log import info
from minindn.minindn import Minindn
from minindn.apps.app_manager import AppManager

from fw import NDNd_FW
import dv_util


def _pick_producer_gateway(nodes):
    links = []
    for node in nodes:
        for intf in node.intfList():
            if intf.link is None:
                continue
            other_intf = intf.link.intf2 if intf.link.intf1 == intf else intf.link.intf1
            links.append((node, other_intf.node, other_intf.IP()))
    if not links:
        raise Exception("No linked nodes found in topology")
    return random.choice(links)


def _run_cmd_or_fail(node, cmd, what):
    out = node.cmd(cmd)
    if "Status=200" in out or out.strip() == "":
        return out
    raise Exception(f"{what} failed on {node.name}\ncmd: {cmd}\nout:\n{out}")


def _configure_gateway_path(producer, gateway_ip):
    insert_prefix = "/localhop/route/insert"
    default_gateway = "/localhop/default"
    gateway_face = f"udp4://{gateway_ip}:6363"

    _run_cmd_or_fail(
        producer,
        f"ndnd fw pet-add-egress prefix={insert_prefix} egress={default_gateway}",
        "configure PET egress for prefix insertion",
    )
    _run_cmd_or_fail(
        producer,
        f"ndnd fw route-add prefix={default_gateway} face={gateway_face}",
        "configure route to gateway for prefix insertion",
    )


def _wait_for_gateway_insertion(node, prefix, deadline=120):
    info(f"Waiting for gateway insertion on {node.name} for {prefix}\n")
    start = time.time()
    while time.time() - start < deadline:
        out = node.cmd("ndnd dv prefix-list")
        if f"prefix={prefix} " in out:
            info(f"Gateway insertion visible on {node.name}\n")
            return
        time.sleep(1)
    raise Exception(f"Gateway insertion did not appear on {node.name} for {prefix}")


def scenario(ndn: Minindn, network="/minindn"):
    """
    Validate gateway-mode prefix insertion end-to-end.
    Security model:
    - DV routers run with trust anchors and signed router certs (dv_util.setup).
    - Exposing client sends signed prefix-insertion interests in gateway mode.
    """

    info("Starting forwarder on nodes\n")
    AppManager(ndn, ndn.net.hosts, NDNd_FW)

    dv_util.setup(ndn, network=network)
    dv_util.converge(ndn.net.hosts, network=network)

    producer, gateway, gateway_ip = _pick_producer_gateway(ndn.net.hosts)
    consumer_candidates = [n for n in ndn.net.hosts if n != producer and n != gateway]
    if not consumer_candidates:
        consumer_candidates = [n for n in ndn.net.hosts if n != producer]
    if not consumer_candidates:
        raise Exception("Need at least two nodes for prefix insertion E2E test")
    consumer = random.choice(consumer_candidates)

    prefix = f"{network}/{producer.name}/prefix-inserted"
    test_file = f"/tmp/prefix-insertion-{producer.name}.bin"
    recv_file = f"/tmp/prefix-insertion-{consumer.name}.recv.bin"
    put_log = f"/tmp/prefix-insertion-{producer.name}.put.log"

    os.system(f"dd if=/dev/urandom of={test_file} bs=64K count=1 status=none")
    _configure_gateway_path(producer, gateway_ip)

    cmd = (
        f"NDN_CLIENT_ROUTING_MODE=gateway "
        f"ndnd put --expose \"{prefix}\" < {test_file} > {put_log} 2>&1 &"
    )
    info(f"{producer.name} {cmd}\n")
    producer.cmd(cmd)

    _wait_for_gateway_insertion(gateway, prefix)
    dv_util.wait_prefix_pet_ready({consumer: {prefix}, gateway: {prefix}}, deadline=180)

    cat_cmd = f'ndnd cat "{prefix}" > {recv_file} 2> /tmp/prefix-insertion-cat.log'
    info(f"{consumer.name} {cat_cmd}\n")
    consumer.cmd(cat_cmd)

    if consumer.cmd(f"diff {test_file} {recv_file}").strip():
        cat_log = consumer.cmd("cat /tmp/prefix-insertion-cat.log")
        put_out = producer.cmd(f"tail -n 100 {put_log}")
        raise Exception(
            "Prefix insertion transfer mismatch\n"
            f"cat.log:\n{cat_log}\n"
            f"put.log:\n{put_out}\n"
        )
