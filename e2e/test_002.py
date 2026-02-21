import random
import os
import time

from mininet.log import info
from minindn.minindn import Minindn
from minindn.apps.app_manager import AppManager

from fw import NDNd_FW
import dv_util

def scenario(ndn: Minindn):
    """
    This scenario tests routing convergence when a router joins
    the network after the network has already converged.
    """

    # Choose a node with a single link to the network
    lazy_node = random.choice([node for node in ndn.net.hosts if len(node.intfList()) == 1])
    if not lazy_node:
        raise Exception('No lazy node found')
    others = [node for node in ndn.net.hosts if node != lazy_node]

    # Disconnect the node from the network
    info(f'Disconnecting {lazy_node.name}\n')
    downIntf = lazy_node.intfList()[0]
    downIntf.config(loss=99.99)

    info('Starting forwarder on nodes\n')
    AppManager(ndn, ndn.net.hosts, NDNd_FW)

    dv_util.setup(ndn)
    dv_util.converge(others)

    # Make sure the node is really disconnected
    if not dv_util.is_converged(others):
        raise Exception('Routing did not converge on other nodes (?!)')
    if dv_util.is_converged(ndn.net.hosts):
        raise Exception('Routing converged on lazy node (?!)')

    # Reconnect the node to the network
    info(f'Reconnecting {lazy_node.name}\n')
    downIntf.config(loss=0.0001)

    # Wait for convergence
    dv_util.converge(ndn.net.hosts)