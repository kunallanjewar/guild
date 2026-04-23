package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Summon transfers ownership of questID to targetAgent. The quest is set
// to status=in_progress and claimed_by=targetAgent. An "assigned" event
// is emitted for the timeline.
//
// The original owner (or calling agent) is the emitter; targetAgent is
// the assignee recorded in both task_status and the event payload.
//
// callerAgent identifies who is issuing the summon (used for the event);
// if empty it defaults via journalAgent.
// Returns ErrNotFound when questID has no row in task_status.
func Summon(ctx context.Context, db *sql.DB, projectID, questID, targetAgent, callerAgent string) error {
	if db == nil {
		return fmt.Errorf("quest: summon: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: summon: empty project_id")
	}
	questID = strings.ToUpper(strings.TrimSpace(questID))
	if questID == "" {
		return fmt.Errorf("quest: summon: empty quest_id")
	}
	targetAgent = strings.TrimSpace(targetAgent)
	if targetAgent == "" {
		return fmt.Errorf("quest: summon: empty target agent")
	}
	callerAgent = journalAgent(callerAgent)

	now := time.Now().UTC().Format(time.RFC3339Nano)

	conn, rollback, err := beginImmediate(ctx, db, "summon")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Verify quest exists inside the pinned transaction so the validation
	// and the ownership transfer share one serialized view.
	var existStatus sql.NullString
	err = conn.QueryRowContext(ctx,
		`SELECT status FROM task_status WHERE project_id = ? AND task_id = ?`,
		projectID, questID,
	).Scan(&existStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, questID)
		}
		return fmt.Errorf("quest: summon: probe %s: %w", questID, err)
	}

	if _, err := conn.ExecContext(ctx,
		`UPDATE task_status
		 SET status = 'in_progress', claimed_by = ?, claimed_at = ?, updated_at = ?
		 WHERE project_id = ? AND task_id = ?`,
		targetAgent, now, now, projectID, questID,
	); err != nil {
		return fmt.Errorf("quest: summon: update %s: %w", questID, err)
	}

	// Emit an "assigned" event: data contains the assignee so Scroll
	// can show "assigned → <agent>".
	if err := emitEvent(ctx, conn, projectID, questID, "assigned", callerAgent, targetAgent, now); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("quest: summon: commit: %w", err)
	}
	committed = true
	return nil
}
