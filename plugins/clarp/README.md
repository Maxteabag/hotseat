# Hotseat Clarp plugin

Optional agent monitoring and recovery for an existing local Clarp installation.
Hotseat's account CLI, terminal UI, and quota readers work without this package.

Install the core and plugin into the **same Python environment** from this repo:

```bash
python3 -m pip install .
python3 -m pip install ./plugins/clarp
```

After the distributions have been published, the equivalent is
`python3 -m pip install hotseat-clarp`. No package has to be installed merely to
use the core TUI.

Installing registers the `hotseat.plugins` entry point. The next Hotseat process
adds:

- `hotseat clarp`: agent state and account attribution.
- Clarp sources for `hotseat resume` and `hotseat inspect`.
- Agent and stopped-work panels in the web dashboard.

The plugin reads the local Clarp state database when its integration is requested
and uses `clarp-admin` for supported operations. `resume --go` sends continuation
prompts only for ready agents. Paused queues remain guarded. Missing Clarp is an
unavailable integration, not a failure of Hotseat's account management.

Uninstall with `python3 -m pip uninstall hotseat-clarp`, or temporarily disable all
plugins with `HOTSEAT_PLUGINS=none`. Restart Hotseat after changing plugins.

Validation from the repository root:

```bash
PYTHONPATH=plugins/clarp python3 -m unittest discover -s plugins/clarp/tests
```
