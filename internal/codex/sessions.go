package codex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// This file ports codexsessions: recent Codex sessions, and getting a stuck
// one moving again.
//
// Three mechanics decide what a session needs, and they are easy to confuse:
//
//   - A writer lock per thread. Only one process may hold a conversation. A
//     second `codex resume` of the same thread fails with "this conversation is
//     open in another app" rather than taking over.
//   - Credentials are read once, at startup. Switching the active Codex account
//     never reaches a session that is already running, so a session started
//     before the switch keeps failing on the old account no matter what is on
//     disk.
//   - A running session accepts a message. `codex queue` hands text to a live
//     session by thread id, without a terminal and without typing anything.
//
// So a live session is nudged, and a stale one is closed and resumed. Reading is
// always safe; both actions are explicit.

const (
	// SessionTimeout bounds `codex queue`.
	SessionTimeout = 60 * time.Second
	// DefaultSessionLimit is how many recent threads Recent lists by default.
	DefaultSessionLimit = 12
)

// failedStatuses mean the turn ended badly rather than finishing.
var failedStatuses = map[string]bool{"failed": true, "error": true, "aborted": true}
var runningStatuses = map[string]bool{"inprogress": true, "in_progress": true, "running": true, "started": true}

// SessionError mirrors the Python: Codex session history could not be read,
// or an action could not be run.
type SessionError struct{ Msg string }

func (e *SessionError) Error() string { return e.Msg }

// Signaller delivers a signal to a process id; Release takes one so tests never
// signal a real process.
type Signaller func(pid int, sig syscall.Signal) error

// Sessions reads the thread history under one CODEX_HOME.
type Sessions struct {
	Home string
	// Run runs `fuser` and `codex queue`; nil uses os/exec.
	Run Runner
	// Which locates executables; nil uses exec.LookPath.
	Which func(string) (string, error)

	// Test hooks; nil means the real implementation.
	holders        func(threadID string) []int
	parentOf       func(pid int) int
	looksLikeCodex func(pid int) bool
}

// NewSessions returns a reader rooted at home.
func NewSessions(home string) *Sessions { return &Sessions{Home: home} }

// HistoryDB is Codex's thread history. Timestamps in thread_turns are epoch
// seconds, not milliseconds.
func (s *Sessions) HistoryDB() string { return filepath.Join(s.Home, "thread_history_1.sqlite") }

// LockDir holds the per-thread writer locks.
func (s *Sessions) LockDir() string { return filepath.Join(s.Home, "thread-writer-locks") }

// Available reports whether a history database exists.
func (s *Sessions) Available() bool { return fileExists(s.HistoryDB()) }

func (s *Sessions) run(ctx context.Context, argv, env []string) (Result, error) {
	if s.Run != nil {
		return s.Run(ctx, argv, env)
	}
	return RunCommand(ctx, argv, env)
}

func (s *Sessions) which(name string) (string, error) {
	if s.Which != nil {
		return s.Which(name)
	}
	return exec.LookPath(name)
}

// walkText is the first value stored under a "text" key, at any depth.
//
// Only under that key: returning any string found would pick up the item type
// and other bookkeeping, which reads as a topic but is not one.
func walkText(value any) string {
	switch v := value.(type) {
	case *orderedObject:
		if candidate, ok := v.get("text").(string); ok && strings.TrimSpace(candidate) != "" {
			return candidate
		}
		for _, key := range v.keys {
			if found := walkText(v.values[key]); found != "" {
				return found
			}
		}
	case []any:
		for _, nested := range v {
			if found := walkText(nested); found != "" {
				return found
			}
		}
	}
	return ""
}

// firstText pulls readable text out of a stored item.
//
// Parsed as JSON rather than pattern-matched: the stored text is UTF-8 with JSON
// escapes, and decoding those by hand turns every apostrophe and dash into
// mojibake.
func firstText(blob string) string {
	if blob == "" {
		return ""
	}
	decoded, err := decodeOrdered([]byte(blob))
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(walkText(decoded)), " ")
}

