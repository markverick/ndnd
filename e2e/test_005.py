import random
import os
import time
from pathlib import Path

from mininet.log import info
from minindn.minindn import Minindn
from minindn.apps.app_manager import AppManager

from fw import NDNd_FW
import dv_util

def require_alo_sync():
    alo_bin_path = Path.cwd() / ".bin/alo-latest"
    if not alo_bin_path.exists():
        raise RuntimeError(
            f"alo-latest not found in {alo_bin_path!r}"
            "please compile alo-latest using `make examples`"
        )

def scenario(ndn: Minindn, network='/minindn'):
    """
    Test replicast using alo-latest svs sync application from std/examples
    """

    info('Starting forwarder on nodes\n')
    require_alo_sync()
    AppManager(ndn, ndn.net.hosts, NDNd_FW, network=network)

    dv_util.setup(ndn, network=network)
    dv_util.converge(ndn.net.hosts, network=network)

    sample_size = min(8, len(ndn.net.hosts) - 1)
    publish_size = min(2, sample_size - 1)
    chatters = random.sample(ndn.net.hosts, sample_size)
    chatter_names = {f"{network}/{node.name}" for node in chatters}

    # set the svs forwarding strategy to replicast as the alo-latest example says to do
    for node in ndn.net.hosts:
        node.cmd("ndnd fw strategy-set prefix=/ndn/svs strategy=/localhost/nfd/strategy/replicast/v=1")


    # start the alo client on each node
    for node in chatters:
        node.sendCmd(f"alo-latest {network}/{node.name}")

    def receive_chat_message(node):
        buf = []
        while 1:
            ch = node.read(1)
            buf.append(ch)
            if ch == "\n":
                line = "".join(buf)
                if line.split(":")[0] in chatter_names and "Skipping" not in line:
                    return line
                buf = []

    try:
        beg = time.time()
        for node in chatters[:publish_size]:
            # send a random message into chat
            msg = os.urandom(8).hex()
            node.write(msg + "\n")
            info(f"[t={time.time() - beg:.2f}] {node.name} publishing {msg!r}\n")

            for other_node in chatters:
                if other_node.name == node.name:
                    continue
                received = receive_chat_message(other_node)
                info(f"[t={time.time() - beg:.2f}] {other_node.name} received {received!r}\n")
                assert msg in received, "received wrong message"

    # custom cleanup logic for this test
    finally:
        for node in chatters:
            info(f"stopping alo-latest on {node.name}\n")
            # send interrupt to alo-latest and reset the cmd stdout buffer read logic
            node.popen(["pkill", "alo-latest"], stdout=-1).stdout.readline()
            node.waiting = False
            node.cmd("ls")
