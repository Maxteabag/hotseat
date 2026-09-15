"""Launch Codex against a saved profile without changing the global default."""
import os
from pathlib import Path
import re
import shutil
import subprocess
from . import actions, codex

SHARED_ENTRIES=("config.toml", "AGENTS.md", "skills", "plugins", "rules")


def prepare(alias):
    if not re.fullmatch(r"[A-Za-z0-9_-]+", alias):
        raise actions.ActionError("Invalid saved profile name")
    account=next((a for a in codex.accounts() if a['alias']==alias and a['saved']),None)
    if account is None:
        raise actions.ActionError("Saved Codex account not found")
    home=codex.PROFILES_DIR/alias
    if home.is_symlink() or (home/"auth.json").is_symlink():
        raise actions.ActionError("Refusing a symlinked profile directory")
    # The profile itself is the persistent CODEX_HOME: refreshed credentials stay
    # in the canonical auth.json instead of being stranded in a temporary copy.
    home.chmod(0o700)
    for name in SHARED_ENTRIES:
        source=codex.CODEX_HOME/name
        target=home/name
        if source.exists() and not target.exists() and not target.is_symlink():
            target.symlink_to(source.resolve(),target_is_directory=source.is_dir())
    env={k:v for k,v in os.environ.items() if k not in ("OPENAI_API_KEY","OPENAI_BASE_URL","CODEX_ACCESS_TOKEN","CODEX_THREAD_ID","CODEX_SESSION_ID")}
    env['CODEX_HOME']=str(home)
    return home,env


def launch(alias,spawn=None,terminal=None):
    terminal=terminal or actions.detect_terminal()
    if terminal is None:raise actions.ActionError("No supported terminal found")
    executable=shutil.which('codex')
    if executable is None:raise actions.ActionError("Codex CLI is not installed")
    home,env=prepare(alias)
    # Set identity inside the terminal too: terminal-server processes may inherit
    # a different environment from the launcher.
    inner=['env']
    for key in ('OPENAI_API_KEY','OPENAI_BASE_URL','CODEX_ACCESS_TOKEN','CODEX_THREAD_ID','CODEX_SESSION_ID'):
        inner += ['-u',key]
    inner += ['CODEX_HOME='+str(home),executable,'-c','cli_auth_credentials_store="file"']
    argv=[terminal,actions._terminal_flag(terminal),*inner]
    (spawn or subprocess.Popen)(argv,env=env,start_new_session=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    return {'started':True,'alias':alias,'terminal':terminal,'config_dir':str(home)}
