package clarp

// Find work that a usage limit stopped, and continue it.
//
// Hitting a limit does not fail loudly in a way you can act on later. A Clarp
// agent records the interruption and goes quiet; a terminal session prints one
// line and waits. Hours later, when quota is back, there is no list of what
// stopped.
//
// This builds that list from evidence, and continues each item only when the
// account it needs actually has capacity. Resuming into an exhausted account just
// reproduces the failure, so quota is checked first rather than after.
//
// Two sources, detected differently:
//
//   - Clarp agents: its state database logs `reason=usage_limit` against the
//     agent. An agent counts as still stopped only if it has completed no turn
//     since.
//   - Native sessions: the transcript's last assistant entry is an API error
//     whose text is the session-limit message. That detection lives in
//     internal/work and is merged in here.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Maxteabag/hotseat/internal/work"
)

// promptTimeout bounds `clarp-admin prompt`.
const promptTimeout = 60 * time.Second

// lastSaid is the last thing an agent said, for the listing.
const lastSaid = `(SELECT m.text FROM messages m
                  WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                    AND length(trim(coalesce(m.text,''))) > 0
                  ORDER BY m.seq DESC LIMIT 1) AS said`

func resumeErr(format string, args ...any) error {
	return &work.ResumeError{Msg: fmt.Sprintf(format, args...)}
}

// open opens the state database read-only with a 10 s busy timeout. The
// database belongs to a running service, so nothing here may write to it.
func (s *Service) open() (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+s.StatePath+"?mode=ro&_time_format=sqlite&_pragma=busy_timeout(10000)")
}

// agentRow is the column set every stopped-agent query selects.
type agentRow struct {
	session, persona, backend, cwd, model, said sql.NullString
	ts, pending                                 sql.NullInt64
}

func (r agentRow) item(cause string) work.StoppedItem {
	name := r.persona.String
	if name == "" {
		name = r.session.String
	}
	return work.StoppedItem{
		Kind:        "clarp",
		ID:          r.session.String,
		Name:        name,
		Backend:     r.backend.String,
		CWD:         r.cwd.String,
		Cause:       cause,
		Pending:     int(r.pending.Int64),
		LastMessage: work.Trim(r.said.String, 150),
		StoppedAt:   float64(r.ts.Int64) / 1000,
	}
}

// PausedClarpAgents lists Clarp agents whose turn queue is paused.
//
// Stopping a turn leaves `paused=1` behind. While it is set, a prompt does not
// run: the dispatcher queues it and returns success, so the agent looks
// contactable while actually going nowhere. Any work already queued sits behind
// that flag indefinitely.
//
// This is a different stuck state from a usage limit and is reported separately,
// because prompting is the fix for one and a no-op for the other.
func (s *Service) PausedClarpAgents(ctx context.Context) ([]work.StoppedItem, error) {
	if !stateExists(s.StatePath) {
		return nil, nil
	}
	const query = `
        SELECT a.session, a.persona, a.backend, a.cwd,
               (SELECT m.text FROM messages m
                 WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                   AND length(trim(coalesce(m.text,''))) > 0
                 ORDER BY m.seq DESC LIMIT 1) AS said,
               (SELECT COUNT(*) FROM queued_turns q
                 WHERE q.agent_id = a.agent_id AND q.status = 'queued') AS pending,
               (SELECT MIN(q.enqueued_at) FROM queued_turns q
                 WHERE q.agent_id = a.agent_id AND q.status = 'queued') AS oldest
          FROM queue_state_revisions r
          JOIN agents a USING (agent_id)
         WHERE r.paused = 1 AND a.deleted_at IS NULL AND a.archived_at IS NULL
    `
	items, err := s.queryAgents(ctx, "queue_paused", query, func(rows *sql.Rows, r *agentRow) error {
		return rows.Scan(&r.session, &r.persona, &r.backend, &r.cwd, &r.said, &r.pending, &r.ts)
	})
	if err != nil {
		return nil, resumeErr("could not read Clarp queue state: %v", err)
	}
	return items, nil
}

