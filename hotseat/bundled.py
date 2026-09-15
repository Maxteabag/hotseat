"""Launch a bundled Python module from either a checkout or an installed wheel."""
from pathlib import Path
import sys


def command(module, *args):
    # Keep the caller's working directory, but resolve our package first. Running
    # scripts inside hotseat/ directly would shadow stdlib inspect with ours.
    return [sys.executable, "-c",
            "import runpy,sys;sys.path.insert(0,sys.argv.pop(1));runpy.run_module(sys.argv.pop(1),run_name='__main__')",
            str(Path(__file__).resolve().parent.parent), module, *args]