// LockHolders is the process ids holding this thread's writer lock, if any.
//
// The lock files are empty and flock-based, so the holder is only discoverable
// through the kernel rather than by reading the file.
func (s *Sessions) LockHolders(threadID string) []int {
	if s.holders != nil {
		return s.holders(threadID)
	}
	lock := filepath.Join(s.LockDir(), threadID+".lock")
	if !fileExists(lock) {
		return []int{}
	}
	if _, err := s.which("fuser"); err != nil {
		return []int{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := s.run(ctx, []string{"fuser", lock}, nil)
	if err != nil {
		return []int{}
	}
	pids := []int{}
	for _, part := range strings.Fields(result.Stdout) {
		if pid, err := strconv.Atoi(part); err == nil && pid >= 0 && !strings.HasPrefix(part, "+") && !strings.HasPrefix(part, "-") {
			pids = append(pids, pid)
		}
	}
	return pids
}

// Session is one recent thread: what it is and how it ended.
type Session struct {
	ThreadID   string  `json:"thread_id"`
	Short      string  `json:"short"`
	LastAt     float64 `json:"last_at"`
	Turns      int     `json:"turns"`
	Failed     int     `json:"failed"`
	Active     int     `json:"active"`
	LastStatus string  `json:"last_status"`
	Topic      string  `json:"topic"`
	Holders    []int   `json:"holders"`
	Live       bool    `json:"live"`
	// State is one of working, stuck, failed, idle, closed.
	State string `json:"state"`
}

const recentQuery = `
        SELECT thread_id,
               MAX(started_at) AS last_at,
               COUNT(*) AS turns,
               SUM(CASE WHEN lower(status) IN ('failed','error','aborted') THEN 1 ELSE 0 END)
                   AS failed,
               SUM(CASE WHEN lower(status) IN ('inprogress','in_progress','running','started')
                        THEN 1 ELSE 0 END) AS active
          FROM thread_turns
         GROUP BY thread_id ORDER BY last_at DESC LIMIT ?
    `

// Recent lists the most recently active threads, with what they are and how
// they ended.
func (s *Sessions) Recent(limit int) ([]Session, error) {
	if !s.Available() {
		return nil, &SessionError{fmt.Sprintf("No Codex thread history at %s.", s.HistoryDB())}
	}
	found, err := s.readRecent(limit)
	if err != nil {
		return nil, &SessionError{fmt.Sprintf("could not read Codex history: %v", err)}
	}
	for i := range found {
		entry := &found[i]
		entry.Holders = s.LockHolders(entry.ThreadID)
		if entry.Holders == nil {
			entry.Holders = []int{}
		}
		entry.Live = len(entry.Holders) > 0
		entry.State = stateOf(*entry)
	}
	return found, nil
}

func (s *Sessions) readRecent(limit int) ([]Session, error) {
	db, err := sql.Open("sqlite", "file:"+s.HistoryDB()+"?mode=ro&_time_format=sqlite&_pragma=busy_timeout(15000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(recentQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := []Session{}
	for rows.Next() {
		var (
			threadID       sql.NullString
			lastAt         sql.NullFloat64
			turns          int
			failed, active sql.NullInt64
		)
		if err := rows.Scan(&threadID, &lastAt, &turns, &failed, &active); err != nil {
			return nil, err
		}
		found = append(found, Session{
			ThreadID: threadID.String,
			Short:    prefixRunes(threadID.String, 8),
			LastAt:   lastAt.Float64,
			Turns:    turns,
			Failed:   int(failed.Int64),
			Active:   int(active.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range found {
		entry := &found[i]
		topic, err := s.topicOf(db, entry.ThreadID)
		if err != nil {
			return nil, err
		}
		if topic == "" {
			topic = "(no prompt recorded)"
		}
		entry.Topic = topic
		var status sql.NullString
		err = db.QueryRow(`SELECT status FROM thread_turns WHERE thread_id=?
                        ORDER BY started_at DESC LIMIT 1`, entry.ThreadID).Scan(&status)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		entry.LastStatus = status.String
	}
	return found, nil
}

// ThreadItems is the readable text of the last limit stored items of a
// thread, oldest first. Items without text are empty strings, so callers can
// count what was stored as well as what was said. It is the work package's
// view of the history database (work.CodexSessions).
func (s *Sessions) ThreadItems(threadID string, limit int) ([]string, error) {
	db, err := sql.Open("sqlite", "file:"+s.HistoryDB()+"?mode=ro&_time_format=sqlite&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, &SessionError{err.Error()}
	}
	defer db.Close()
	rows, err := db.Query(`SELECT item_json FROM thread_items WHERE thread_id=?
                           ORDER BY rollout_ordinal DESC LIMIT ?`, threadID, limit)
	if err != nil {
		return nil, &SessionError{err.Error()}
	}
	defer rows.Close()
	var texts []string
	for rows.Next() {
		var blob sql.NullString
		if err := rows.Scan(&blob); err != nil {
			return nil, &SessionError{err.Error()}
		}
		texts = append(texts, firstText(blob.String))
	}
	if err := rows.Err(); err != nil {
		return nil, &SessionError{err.Error()}
	}
	slices.Reverse(texts)
	return texts, nil
}

// topicOf is the first stored item that reads as a prompt rather than a
// system preamble.
func (s *Sessions) topicOf(db *sql.DB, threadID string) (string, error) {
	items, err := db.Query(`SELECT item_json FROM thread_items WHERE thread_id=?
                            ORDER BY rollout_ordinal LIMIT 8`, threadID)
	if err != nil {
		return "", err
	}
	defer items.Close()
	for items.Next() {
		var blob sql.NullString
		if err := items.Scan(&blob); err != nil {
			return "", err
		}
		text := firstText(blob.String)
		if text != "" && !strings.HasPrefix(text, "<") && !strings.HasPrefix(text, "You are") &&
			!strings.HasPrefix(text, "#") {
			return prefixRunes(text, 100), nil
		}
	}
	return "", items.Err()
}

func prefixRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}

// stateOf says what this session needs, in one word.
func stateOf(entry Session) string {
	status := strings.ToLower(entry.LastStatus)
	if runningStatuses[status] && entry.Live {
		return "working"
	}
	if failedStatuses[status] {
		// A failed last turn with the lock still held is the stuck case: the
		// holder keeps failing and nothing else can take the conversation.
		if entry.Live {
			return "stuck"
		}
		return "failed"
	}
	if entry.Live {
		return "idle"
	}
	return "closed"
}

// Resolve finds one session by id prefix, refusing an ambiguous one.
func (s *Sessions) Resolve(prefix string) (*Session, error) {
	recent, err := s.Recent(200)
	if err != nil {
		return nil, err
	}
	matches := []Session{}
	for _, entry := range recent {
		if strings.HasPrefix(entry.ThreadID, prefix) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return nil, &SessionError{fmt.Sprintf("No recent Codex session starting %s", pyQuote(prefix))}
	}
	if len(matches) > 1 {
		names := []string{}
		for _, m := range matches[:min(5, len(matches))] {
			names = append(names, m.Short)
		}
		return nil, &SessionError{fmt.Sprintf("%s matches several sessions: %s", pyQuote(prefix), strings.Join(names, ", "))}
	}
	return &matches[0], nil
}

// --- actions --------------------------------------------------------------

// Nudged reports a message handed to a live session.
type Nudged struct {
	ThreadID string `json:"thread_id"`
	Queued   bool   `json:"queued"`
	Message  string `json:"message"`
}

// Nudge hands a message to a session that is already running.
//
// Uses Codex's own queue, so nothing is typed into a terminal and no window has
// to be opened. It only reaches a live session.
func (s *Sessions) Nudge(ctx context.Context, threadID, message string) (*Nudged, error) {
	ctx, cancel := context.WithTimeout(ctx, SessionTimeout)
	defer cancel()
	result, err := s.run(ctx, []string{"codex", "queue", "--thread", threadID, "--message", message}, nil)
	if err != nil {
		return nil, &SessionError{fmt.Sprintf("could not queue a message: %v", err)}
	}
	if result.Code != 0 {
		detail := result.Stderr
		if detail == "" {
			detail = result.Stdout
		}
		if detail == "" {
			detail = "codex queue failed"
		}
		return nil, &SessionError{prefixRunes(strings.TrimSpace(detail), 200)}
	}
	return &Nudged{ThreadID: threadID, Queued: true, Message: message}, nil
}

// Release closes whatever holds this thread's lock, so it can be resumed, and
// returns the process ids it signalled.
//
// Terminating rather than killing: Codex releases the lock and flushes its
// history on the way out. The holder is a session that can no longer make
// progress, which is why this is only reached for a stuck one.
func (s *Sessions) Release(threadID string, signal Signaller) ([]int, error) {
	if signal == nil {
		signal = signalProcess
	}
	parentOf, looksLikeCodex := s.parentOf, s.looksLikeCodex
	if parentOf == nil {
		parentOf = parentPID
	}
	if looksLikeCodex == nil {
		looksLikeCodex = cmdlineMentionsCodex
	}
	closed := []int{}
	for _, pid := range s.LockHolders(threadID) {
		parent := parentOf(pid)
		// The lock is held by the inner binary; terminating its launcher takes
		// the whole session down cleanly rather than orphaning a child.
		target := pid
		if parent > 1 && looksLikeCodex(parent) {
			target = parent
		}
		if err := signal(target, syscall.SIGTERM); err != nil {
			return closed, &SessionError{fmt.Sprintf("could not close process %d: %v", target, err)}
		}
		closed = append(closed, target)
	}
	return closed, nil
}

// parentPID reads PPid from /proc; 0 when unavailable.
func parentPID(pid int) int {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			parent, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0
			}
			return parent
		}
	}
	return 0
}

// cmdlineMentionsCodex reports whether the process command line names codex.
func cmdlineMentionsCodex(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(raw)), "codex")
}
