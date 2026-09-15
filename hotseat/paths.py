"""Where the things Hotseat reads live on disk."""

from __future__ import annotations

import os
from pathlib import Path


def claude_config_dir() -> Path:
    override = os.environ.get("CLAUDE_CONFIG_DIR")
    return Path(override) if override else Path.home() / ".claude"


PROJECTS_DIR = Path.home() / ".claude" / "projects"
