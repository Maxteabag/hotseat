package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Maxteabag/hotseat/internal/codex"
)

// cmdCodex shows Codex accounts and their quota, alongside the Claude ones.
// statusWidth is the STATUS column; the widest word it must hold is a block
// reason such as "no credits".
const statusWidth = 10

// codexRowName is how an account is listed: its profile name, or a label when
// the credential is live but saved nowhere.
func codexRowName(entry codex.Account) string {
	if !entry.Saved {
		return "live (unsaved)"
	}
	return entry.Alias
}

func (a *app) cmdCodex(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	noQuota := fs.Bool("no-quota", false, "Skip the live quota probe, which is the slow part")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	overview, err := a.codexOverview(ctx, !*noQuota)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(overview)
	}

	entries := overview.Accounts
	if len(entries) == 0 {
		a.println("No Codex accounts found.")
		return 1, nil
	}

	// Width from the data: a fixed 13 silently misaligned every row whose profile
	// name was longer, which is most of them once names get descriptive.
	nameWidth := len("PROFILE")
	for _, entry := range entries {
		if n := runeLen(codexRowName(entry)); n > nameWidth {
			nameWidth = n
		}
	}
	a.printf(" %s %s  %s %s  ACCOUNT\n", leftPad("PROFILE", nameWidth), rightPad("WORST", 6), leftPad("STATUS", statusWidth), rightPad("TOKEN", 9))
	for _, entry := range entries {
		usage := entry.Usage
		used := "—"
		if usage != nil && usage.WorstUsed != nil {
			used = fmt.Sprintf("%d%%", pyRound(*usage.WorstUsed*100))
		}
		var word, colour string
		switch {
		case usage != nil && usage.Error != nil && *usage.Error != "":
			word, colour = "error", yellow
		case usage == nil:
			word, colour = "unknown", dim
		case usage.Usable:
			word, colour = "usable", green
		case usage.NoResetReason != "":
			// Blocked by something the reset does not clear; naming it stops the
			// reset time from reading as "back at that time".
			word, colour = usage.NoResetReason, red
		default:
			word, colour = "blocked", red
		}

		token := "—"
		if hours := entry.TokenHoursLeft; hours != nil {
			if *hours <= 0 {
				// A saved profile refreshes on activation, so an expired stored
				// token is normal rather than a problem to flag.
				token = "expired"
			} else {
				token = fmt.Sprintf("%.1fh", *hours)
			}
		}

		marker := " "
		if entry.IsActive {
			marker = "*"
		}
		// The status word is coloured, so it is padded here rather than by leftPad,
		// which would count the escape sequence. A word wider than the column
		// simply gets no padding; it must never ask for a negative Repeat.
		a.printf("%s%s %s  %s%s %s  %s\n", marker, leftPad(codexRowName(entry), nameWidth), rightPad(used, 6), a.paint(word, colour),
			strings.Repeat(" ", max(0, statusWidth-runeLen(word))), rightPad(token, 9), deref(entry.Email, "?"))
	}

	a.println()
	if overview.UnsavedLive != nil {
		a.println(a.paint(fmt.Sprintf("! The live account (%s) is not saved as a profile. Switching profiles would lose it.",
			*overview.UnsavedLive), yellow))
		a.println(a.paint("  Save it first: hotseat codex-account save <name>", dim))
	}
	if !overview.QuotaRead {
		a.println(a.paint("Quota could not be read; showing accounts only.", dim))
	}
	if age := overview.QuotaAgeS; age != nil && *age > 60 {
		a.println(a.paint(fmt.Sprintf("Quota figures are %.0f minutes old. Each refresh starts one process per account, "+
			"so they are not re-read on a timer.", *age/60), dim))
	}
	return 0, nil
}

