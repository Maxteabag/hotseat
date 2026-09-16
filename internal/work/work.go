package work

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// WorkError means a work listing, detail or action could not be completed. The
// message is shown to users as-is.
type WorkError struct{ Msg string }

func (e *WorkError) Error() string { return e.Msg }

func workErr(msg string) error { return &WorkError{Msg: msg} }

// Process is the exact identity of one native CLI process: its pid, the kernel
// start time that makes the pid unambiguous, its argv, and whether it is a shared
// host (an app-server or a stream-json bridge) rather than an interactive owner.
type Process struct {
	PID    int      `json:"pid"`
	Start  string   `json:"start"`
	Argv   []string `json:"argv"`
	Shared bool     `json:"shared"`
}

// CodexRow mirrors one row of codexsessions.recent(): thread history plus who
// holds its writer lock. LastAt is MAX(started_at) in epoch seconds; Codex
// stores that column as INTEGER, so an integral value is hashed into the revision
// as an integer, which is what Python's sqlite3 hands json.dumps.
type CodexRow struct {
	ThreadID   string
	Short      string
	LastAt     float64
	Turns      int
	Failed     int
	Active     int
	LastStatus string
	Topic      string
	Holders    []int
	Live       bool
	State      string
}

// CodexSessions is the part of the codex package this package needs.
type CodexSessions interface {
	// Recent lists the most recently active threads, newest first.
	Recent(ctx context.Context, limit int) ([]CodexRow, error)
	// LockHolders lists process ids holding the thread's writer lock.
	LockHolders(threadID string) ([]int, error)
	// ThreadItems returns the readable text of the last limit stored items of a
	// thread, oldest first; items without text may be empty strings.
	ThreadItems(threadID string, limit int) ([]string, error)
}

// Launcher opens a command in a new terminal window in the given directory.
type Launcher interface {
	LaunchCommand(argv []string, cwd string) error
}

// CodexMeta is what Codex's state database knows about a thread.
type CodexMeta struct {
	CWD   string
	Title string
}

// Item is one row of the work listing.
type Item struct {
	ID        string  `json:"id"`
	Provider  string  `json:"provider"`
	Title     string  `json:"title"`
	State     string  `json:"state"`
	Updated   float64 `json:"updated"`
	CWD       string  `json:"cwd"`
	Live      bool    `json:"live"`
	CanResume bool    `json:"can_resume"`
	CanReboot bool    `json:"can_reboot"`
	Reason    string  `json:"reason"`
	Revision  string  `json:"revision"`
	// updatedInteger records that Updated came from an integer column, so the
	// revision hashes it as Python would.
	updatedInteger bool
}

// Listing is the work list plus the errors met while building it.
type Listing struct {
	Items  []Item   `json:"items"`
	Errors []string `json:"errors"`
}

// ItemDetail is an Item with what the conversation was about.
type ItemDetail struct {
	Item
	LastUser      string `json:"last_user"`
	LastAssistant string `json:"last_assistant"`
	Model         string `json:"model"`
}

// ActResult reports a resume or reboot.
type ActResult struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Operation string `json:"operation"`
	Launched  bool   `json:"launched"`
}

// Service reads stopped work and explicitly resumes it. The zero value is not
// usable; use New.
type Service struct {
	// ProjectsDir holds Claude Code's native transcripts.
	ProjectsDir string
	// CodexHome holds Codex's state_*.sqlite databases.
	CodexHome string
	// ProcRoot is /proc, or a fake tree in tests.
	ProcRoot string
	// Codex is nil when no Codex history is readable here.
	Codex CodexSessions
	// Launcher opens the replacement session; nil refuses to launch.
	Launcher Launcher
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// Test seams; nil means the real thing.
	resolveHook       func(ctx context.Context, provider, identifier string) (Item, error)
	identityHook      func(pid int, provider string) *Process
	claudeHoldersHook func(identifier string) []Process
	killHook          func(pid int) error
	selfPID           func() int
	sleep             func(time.Duration)
}

