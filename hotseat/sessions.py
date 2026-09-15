"""Count Claude Code processes currently running on this machine.

This number is the whole reason the dashboard treats switching as dangerous:
changing the machine-wide default retargets every one of them mid-conversation.
"""

from __future__ import annotations

import os
import subprocess


def running_sessions() -> int:
    """Best-effort count of live `claude` processes owned by this user.

    Returns 0 rather than raising if the process list cannot be read: a wrong
    count must never stop the dashboard from rendering.
    """
    try:
        result = subprocess.run(["ps", "-eo", "pid=,args="],
                                capture_output=True, text=True, timeout=10, check=False)
    except (OSError, subprocess.SubprocessError):
        return 0
    if result.returncode != 0:
        return 0

    mine = os.getpid()
    count = 0
    for line in result.stdout.splitlines():
        parts = line.strip().split(None, 1)
        if len(parts) != 2:
            continue
        pid_text, args = parts
        try:
            pid = int(pid_text)
        except ValueError:
            continue
        if pid == mine:
            continue
        executable = args.split()[0] if args.split() else ""
        name = os.path.basename(executable)
        # Match the CLI itself, not this dashboard or an editor holding the word.
        if name == "claude" or name.startswith("claude-"):
            if "hotseat" in args:
                continue
            count += 1
    return count
