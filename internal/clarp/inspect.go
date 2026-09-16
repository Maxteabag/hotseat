package clarp

// What a stopped Clarp agent was actually doing.
//
// A list of stopped agents is only half useful: deciding whether to continue one
// means knowing what it was in the middle of. This recovers that from what Clarp
// kept: its message log and its turn queue. Content is truncated on the way out
// rather than in the caller, so a long transcript cannot flood a terminal, and
// speech markup is stripped because it is delivery scaffolding, not what the
// agent said.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Maxteabag/hotseat/internal/work"
)

// QueuedTurn is a prompt waiting in a Clarp agent's queue.
type QueuedTurn struct {
	Origin *string `json:"origin"`
	Text   string  `json:"text"`
}

// Inspect is everything known about one Clarp agent, by session slug. It
// returns nil, nil when no state database exists or no live agent has that
// session, so a caller can fall through to work.Inspect for native transcripts.
// That is the merge order the Python plugin used: Clarp answers first, then the
// native transcript, then "nothing found".
func (s *Service) Inspect(ctx context.Context, identifier string) (*work.Detail, error) {
	if !stateExists(s.StatePath) {
		return nil, nil
	}
	detail, err := s.clarpDetail(ctx, identifier)
	if err != nil {
		return nil, &work.InspectError{Msg: fmt.Sprintf("could not read Clarp state: %v", err)}
	}
	return detail, nil
}

type message struct {
	role, text, timestamp sql.NullString
}

func (s *Service) clarpDetail(ctx context.Context, session string) (*work.Detail, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var agentID string
	var persona, backend, cwd, model sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT agent_id, persona, backend, cwd, model FROM agents
                    WHERE session = ? AND deleted_at IS NULL`, session).
		Scan(&agentID, &persona, &backend, &cwd, &model)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx,
		`SELECT role, text, timestamp FROM messages
                    WHERE agent_id = ? AND role IN ('user','assistant')
                      AND length(trim(coalesce(text,''))) > 0
                    ORDER BY seq DESC LIMIT 200`, agentID)
	if err != nil {
		return nil, err
	}
	var newestFirst []message
	for rows.Next() {
		var m message
		if err := rows.Scan(&m.role, &m.text, &m.timestamp); err != nil {
			rows.Close()
			return nil, err
		}
		newestFirst = append(newestFirst, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	queuedRows, err := db.QueryContext(ctx,
		`SELECT text, origin, enqueued_at FROM queued_turns
                    WHERE agent_id = ? AND status = 'queued'
                    ORDER BY queue_seq`, agentID)
	if err != nil {
		return nil, err
	}
	queued := []any{}
	for queuedRows.Next() {
		var text, origin sql.NullString
		var enqueued sql.NullInt64
		if err := queuedRows.Scan(&text, &origin, &enqueued); err != nil {
			queuedRows.Close()
			return nil, err
		}
		queued = append(queued, QueuedTurn{Origin: nullable(origin), Text: work.Trim(text.String, 300)})
	}
	queuedRows.Close()
	if err := queuedRows.Err(); err != nil {
		return nil, err
	}

	// newestFirst is the query order; the Python reversed it into "ordered" and
	// then walked that backwards, which is the same thing.
	detail := &work.Detail{
		Kind:    "clarp",
		ID:      session,
		Name:    persona.String,
		Backend: backend.String,
		Model:   nullable(model),
		CWD:     nullable(cwd),
		Queued:  queued,
		Recent:  []work.RecentTurn{},
	}
	if detail.Name == "" {
		detail.Name = session
	}
	for _, m := range newestFirst {
		if m.role.String != "user" {
			continue
		}
		if !strings.HasPrefix(m.text.String, work.ContextPreamble) {
			detail.LastUser = work.Trim(m.text.String, work.Snippet)
			break
		}
	}
	for _, m := range newestFirst {
		if m.role.String != "user" {
			continue
		}
		if summary := work.SummaryOf(m.text.String); summary != "" {
			detail.Summary = summary
			break
		}
	}
	for _, m := range newestFirst {
		if m.role.String == "assistant" {
			detail.LastAssistant = work.Trim(m.text.String, work.Snippet)
			break
		}
	}
	if len(newestFirst) > 0 {
		detail.LastActivity = nullable(newestFirst[0].timestamp)
	}
	recent := newestFirst
	if len(recent) > 6 {
		recent = recent[:6]
	}
	for i := len(recent) - 1; i >= 0; i-- {
		detail.Recent = append(detail.Recent, work.RecentTurn{
			Role: recent[i].role.String,
			Text: work.Trim(recent[i].text.String, 160),
		})
	}
	return detail, nil
}

// nullable is a NULL-able column as the pointer the JSON shape wants: nil for
// NULL, the value otherwise, even when empty.
func nullable(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	s := value.String
	return &s
}
