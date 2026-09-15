# Optional Hotseat plugins

Hotseat discovers installed packages through the `hotseat.plugins` Python entry
point group. Plugins must be installed into the Python environment used by
Hotseat. Core installation does not include any plugin distribution.

A plugin entry point returns a class with `api_version = 1`. Supported hooks:

- `register_commands(add, nodes)`: add CLI commands; `nodes` contains core command
  parsers so a plugin can extend a command's handler deliberately.
- `snapshot(accounts)`: return public dashboard data, namespaced under
  `snapshot.extensions[entry_point_name]`.
- `inspect(identifier)`: return a detail object or `None` to let native inspection
  continue.
- `dashboard_script()`: return the plugin's packaged JavaScript. Register a
  renderer in `window.hotseatPlugins`; it receives the full snapshot and returns
  HTML. Escape data before rendering it. Existing dashboard styling is available.

Plugins are local installed Python code, not downloaded by the dashboard.
Incompatible or broken imports produce a warning and are skipped. Snapshot errors
remain in the plugin's namespace. `HOTSEAT_PLUGINS=none` disables discovery without
loading plugin code. Changes to installed packages take effect on restart.

See [the Clarp plugin](../plugins/clarp/README.md) for a complete implementation.