// New is a Service against the real machine: CLAUDE_CONFIG_DIR or ~/.claude for
// transcripts, CODEX_HOME or ~/.codex for Codex state, and /proc.
func New(codex CodexSessions, launcher Launcher) *Service {
	return &Service{
		ProjectsDir: DefaultProjectsDir(),
		CodexHome:   DefaultCodexHome(),
		ProcRoot:    "/proc",
		Codex:       codex,
		Launcher:    launcher,
	}
}

// DefaultCodexHome is CODEX_HOME, or ~/.codex.
func DefaultCodexHome() string {
	if override := os.Getenv("CODEX_HOME"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".codex")
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) procRoot() string {
	if s.ProcRoot == "" {
		return "/proc"
	}
	return s.ProcRoot
}

// --- process identity ------------------------------------------------------

// ProcessIdentity is the exact native CLI identity of a process plus its kernel
// start time; no substring matching. It is nil for processes of other users,
// processes that are not the provider's CLI, and processes that vanished.
func (s *Service) ProcessIdentity(pid int, provider string) *Process {
	if s.identityHook != nil {
		return s.identityHook(pid, provider)
	}
	return processIdentity(s.procRoot(), pid, provider)
}

func processIdentity(procRoot string, pid int, provider string) *Process {
	root := filepath.Join(procRoot, strconv.Itoa(pid))
	info, err := os.Stat(root)
	if err != nil || !ownedByCaller(info) {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(root, "cmdline"))
	if err != nil {
		return nil
	}
	var argv []string
	for _, part := range strings.Split(string(raw), "\x00") {
		if part != "" {
			argv = append(argv, strings.ToValidUTF8(part, "�"))
		}
	}
	if len(argv) == 0 {
		return nil
	}
	names := []string{filepath.Base(argv[0])}
	if (names[0] == "node" || names[0] == "nodejs") && len(argv) > 1 {
		names = append(names, filepath.Base(argv[1]))
	}
	if !slices.Contains(names, provider) && !isClaudeCodeScript(provider, names, argv) {
		return nil
	}
	fields, err := statFields(root)
	if err != nil || len(fields) <= 19 {
		return nil
	}
	return &Process{
		PID:    pid,
		Start:  fields[19],
		Argv:   argv,
		Shared: slices.Contains(argv, "app-server") || (slices.Contains(argv, "--input-format") && slices.Contains(argv, "stream-json")),
	}
}

// isClaudeCodeScript recognises Claude Code run through node as
// .../@anthropic-ai/claude-code/cli.js rather than the claude wrapper.
func isClaudeCodeScript(provider string, names, argv []string) bool {
	if provider != "claude" {
		return false
	}
	script := false
	for _, n := range names {
		if n == "claude.js" || n == "cli.js" {
			script = true
		}
	}
	if !script {
		return false
	}
	for _, a := range argv[:min(2, len(argv))] {
		if strings.Contains(a, "@anthropic-ai/claude-code") {
			return true
		}
	}
	return false
}

// statFields are the fields of /proc/<pid>/stat after the parenthesised command
// name, so a ')' inside the name cannot shift them.
func statFields(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return nil, err
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return nil, errors.New("malformed stat")
	}
	return strings.Fields(text[end+1:]), nil
}