// cmdSessions lists recent Codex sessions, and what each one needs.
func (a *app) cmdSessions(cmd *command, _ context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	limit := fs.Int("limit", codex.DefaultSessionLimit, "")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	found, err := a.sessions().Recent(*limit)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if *asJSON {
		if found == nil {
			found = []codex.Session{}
		}
		return 0, a.emit(found)
	}
	if len(found) == 0 {
		a.println("No recent Codex sessions.")
		return 0, nil
	}

	tone := map[string]string{"working": green, "stuck": red, "failed": yellow, "idle": dim, "closed": dim}
	a.printf("%s%s%s%s%s  TOPIC\n", leftPad("ID", 10), leftPad("STATE", 9), leftPad("LAST", 14), rightPad("TURNS", 6), rightPad("FAILED", 7))
	var stuck []codex.Session
	for _, entry := range found {
		when := epochTime(entry.LastAt).Format("02 Jan 15:04")
		colour, ok := tone[entry.State]
		if !ok {
			colour = dim
		}
		state := a.paint(entry.State, colour)
		pad := runeLen(state) - runeLen(entry.State)
		a.printf("%s%s%s%s%s  %s\n", leftPad(entry.Short, 10), leftPad(state, 9+pad), leftPad(when, 14),
			rightPad(strconv.Itoa(entry.Turns), 6), rightPad(strconv.Itoa(entry.Failed), 7), truncateRunes(entry.Topic, 52))
		if entry.State == "stuck" {
			stuck = append(stuck, entry)
		}
	}

	a.println()
	if len(stuck) > 0 {
		a.println(a.paint(fmt.Sprintf("%d stuck: the holder keeps failing and nothing else can take the conversation.", len(stuck)), yellow))
		a.printf("  Reboot one: %s\n", a.paint("hotseat reboot "+stuck[0].Short, bold))
	}
	a.println(a.paint(`Nudge a live one: hotseat nudge <id> "continue"`, dim))
	return 0, nil
}

// cmdNudge hands a message to a running session without opening a terminal.
func (a *app) cmdNudge(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	positional, err := a.parse(cmd, fs, args, 2, 2, "id", "message")
	if err != nil {
		return 2, err
	}
	id, message := positional[0], positional[1]
	store := a.sessions()
	entry, err := store.Resolve(id)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if !entry.Live {
		fmt.Fprintf(a.stderr, "✗ %s is not running, so it cannot accept a message. Reboot it instead: hotseat reboot %s\n",
			entry.Short, entry.Short)
		return 1, nil
	}
	result, err := store.Nudge(ctx, entry.ThreadID, message)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(result)
	}
	a.println(a.paint(fmt.Sprintf("✓ queued for %s: %s", entry.Short, message), green))
	return 0, nil
}

// cmdReboot closes a stuck session and resumes the same conversation.
func (a *app) cmdReboot(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	message := fs.String("message", "", "Send this once it has attached")
	cwd := fs.String("cwd", "", "Directory to resume in")
	yes := fs.Bool("yes", false, "Skip the confirmation")
	positional, err := a.parse(cmd, fs, args, 1, 1, "id")
	if err != nil {
		return 2, err
	}
	store := a.sessions()
	entry, err := store.Resolve(positional[0])
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}

	if entry.Live && !*yes {
		a.printf("%s is held by process(es) %s.\n", entry.Short, joinInts(entry.Holders))
		a.println("Rebooting closes that session. Its history is kept; its scrollback is not.")
		if !a.stdinIsTTY() {
			a.errorln("Refusing without --yes.")
			return 1, nil
		}
		if !a.confirm("Close and resume? [y/N] ") {
			a.println("Cancelled.")
			return 1, nil
		}
	}

	closed := []int{}
	if entry.Live {
		closed, err = store.Release(entry.ThreadID, nil)
		if err != nil {
			a.errorln("✗ " + err.Error())
			return 1, nil
		}
		if closed == nil {
			closed = []int{}
		}
		a.waitForLock(entry.ThreadID, 10*time.Second)
	}
	result, err := a.launchCommand([]string{"codex", "resume", entry.ThreadID}, *cwd)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}

	if *asJSON {
		if err := a.emit(map[string]any{"thread_id": entry.ThreadID, "closed": closed,
			"terminal": result.Terminal, "started": result.Started}); err != nil {
			return 1, err
		}
	} else {
		if len(closed) > 0 {
			a.printf("Closed %s.\n", joinInts(closed))
		}
		a.println(a.paint(fmt.Sprintf("✓ resumed %s in a new %s window", entry.Short, result.Terminal), green))
		if *message != "" {
			a.println(a.paint("  waiting for it to attach before sending the message…", dim))
		}
	}
	if *message != "" {
		return a.sendAfterAttach(ctx, entry, *message, 45*time.Second), nil
	}
	return 0, nil
}