// ParkedClarpAgents lists Clarp agents parked waiting for an account with quota.
//
// When a Claude turn hits a limit, Clarp parks it and retries an account check
// every minute. The check demands that ONE account serve every parked agent's
// model at once, so a single agent wanting a model nobody has quota for keeps
// every other parked agent waiting, including ones whose model is available.
//
// Detecting this matters because nothing else names it: the runtime only logs
// "No verified account", and the agent that cannot be served is not necessarily
// the agent you are waiting on.
func (s *Service) ParkedClarpAgents(ctx context.Context, windowDays int) ([]work.StoppedItem, error) {
	if !stateExists(s.StatePath) {
		return nil, nil
	}
	var cutoff int64
	if windowDays != 0 {
		cutoff = s.cutoffMillis(windowDays)
	}
	query := `
        WITH parked AS (
            SELECT agent_id, MAX(ts) AS ts FROM state_log
             WHERE json_extract(detail, '$.account_recovery') = 'waiting'
               AND ts >= ?
             GROUP BY agent_id),
        finished AS (
            SELECT agent_id, MAX(ts) AS ok_ts FROM state_log
             WHERE kind = 'done' GROUP BY agent_id)
        SELECT a.session, a.persona, a.backend, a.cwd, a.model, p.ts,
               ` + lastSaid + `
          FROM parked p
          JOIN agents a USING (agent_id)
          LEFT JOIN finished f USING (agent_id)
         WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
           AND (f.ok_ts IS NULL OR f.ok_ts < p.ts)
    `
	items, err := s.queryAgents(ctx, "waiting_for_account", query, func(rows *sql.Rows, r *agentRow) error {
		return rows.Scan(&r.session, &r.persona, &r.backend, &r.cwd, &r.model, &r.ts, &r.said)
	}, cutoff)
	if err != nil {
		return nil, resumeErr("could not read Clarp state: %v", err)
	}
	return items, nil
}

// StoppedClarpAgents lists Clarp agents whose last usage-limit stop has not been
// followed by a turn, most recent stop first.
func (s *Service) StoppedClarpAgents(ctx context.Context, windowDays int) ([]work.StoppedItem, error) {
	if !stateExists(s.StatePath) {
		return nil, nil
	}
	var cutoff any
	if windowDays != 0 {
		cutoff = s.cutoffMillis(windowDays)
	}
	const query = `
        WITH stopped AS (
            SELECT agent_id, MAX(ts) AS fail_ts
              FROM state_log
             WHERE kind IN ('interrupted', 'error')
               AND json_extract(detail, '$.reason') = 'usage_limit'
             GROUP BY agent_id),
        finished AS (
            SELECT agent_id, MAX(ts) AS ok_ts FROM state_log
             WHERE kind = 'done' GROUP BY agent_id)
        SELECT a.session, a.persona, a.backend, a.cwd, s.fail_ts,
               (SELECT m.text FROM messages m
                 WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                   AND length(trim(coalesce(m.text,''))) > 0
                 ORDER BY m.seq DESC LIMIT 1) AS said
          FROM stopped s
          JOIN agents a USING (agent_id)
          LEFT JOIN finished f USING (agent_id)
         WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
           AND (f.ok_ts IS NULL OR f.ok_ts < s.fail_ts)
           AND (? IS NULL OR s.fail_ts >= ?)
         ORDER BY s.fail_ts DESC
    `
	items, err := s.queryAgents(ctx, "usage_limit", query, func(rows *sql.Rows, r *agentRow) error {
		return rows.Scan(&r.session, &r.persona, &r.backend, &r.cwd, &r.ts, &r.said)
	}, cutoff, cutoff)
	if err != nil {
		return nil, resumeErr("could not read Clarp state: %v", err)
	}
	return items, nil
}

// cutoffMillis is now minus the window, in the epoch milliseconds state_log uses.
func (s *Service) cutoffMillis(windowDays int) int64 {
	now := s.now()
	seconds := float64(now.UnixNano())/1e9 - float64(windowDays)*86400
	return int64(seconds * 1000)
}

