# Go port: design contract

Hotseat is a single Go binary; this documents how the Python original was ported. Every Python module under `hotseat/` is ported to
Go under `internal/`; the Bubble Tea TUI in `internal/tui` stops shelling out to
`python3 -m hotseat.tui_bridge` and calls Go functions. The Python package, the wheel
build, the binary download path, the plugin entry-point system, `hotseat serve` and
`web/` are deleted at the end. The Clarp integration is compiled in and activates
only when `~/.local/share/clarp/state.sqlite` exists.

Behaviour is preserved unless a line below says otherwise. When the Python and this
document disagree, the Python is the reference; when the Python is inconsistent,
list it here under "Decisions".

## Layout

| Package | Ports | Notes |
|---|---|---|
| `internal/claude` | `paths`, `backends/{base,linux,darwin,__init__}`, `sessiondirs`, `statusline`, `modelstats`, `sessions` | Credential discovery, `Account`, pinned session dirs. Darwin keychain behind `//go:build darwin` with the `security` CLI. |
| `internal/claude` (wave 2) | `refresh`, `actions` | Token refresh through the `claude` CLI, terminal detection, launch, switch, exec. |
| `internal/quota` | `usage`, `ratelimits`, `quota_cache` | Anthropic OAuth usage endpoint, header probe, per-token cooldown cache. |
| `internal/codex` | `codex`, `codex_accounts`, `compare_limits`, `resetcredits`, `codexsessions`, `codexlaunch` | Codex profiles, JWT claims, app-server JSON-RPC rate-limit client (in-process, no re-exec), reset credits, thread history SQLite, pinned launch. |
| `internal/work` | `textutil`, `resume`, `inspect`, `work` | Transcript readers, stopped-work listing, `/proc` process identity, resume/reboot safety chain. |
| `internal/clarp` | `plugins/clarp/hotseat_clarp/*` | Reads Clarp's state SQLite and `clarp-admin`. Uses `internal/work` for the shared native-transcript logic (do not duplicate it). |
| `internal/collect` | `collect`, `tui_bridge` | `Collector` (singleflight + errgroup), the bridge `Snapshot`/`GroupAccounts`/work operations returning the exact structs `internal/tui` consumes. |
| `internal/tui` | existing | `PythonBackend`, `FindRoot`, `process_unix.go` deleted; a `GoBackend` wraps `internal/collect`. |
| `cmd/hotseat` | `cli` | All subcommands, `--json` shapes preserved byte-for-byte where they are documented as scriptable. `hotseat tui` runs the TUI in-process. `cmd/hotseat-tui` is deleted. |

## Conventions

- Go 1.26, `CGO_ENABLED=0`. SQLite is `modernc.org/sqlite` (driver name `sqlite`), opened read-only with `file:<path>?mode=ro&_time_format=sqlite`. PTY is not needed.
- Errors: each package has one sentinel-ish type mirroring the Python exception (`quota.UsageError` with `RetryAfter`, `claude.BackendError`, `work.WorkError`, ...). Messages copy the Python strings; the TUI and CLI show them to users.
- No global mutable state except explicit in-memory caches guarded by a mutex. Never mutate `os.Environ`; build `exec.Cmd.Env` per command.
- Subprocesses: same argv as the Python, same timeouts, `context.WithTimeout`. Strip `BLOCKED_ENV` where the Python does.
- Files: same paths, same modes (0700 dirs, 0600 secrets), atomic writes = temp file in the same dir + `Sync` + `Chmod` + `Rename`.
- Times: the Python uses epoch seconds as `float`; keep `float64` seconds in JSON-facing structs and `time.Time` internally when convenient. Credential `expiresAt` fields are **milliseconds**.
- JSON: field tags match the Python keys exactly. Omit nothing the Python emits; the TUI and `--json` consumers depend on it. Where Python emits `null`, use pointers.
- Tests: `_test.go` in the same package, table tests ported from `tests/test_*.py`, fixtures copied from the Python tests (fake credential JSON, JSONL transcripts, usage payloads, header maps, SQLite builders). Tests never touch the network, the real home directory, or real `claude`/`codex` binaries: inject `run func(*exec.Cmd) error`-style hooks, `http.RoundTripper`s, and root directories. Tests that need `/proc` skip on non-Linux.
- `gofmt` clean, `go vet ./...` clean, `go test -race ./...` green. Do not add dependencies beyond those already in `go.mod` (bubbletea/bubbles/lipgloss, `modernc.org/sqlite`, `creack/pty` if actually needed, `golang.org/x/sync`, `golang.org/x/term`).
- Package doc comments carry over the Python module docstrings; they explain *why*, keep them.

## Decisions on ambiguous Python behaviour

1. `PROJECTS_DIR` honours `CLAUDE_CONFIG_DIR` (Python hardcoded `~/.claude/projects`; that was a bug).
2. `usage.summarise` divides utilization by 100 and `ratelimits.summarise` does not; both are right for their respective APIs. Keep.
3. The Codex limits in-memory cache stays in-memory; the TUI is the long-lived caller that benefits.
4. The `bengalfox`/`spark` profile note in `group_accounts` is kept verbatim.
5. `rate_limit_tier` is added to the TUI `Account` struct.
6. In-process calls return `error`; the `{"error": ...}` stdout convention is gone.
7. `resume.continue_item`'s plugin branch is dead; drop it.
8. `codex_accounts` keeps treating `login status` exit code 1 as "logged out".
9. `_sweep_old_launchers` only removes directories owned by the current uid.
10. `hotseat serve`, `web/`, `dashboard_script` and the Python plugin API are removed, not ported.
11. `work.listing` keeps the asymmetric per-provider limit; not worth changing.
12. The duplicated `resume.py` logic in the Clarp plugin is ported once, in `internal/work`.

## Verification against the Python

During the port a harness ran the Python bridge and the Go binary side by side on a
real machine, stripped volatile fields and diffed the rest. `snapshot`, `work` and
every scriptable `--json` subcommand matched; the only remaining differences were
the server's own unstable key order in Codex rate-limit windows and Go printing
integral floats without a trailing `.0`. The Python package was then removed.
