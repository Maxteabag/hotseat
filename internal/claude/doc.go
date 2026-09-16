// Package claude knows where Claude Code keeps its state on disk and how to read
// it: credential discovery per platform (files on Linux, the Keychain on macOS),
// the Account shape shared by the CLI and the TUI, the per-account pinned
// session directories, the in-session status line, per-model token statistics
// and the count of live `claude` processes.
//
// Everything here reads local files or runs small local commands; nothing talks
// to the network. Tokens are held only long enough to probe quota and never
// leave the process through Account.Public.
package claude