// waitForLock waits for the writer lock to clear, so the resume is not refused.
func (a *app) waitForLock(threadID string, wait time.Duration) bool {
	deadline := a.now().Add(wait)
	for a.now().Before(deadline) {
		if len(a.sessions().LockHolders(threadID)) == 0 {
			return true
		}
		a.sleep(500 * time.Millisecond)
	}
	return false
}

// sendAfterAttach queues a message once the resumed session has taken the lock.
func (a *app) sendAfterAttach(ctx context.Context, entry *codex.Session, message string, wait time.Duration) int {
	deadline := a.now().Add(wait)
	for a.now().Before(deadline) {
		if len(a.sessions().LockHolders(entry.ThreadID)) > 0 {
			if _, err := a.sessions().Nudge(ctx, entry.ThreadID, message); err != nil {
				a.errorln("✗ resumed, but the message did not send: " + err.Error())
				return 1
			}
			a.println(a.paint("✓ sent: "+message, green))
			return 0
		}
		a.sleep(time.Second)
	}
	a.errorln("✗ resumed, but it did not attach in time to accept the message.")
	return 1
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ", ")
}

// cmdResets reads banked Codex usage resets; it never redeems them.
func (a *app) cmdResets(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	aliases, err := a.parse(cmd, fs, args, 0, -1)
	if err != nil {
		return 2, err
	}
	rows, err := a.balances(ctx, aliases)
	if err != nil {
		a.errorln(err.Error())
		return 1, nil
	}
	if rows == nil {
		rows = []codex.Balance{}
	}
	failed := false
	for _, row := range rows {
		if row.Error != nil && *row.Error != "" {
			failed = true
		}
	}
	if *asJSON {
		if err := a.emit(map[string]any{"accounts": rows}); err != nil {
			return 1, err
		}
	} else {
		a.printf("%s %s  STATUS\n", leftPad("PROFILE", 26), rightPad("RESETS", 6))
		for _, row := range rows {
			count := "—"
			if row.AvailableCount != nil {
				count = strconv.Itoa(*row.AvailableCount)
			}
			a.printf("%s %s  %s\n", leftPad(row.Alias, 26), rightPad(count, 6), deref(row.Error, "ok"))
		}
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

// --- codex-account ----------------------------------------------------------

// codexAccountRequest is one parsed `hotseat codex-account` invocation, the
// argument shape of the Python codex_accounts.main.
type codexAccountRequest struct {
	Command string
	Name    string
	Email   string
	Browser bool
	Force   bool
	JSON    bool
}

var codexAccountCommands = []string{"save", "list", "switch", "current", "login", "verify", "probe", "remove"}

const codexAccountUsage = "usage: hotseat codex-account [-h]\n" +
	"                             {save,list,switch,current,login,verify,probe,remove} ...\n"

var errCodexAccountHelp = errors.New("help")

// parseCodexAccount mirrors the argparse tree of codex_accounts.main.
func parseCodexAccount(args []string) (codexAccountRequest, error) {
	fail := func(prog, msg string) (codexAccountRequest, error) {
		return codexAccountRequest{}, &usageError{prog, msg}
	}
	if len(args) == 0 {
		return fail("hotseat codex-account", "the following arguments are required: cmd")
	}
	if args[0] == "-h" || args[0] == "--help" {
		return codexAccountRequest{}, errCodexAccountHelp
	}
	request := codexAccountRequest{Command: args[0]}
	known := false
	for _, name := range codexAccountCommands {
		known = known || name == request.Command
	}
	if !known {
		return fail("hotseat codex-account", fmt.Sprintf("argument cmd: invalid choice: '%s' (choose from '%s')",
			request.Command, strings.Join(codexAccountCommands, "', '")))
	}
	prog := "hotseat codex-account " + request.Command
	var positional []string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		switch {
		case arg == "-h" || arg == "--help":
			return codexAccountRequest{}, errCodexAccountHelp
		case request.Command == "login" && (arg == "--email" || strings.HasPrefix(arg, "--email=")):
			if value, ok := strings.CutPrefix(arg, "--email="); ok {
				request.Email = value
			} else if i+1 < len(rest) {
				i++
				request.Email = rest[i]
			} else {
				return fail(prog, "argument --email: expected one argument")
			}
		case request.Command == "login" && arg == "--browser":
			request.Browser = true
		case request.Command == "login" && (arg == "--force" || arg == "-f"):
			request.Force = true
		case request.Command == "probe" && arg == "--json":
			request.JSON = true
		case strings.HasPrefix(arg, "-") && arg != "-":
			return fail("hotseat codex-account", "unrecognized arguments: "+arg)
		default:
			positional = append(positional, arg)
		}
	}
	switch request.Command {
	case "list", "current":
		if len(positional) > 0 {
			return fail("hotseat codex-account", "unrecognized arguments: "+strings.Join(positional, " "))
		}
	case "probe":
		if len(positional) > 1 {
			return fail("hotseat codex-account", "unrecognized arguments: "+strings.Join(positional[1:], " "))
		}
		if len(positional) == 1 {
			request.Name = positional[0]
		}
	default:
		if len(positional) == 0 {
			return fail(prog, "the following arguments are required: name")
		}
		if len(positional) > 1 {
			return fail("hotseat codex-account", "unrecognized arguments: "+strings.Join(positional[1:], " "))
		}
		request.Name = positional[0]
		if request.Command == "login" && request.Email == "" {
			return fail(prog, "the following arguments are required: --email")
		}
	}
	return request, nil
}

// run dispatches the request to the profile manager in-process.
func (r codexAccountRequest) run(ctx context.Context, profiles *codex.Profiles) error {
	switch r.Command {
	case "save":
		return profiles.Save(r.Name)
	case "list":
		return profiles.List()
	case "switch":
		return profiles.Switch(r.Name)
	case "current":
		return profiles.ShowCurrent()
	case "login":
		return profiles.Login(ctx, r.Name, codex.LoginOptions{Email: r.Email, Browser: r.Browser, Force: r.Force})
	case "verify":
		return profiles.Verify(ctx, r.Name)
	case "probe":
		return profiles.Probe(ctx, r.Name, r.JSON)
	case "remove":
		return profiles.Remove(r.Name)
	}
	return fmt.Errorf("unknown codex-account command %q", r.Command)
}

// cmdCodexAccount runs the Codex profile helper in-process.
func (a *app) cmdCodexAccount(cmd *command, ctx context.Context, args []string) (int, error) {
	request, err := parseCodexAccount(args)
	if errors.Is(err, errCodexAccountHelp) {
		fmt.Fprint(a.stdout, codexAccountUsage)
		a.println()
		a.println("Save, list, and switch Codex CLI accounts.")
		a.println()
		a.println("positional arguments:")
		a.println("  {save,list,switch,current,login,verify,probe,remove}")
		a.println("    save                snapshot the live login under a name")
		a.println("    list                list saved profiles")
		a.println("    switch              activate a saved profile")
		a.println("    current             show the live account")
		a.println("    login               add a new account via browser OAuth or device code without switching")
		a.println("    verify              check a saved profile locally without switching")
		a.println("    probe               test server token validity and live quota in an isolated environment")
		a.println("    remove              delete a saved profile")
		return 0, nil
	}
	var usage *usageError
	if errors.As(err, &usage) {
		a.usageFailure(codexAccountUsage, usage.prog, usage.msg)
		return 2, nil
	}
	if err != nil {
		return 1, err
	}
	if err := a.codexAccount(ctx, request); err != nil {
		// The Python helper's _fail: "error: <message>" on stderr, exit 1.
		fmt.Fprintf(a.stderr, "error: %s\n", err.Error())
		return 1, nil
	}
	return 0, nil
}
