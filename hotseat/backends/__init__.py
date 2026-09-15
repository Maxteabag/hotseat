"""Platform selection for credential access."""

from __future__ import annotations

import sys

from .base import Account, Backend, BackendError
from .darwin import DarwinBackend
from .linux import LinuxBackend

__all__ = ["Account", "Backend", "BackendError", "DarwinBackend", "LinuxBackend", "for_platform"]


def for_platform(platform: str | None = None) -> Backend:
    """Pick the backend for this machine.

    macOS stores credentials in the Keychain; everything else uses files.
    """
    return DarwinBackend() if (platform or sys.platform) == "darwin" else LinuxBackend()
