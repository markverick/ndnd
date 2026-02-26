import random
import os
import subprocess
import time
from pathlib import Path

from types import FunctionType

from mininet.log import setLogLevel, info
from minindn.minindn import Minindn
from minindn.util import MiniNDNCLI

import test_001
import test_002
import test_003
import test_004


def ensure_local_ndnd() -> None:
    repo_root = Path(__file__).resolve().parent.parent
    local_bin = repo_root / ".bin"
    local_ndnd = local_bin / "ndnd"
    local_bin.mkdir(parents=True, exist_ok=True)
    if not local_ndnd.exists():
        info("Building local ndnd binary for E2E scenarios\n")
        subprocess.check_call(
            ["go", "build", "-o", str(local_ndnd), "./cmd/ndnd"],
            cwd=repo_root,
        )
    os.environ["PATH"] = f"{local_bin}:{os.environ.get('PATH', '')}"


def run(scenario: FunctionType, **kwargs) -> None:
    try:
        random.seed(0)

        info(f"===================================================\n")
        start = time.time()
        scenario(ndn, **kwargs)
        info(f'Scenario completed in: {time.time()-start:.2f}s\n')
        info(f"===================================================\n\n")
        # MiniNDNCLI(ndn.net)

        # Call all cleanups without stopping the network
        # This ensures we don't recreate the network for each test
        for cleanup in reversed(ndn.cleanups):
            cleanup()
    except Exception as e:
        ndn.stop()
        raise e
    finally:
        # kill everything we started just in case ...
        os.system('pkill -9 ndnd')
        os.system('pkill -9 nfd')

if __name__ == '__main__':
    setLogLevel('info')

    ensure_local_ndnd()
    Minindn.cleanUp()
    Minindn.verifyDependencies()

    ndn = Minindn()
    ndn.start()

    run(test_001.scenario)
    run(test_002.scenario)
    run(test_003.scenario)
    run(test_004.scenario)

    ndn.stop()