// ProcessRunning says whether the very process described by owner is still
// alive: same pid, same start time, not a zombie.
func (s *Service) ProcessRunning(owner Process) (bool, error) {
	fields, err := statFields(filepath.Join(s.procRoot(), strconv.Itoa(owner.PID)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, &WorkError{Msg: "Cannot verify process exit"}
	}
	if len(fields) <= 19 {
		return false, &WorkError{Msg: "Cannot verify process exit"}
	}
	return fields[19] == owner.Start && fields[0] != "Z" && fields[0] != "X", nil
}

// ClaudeProcesses lists every identified claude CLI process of this user.
func (s *Service) ClaudeProcesses() []Process {
	entries, err := os.ReadDir(s.procRoot())
	if err != nil {
		return nil
	}
	var found []Process
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !isDigits(entry.Name()) {
			continue
		}
		if item := s.ProcessIdentity(pid, "claude"); item != nil {
			found = append(found, *item)
		}
	}
	return found
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ClaudeHolders are the processes resuming or owning exactly this session id:
// --resume, -r or --session-id followed by the id itself.
func ClaudeHolders(identifier string, processes []Process) []Process {
	var holders []Process
	for _, item := range processes {
		for i, arg := range item.Argv {
			if (arg == "--resume" || arg == "-r" || arg == "--session-id") && i+1 < len(item.Argv) && item.Argv[i+1] == identifier {
				holders = append(holders, item)
				break
			}
		}
	}
	return holders
}

func (s *Service) claudeHolders(identifier string) []Process {
	if s.claudeHoldersHook != nil {
		return s.claudeHoldersHook(identifier)
	}
	return ClaudeHolders(identifier, s.ClaudeProcesses())
}

// --- codex metadata --------------------------------------------------------

// CodexMetadata looks up thread working directories and titles in Codex's state
// databases, newest first, returning the first database that knows any of them.
func (s *Service) CodexMetadata(ctx context.Context, identifiers []string) map[string]CodexMeta {
	if len(identifiers) == 0 {
		return map[string]CodexMeta{}
	}
	paths, _ := filepath.Glob(filepath.Join(s.CodexHome, "state_*.sqlite"))
	type candidate struct {
		path  string
		mtime time.Time
	}
	var candidates []candidate
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{path, info.ModTime()})
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return b.mtime.Compare(a.mtime) })
	for _, c := range candidates {
		found, err := queryThreads(ctx, c.path, identifiers)
		if err != nil {
			continue
		}
		if len(found) > 0 {
			return found
		}
	}
	return map[string]CodexMeta{}
}

func queryThreads(ctx context.Context, path string, identifiers []string) (map[string]CodexMeta, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_time_format=sqlite&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(identifiers)), ",")
	args := make([]any, len(identifiers))
	for i, id := range identifiers {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, "SELECT id,cwd,title FROM threads WHERE id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]CodexMeta{}
	for rows.Next() {
		var id string
		var cwd, title sql.NullString
		if err := rows.Scan(&id, &cwd, &title); err != nil {
			return nil, err
		}
		found[id] = CodexMeta{CWD: cwd.String, Title: title.String}
	}
	return found, rows.Err()
}

// --- revision --------------------------------------------------------------

// Revision fingerprints an item together with the processes that own it, so an
// action confirmed against one state is refused once anything about it moved.
// The digest is byte-identical to the Python implementation's.
func Revision(item Item, holders []Process) string {
	pairs := make([][2]any, 0, len(holders))
	for _, p := range holders {
		pairs = append(pairs, [2]any{p.PID, p.Start})
	}
	slices.SortStableFunc(pairs, func(a, b [2]any) int {
		if c := a[0].(int) - b[0].(int); c != 0 {
			return c
		}
		return strings.Compare(a[1].(string), b[1].(string))
	})
	owners := make([]any, 0, len(pairs))
	for _, pair := range pairs {
		owners = append(owners, []any{pair[0], pair[1]})
	}
	var updated any = item.Updated
	if item.updatedInteger {
		updated = int64(item.Updated)
	}
	material := []any{item.ID, item.Provider, item.State, updated, item.CWD, owners}
	sum := sha256.Sum256([]byte(pyDumps(material)))
	return hex.EncodeToString(sum[:])
}

// --- listing ---------------------------------------------------------------