// queryAgents runs one stopped-agent query and shapes each row as an item.
func (s *Service) queryAgents(ctx context.Context, cause, query string,
	scan func(*sql.Rows, *agentRow) error, args ...any) ([]work.StoppedItem, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []work.StoppedItem
	for rows.Next() {
		var r agentRow
		if err := scan(rows, &r); err != nil {
			return nil, err
		}
		item := r.item(cause)
		if cause == "waiting_for_account" {
			item.Model = r.model.String
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// Stopped lists everything that stopped and has not run since, by either
// cause, ordered by priority. Two causes are kept apart because their fixes
// differ: a usage limit, which a prompt fixes once quota returns, and a paused
// queue, which a prompt does not fix at all.
//
// Merge order, as the Python plugin did it:
//
//  1. native sessions whose transcript ends on a usage limit (internal/work);
//  2. Clarp agents stopped by a usage limit;
//  3. Clarp agents parked waiting for an account, skipping ids already listed;
//  4. when includePaused, Clarp agents whose queue is paused replace any Clarp
//     item with the same id, because a pause blocks dispatch regardless of the
//     earlier interruption reason;
//  5. the whole list is sorted with work.SortByPriority.
func (s *Service) Stopped(ctx context.Context, windowDays int, includePaused bool) ([]work.StoppedItem, error) {
	items := work.StoppedNativeSessions(s.ProjectsDir, windowDays, s.now())
	stopped, err := s.StoppedClarpAgents(ctx, windowDays)
	if err != nil {
		return nil, err
	}
	items = append(items, stopped...)
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.ID] = true
	}
	parked, err := s.ParkedClarpAgents(ctx, windowDays)
	if err != nil {
		return nil, err
	}
	for _, item := range parked {
		if !seen[item.ID] {
			items = append(items, item)
		}
	}
	if includePaused {
		paused, err := s.PausedClarpAgents(ctx)
		if err != nil {
			return nil, err
		}
		pausedIDs := map[string]bool{}
		for _, item := range paused {
			pausedIDs[item.ID] = true
		}
		kept := make([]work.StoppedItem, 0, len(items)+len(paused))
		for _, item := range items {
			if item.Kind != "clarp" || !pausedIDs[item.ID] {
				kept = append(kept, item)
			}
		}
		items = append(kept, paused...)
	}
	work.SortByPriority(items)
	return items, nil
}

// Continue continues one stopped item. A Clarp agent is prompted through
// clarp-admin; a native session has no supervisor to accept a prompt, so the
// resume command is handed back rather than run behind the user's back. An
// empty prompt means work.ResumePrompt.
func (s *Service) Continue(ctx context.Context, item work.StoppedItem, prompt string) (work.Continuation, error) {
	if prompt == "" {
		prompt = work.ResumePrompt
	}
	if item.Cause == "queue_paused" {
		return work.Continuation{}, resumeErr(
			"%s's queue is paused. A prompt would be queued behind the pause rather than run. "+
				"Clear it from the Clarp app: start or remove the waiting turns for that agent.", item.Name)
	}
	if item.Kind != "clarp" {
		return work.ContinueItem(item)
	}
	ctx, cancel := context.WithTimeout(ctx, promptTimeout)
	defer cancel()
	// origin=automation, because this is not the user typing. Clarp records it
	// as such rather than as a message they sent.
	cmd := exec.CommandContext(ctx, Admin, "prompt", "--to", item.ID, "--text", prompt, "--origin", "automation")
	if _, err := s.run(cmd); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return work.Continuation{}, &work.ResumeError{Msg: stderrMessage(exit, "clarp-admin prompt failed")}
		}
		return work.Continuation{}, resumeErr("could not prompt %s: %v", item.Name, err)
	}
	return work.Continuation{ID: item.ID, Kind: "clarp", Continued: true}, nil
}

// Snapshot is what the collector adds to its snapshot for Clarp: the agents
// overview, or nil when the registry could not be read, and the stopped Clarp
// agents with their readiness against the given accounts.
type Snapshot struct {
	Agents  *Overview          `json:"agents"`
	Stopped []work.StoppedItem `json:"stopped"`
}

// Snapshot mirrors the plugin's snapshot(accounts). defaultAlias is the active
// account; the Python derived it from the accounts' is_active flag, which
// work.AccountQuota does not carry.
func (s *Service) Snapshot(ctx context.Context, accounts []work.AccountQuota, defaultAlias string) (Snapshot, error) {
	overview, err := s.Overview(ctx, defaultAlias)
	if err != nil {
		var clarpError *ClarpError
		if !errors.As(err, &clarpError) {
			return Snapshot{}, err
		}
		overview = nil
	}
	all, err := s.Stopped(ctx, work.DefaultWindowDays, true)
	if err != nil {
		return Snapshot{}, err
	}
	items := make([]work.StoppedItem, 0, len(all))
	for _, item := range all {
		if item.Kind != "clarp" {
			continue
		}
		readiness := work.ReadinessOf(item, accounts, defaultAlias)
		item.Readiness = &readiness
		items = append(items, item)
	}
	return Snapshot{Agents: overview, Stopped: items}, nil
}
