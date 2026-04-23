package quest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// Journal appends a task-scoped journal entry to task_notes for questID.
//
// Wire format: the note is stored raw — the prefix is the note text as
// supplied by the caller, stored verbatim (not with a [journal] prefix tag).
// The agent and timestamp are tracked via the agent_id and created_at
// columns so Scroll can render them. An event is also emitted into
// task_events for the timeline view.
//
// agent may be empty — defaults to the OS user or "agent".
// Returns ErrNotFound when questID has no row in task_status.
func Journal(ctx context.Context, db *sql.DB, projectID, questID, agent, text string) error {
	if db == nil {
		return fmt.Errorf("quest: journal: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: journal: empty project_id")
	}
	questID = strings.ToUpper(strings.TrimSpace(questID))
	if questID == "" {
		return fmt.Errorf("quest: journal: empty quest_id")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("quest: journal: empty text")
	}
	agent = journalAgent(agent)

	now := time.Now().UTC().Format(time.RFC3339Nano)

	conn, rollback, err := beginImmediate(ctx, db, "journal")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Verify quest exists inside the pinned transaction so the check and
	// the note/event append share one serialized view.
	var n int
	err = conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_status WHERE project_id = ? AND task_id = ?`,
		projectID, questID,
	).Scan(&n)
	if err != nil {
		return fmt.Errorf("quest: journal: probe %s: %w", questID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, questID)
	}

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		projectID, questID, agent, text, now,
	); err != nil {
		return fmt.Errorf("quest: journal: insert note: %w", err)
	}

	if err := emitEvent(ctx, conn, projectID, questID, EventNoted, agent, text, now); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("quest: journal: commit: %w", err)
	}
	committed = true
	return nil
}

// journalAgent returns the agent string to use for a journal entry.
// Fallback chain: PM_OWNER → USER → "agent".
func journalAgent(agent string) string {
	if a := strings.TrimSpace(agent); a != "" {
		return a
	}
	if a := os.Getenv("PM_OWNER"); a != "" {
		return a
	}
	if a := os.Getenv("GUILD_AGENT"); a != "" {
		return a
	}
	if u, err := os.UserHomeDir(); err == nil && u != "" {
		// Use $USER if available, not the home dir.
		if a := os.Getenv("USER"); a != "" {
			return a
		}
	}
	return "agent"
}