// Listing is the recent Codex threads and native Claude transcripts, failed and
// stuck ones first, then by recency. limit applies to each provider separately
// (design decision 11).
func (s *Service) Listing(ctx context.Context, limit int) Listing {
	items := []Item{}
	errs := []string{}
	var entries []CodexRow
	if s.Codex != nil {
		rows, err := s.Codex.Recent(ctx, limit)
		if err != nil {
			errs = append(errs, err.Error())
		} else {
			entries = rows
		}
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ThreadID)
	}
	directories := s.CodexMetadata(ctx, ids)
	for _, entry := range entries {
		var holders []Process
		for _, pid := range entry.Holders {
			if p := s.ProcessIdentity(pid, "codex"); p != nil {
				holders = append(holders, *p)
			}
		}
		shared := anyShared(holders)
		unknown := len(holders) != len(entry.Holders)
		meta := directories[entry.ThreadID]
		title := meta.Title
		if title == "" {
			title = entry.Topic
		}
		reason := ""
		if shared {
			reason = "Managed by a shared app-server; use its host integration"
		} else if unknown {
			reason = "Cannot identify the exact owning process"
		}
		item := Item{
			ID: entry.ThreadID, Provider: "codex", Title: title, State: entry.State,
			Updated: entry.LastAt, CWD: meta.CWD, Live: entry.Live,
			CanResume: !entry.Live, CanReboot: entry.Live && !shared && !unknown,
			Reason:         reason,
			updatedInteger: entry.LastAt == math.Trunc(entry.LastAt),
		}
		item.Revision = Revision(item, holders)
		items = append(items, item)
	}

	// Native Claude transcripts are local and include the original working directory.
	type candidate struct {
		path    string
		updated float64
	}
	var candidates []candidate
	for _, path := range transcripts(s.ProjectsDir) {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{path, epochSeconds(info.ModTime())})
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return compareFloatDesc(a.updated, b.updated) })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	var nativeProcesses []Process
	if s.claudeHoldersHook == nil {
		nativeProcesses = s.ClaudeProcesses()
	}
	for _, c := range candidates {
		var tail []Entry
		for _, e := range LastEntries(c.path, tailKeep) {
			if e != nil {
				tail = append(tail, e)
			}
		}
		last := lastTurn(tail)
		if last == nil {
			continue
		}
		id := stem(c.path)
		var holders []Process
		if s.claudeHoldersHook != nil {
			holders = s.claudeHoldersHook(id)
		} else {
			holders = ClaudeHolders(id, nativeProcesses)
		}
		failed := truthy(last["isApiErrorMessage"])
		live := len(holders) > 0
		state := "recorded"
		switch {
		case failed && live:
			state = "stuck"
		case failed:
			state = "failed"
		case live:
			state = "running"
		}
		title := id
		for i := len(tail) - 1; i >= 0; i-- {
			if tail[i].str("type") != "user" {
				continue
			}
			if text := EntryText(tail[i]); !strings.HasPrefix(text, "<") && !strings.HasPrefix(text, "# AGENTS") {
				title = text
				break
			}
		}
		cwd := last.str("cwd")
		if cwd == "" {
			for i := len(tail) - 1; i >= 0; i-- {
				if truthy(tail[i]["cwd"]) {
					cwd = tail[i].str("cwd")
					break
				}
			}
		}
		shared := anyShared(holders)
		reason := ""
		switch {
		case shared:
			reason = "Managed streaming process; use its host integration"
		case live:
			reason = ""
		default:
			reason = "Reboot requires an exact --resume/--session-id process match"
		}
		item := Item{
			ID: id, Provider: "claude", Title: truncateRunes(title, 160), State: state,
			Updated: c.updated, CWD: cwd, Live: live, CanResume: !live,
			CanReboot: live && !shared, Reason: reason,
		}
		item.Revision = Revision(item, holders)
		items = append(items, item)
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		aSettled, bSettled := a.State != "failed" && a.State != "stuck", b.State != "failed" && b.State != "stuck"
		if aSettled != bSettled {
			if !aSettled {
				return -1
			}
			return 1
		}
		return compareFloatDesc(a.Updated, b.Updated)
	})
	return Listing{Items: items, Errors: errs}
}

func anyShared(processes []Process) bool {
	for _, p := range processes {
		if p.Shared {
			return true
		}
	}
	return false
}

func truncateRunes(s string, limit int) string {
	if runes := []rune(s); len(runes) > limit {
		return string(runes[:limit])
	}
	return s
}

// Resolve finds exactly one listed item.
func (s *Service) Resolve(ctx context.Context, provider, identifier string) (Item, error) {
	if s.resolveHook != nil {
		return s.resolveHook(ctx, provider, identifier)
	}
	var found []Item
	for _, item := range s.Listing(ctx, 100).Items {
		if item.Provider == provider && item.ID == identifier {
			found = append(found, item)
		}
	}
	if len(found) != 1 {
		return Item{}, workErr("Session is missing or ambiguous; refresh the work list")
	}
	return found[0], nil
}

// Detail is one listed item with what the conversation was about: the native
// transcript's last prompt and reply, or Codex's title and last few messages.
func (s *Service) Detail(ctx context.Context, provider, identifier string) (ItemDetail, error) {
	item, err := s.Resolve(ctx, provider, identifier)
	if err != nil {
		return ItemDetail{}, err
	}
	detail := ItemDetail{Item: item}
	if provider == "claude" {
		result, err := NativeDetail(s.ProjectsDir, identifier)
		if err != nil {
			return ItemDetail{}, err
		}
		if result != nil {
			detail.LastUser = result.LastUser
			detail.LastAssistant = result.LastAssistant
			if result.Model != nil {
				detail.Model = *result.Model
			}
		}
		return detail, nil
	}
	if s.Codex == nil {
		return ItemDetail{}, workErr("No Codex thread history is available")
	}
	texts, err := s.Codex.ThreadItems(identifier, 40)
	if err != nil {
		return ItemDetail{}, workErr(err.Error())
	}
	var messages []string
	for _, text := range texts {
		if text != "" {
			messages = append(messages, text)
		}
	}
	if len(messages) > 6 {
		messages = messages[len(messages)-6:]
	}
	joined := []rune(strings.Join(messages, "\n\n"))
	if len(joined) > 6000 {
		joined = joined[len(joined)-6000:]
	}
	detail.LastUser = item.Title
	detail.LastAssistant = string(joined)
	return detail, nil
}

// --- acting ----------------------------------------------------------------

// Act resumes or reboots one session after every check that keeps it from
// touching the wrong process: the caller acknowledged, the item is unchanged
// since they saw it, the action is available, the working directory still
// exists; for a reboot the owning processes are re-identified, must all be
// interactive and known, must not include this process's own ancestors, and are
// re-checked one more time before SIGTERM; then the old process must be gone
// (and Codex's lock released) before the replacement is launched.
func (s *Service) Act(ctx context.Context, provider, identifier, operation, expectedRevision string, acknowledged bool) (ActResult, error) {
	if operation != "resume" && operation != "reboot" {
		return ActResult{}, workErr("Unsupported work action")
	}
	if !acknowledged {
		return ActResult{}, workErr("Confirmation is required")
	}
	item, err := s.Resolve(ctx, provider, identifier)
	if err != nil {
		return ActResult{}, err
	}
	if item.Revision != expectedRevision {
		return ActResult{}, workErr("Session changed; refresh and confirm again")
	}
	allowed := item.CanResume
	if operation == "reboot" {
		allowed = item.CanReboot
	}
	if !allowed {
		if item.Reason != "" {
			return ActResult{}, workErr(item.Reason)
		}
		return ActResult{}, workErr("This action is not available for the current session state")
	}
	cwd := item.CWD
	if info, err := os.Stat(cwd); cwd == "" || err != nil || !info.IsDir() {
		return ActResult{}, workErr("Original working directory is missing; no session was changed")
	}
	if operation == "reboot" {
		if err := s.reboot(ctx, provider, identifier, item, expectedRevision); err != nil {
			return ActResult{}, err
		}
	}
	if operation == "resume" {
		var owned bool
		if provider == "codex" {
			pids, err := s.lockHolders(identifier)
			if err != nil {
				return ActResult{}, err
			}
			owned = len(pids) > 0
		} else {
			owned = len(s.claudeHolders(identifier)) > 0
		}
		if owned {
			return ActResult{}, workErr("Session acquired an owner; refresh before resuming")
		}
	}
	command := []string{"claude", "--resume", identifier}
	if provider == "codex" {
		command = []string{"codex", "resume", identifier}
	}
	if s.Launcher == nil {
		return ActResult{}, workErr("No launcher is available; no session was started")
	}
	if err := s.Launcher.LaunchCommand(command, cwd); err != nil {
		return ActResult{}, err
	}
	return ActResult{ID: identifier, Provider: provider, Operation: operation, Launched: true}, nil
}

func (s *Service) lockHolders(identifier string) ([]int, error) {
	if s.Codex == nil {
		return nil, nil
	}
	pids, err := s.Codex.LockHolders(identifier)
	if err != nil {
		return nil, workErr(err.Error())
	}
	return pids, nil
}

// reboot stops the exact owners of a session, then waits for them to be gone.
func (s *Service) reboot(ctx context.Context, provider, identifier string, item Item, expectedRevision string) error {
	var holders []Process
	if provider == "claude" {
		holders = s.claudeHolders(identifier)
	} else {
		pids, err := s.lockHolders(identifier)
		if err != nil {
			return err
		}
		for _, pid := range pids {
			p := s.ProcessIdentity(pid, "codex")
			if p == nil {
				return workErr("Owner changed; refusing to stop a shared or unknown process")
			}
			holders = append(holders, *p)
		}
	}
	if len(holders) == 0 || anyShared(holders) {
		return workErr("Owner changed; refusing to stop a shared or unknown process")
	}
	if Revision(item, holders) != expectedRevision {
		return workErr("Owning process changed; confirm again")
	}
	ancestors := s.ancestors()
	for _, p := range holders {
		if ancestors[p.PID] {
			return workErr("Refusing to stop the controller process")
		}
	}
	for _, p := range holders {
		current := s.ProcessIdentity(p.PID, provider)
		if current == nil || current.Start != p.Start {
			return workErr("Process identity changed before shutdown")
		}
		if err := s.kill(p.PID); err != nil {
			return workErr(err.Error())
		}
	}
	deadline := s.now().Add(10 * time.Second)
	exited := false
	for s.now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		alive := false
		for _, p := range holders {
			running, err := s.ProcessRunning(p)
			if err != nil {
				return err
			}
			if running {
				alive = true
				break
			}
		}
		locked := false
		if provider == "codex" {
			pids, err := s.lockHolders(identifier)
			if err != nil {
				return err
			}
			locked = len(pids) > 0
		}
		if !alive && !locked {
			exited = true
			break
		}
		s.pause(100 * time.Millisecond)
	}
	if !exited {
		return workErr("Session did not exit; no replacement was launched")
	}
	if provider == "codex" {
		pids, err := s.lockHolders(identifier)
		if err != nil {
			return err
		}
		if len(pids) > 0 {
			return workErr("Conversation lock is still held; no replacement was launched")
		}
	}
	return nil
}

// ancestors is this process and every parent up the tree, read from
// /proc/<pid>/status, so a reboot can never terminate its own controller.
func (s *Service) ancestors() map[int]bool {
	pid := os.Getpid()
	if s.selfPID != nil {
		pid = s.selfPID()
	}
	seen := map[int]bool{}
	for pid > 1 && !seen[pid] {
		seen[pid] = true
		parent, err := parentOf(s.procRoot(), pid)
		if err != nil {
			break
		}
		pid = parent
	}
	return seen
}

func parentOf(procRoot string, pid int) (int, error) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, errors.New("malformed status")
			}
			return strconv.Atoi(fields[1])
		}
	}
	return 0, errors.New("no PPid")
}

func (s *Service) kill(pid int) error {
	if s.killHook != nil {
		return s.killHook(pid)
	}
	return terminate(pid)
}

func (s *Service) pause(d time.Duration) {
	if s.sleep != nil {
		s.sleep(d)
		return
	}
	time.Sleep(d)
}
