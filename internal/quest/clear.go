package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Clear is a backward-compat alias for Fulfill. Existing callers keep
// compiling; new code should call Fulfill directly. QUEST-106 / LORE-80.
var Clear = Fulfill

// Fulfill marks taskID as status='done' and atomically runs the cascade-
// unblock pass: every quest currently in status='blocked' whose full
// depends_on list is satisfied by the post-Fulfill done-set flips to
// status='next'.
//
// Non-negotiable correctness (QUEST-9 spec): the cascade must find
// EVERY newly-unblocked quest — not just immediate children. The done-set
// is re-read AFTER marking taskID done, then every blocked quest is
// re-scanned (no pre-filter). This handles the case where a quest's
// depends_on was updated mid-flight to include taskID. The strict
// algorithm costs almost nothing at our scale and immunizes against
// future schema drift.
//
// All writes happen in ONE transaction.
//
// `report` is written both into task_events.data (as the `done` event's
// payload) and as a `[completed] <report>` note in task_notes for
// chronological history. If `report` is empty, only the event fires
// (no note).
func Fulfill(ctx context.Context, db *sql.DB, projectID, taskID, report string) (*FulfillResult, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: clear: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: clear: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return nil, fmt.Errorf("quest: clear: empty task_id")
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Acquire a dedicated connection and open with BEGIN IMMEDIATE.
	// DEFERRED transactions would take a read snapshot before the first
	// write, causing concurrent Fulfill calls to read a stale done-set
	// and miss each other's parent→done transitions during the cascade
	// pass. BEGIN IMMEDIATE serializes writers at lock-acquisition time
	// so every cascade reads a fully committed done-set.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("quest: clear: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	const maxBeginAttempts = 20
	var beginErr error
	for attempt := range maxBeginAttempts {
		_, beginErr = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if beginErr == nil {
			break
		}
		if !isBusyErr(beginErr.Error()) {
			return nil, fmt.Errorf("quest: clear: begin immediate: %w", beginErr)
		}
		// Capped linear backoff: 10ms × attempt, max 200ms.
		base := time.Duration(attempt+1) * 10 * time.Millisecond
		if base > 200*time.Millisecond {
			base = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("quest: clear: begin immediate: %w", ctx.Err())
		case <-time.After(base):
		}
	}
	if beginErr != nil {
		return nil, fmt.Errorf("quest: clear: begin immediate (contended out after %d attempts): %w", maxBeginAttempts, beginErr)
	}
	committed := false
	defer func() { //nolint:contextcheck // rollback must not be cancelled by a caller-cancelled ctx
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// Exists check — prevents silent no-op when the caller typos an id.
	var existStatus, existOwner sql.NullString
	err = conn.QueryRowContext(ctx,
		`SELECT status, claimed_by FROM task_status
		 WHERE project_id = ? AND task_id = ?`,
		projectID, taskID,
	).Scan(&existStatus, &existOwner)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, taskID)
		}
		return nil, fmt.Errorf("quest: clear: probe existing: %w", err)
	}
	owner := existOwner.String

	// Mark done. We don't gate on status here — clearing an already-done
	// quest is a no-op that still emits an event, and clearing an
	// in_progress quest is the normal flow.
	if _, err := conn.ExecContext(ctx,
		`UPDATE task_status
		 SET status = 'done', updated_at = ?
		 WHERE project_id = ? AND task_id = ?`,
		now, projectID, taskID,
	); err != nil {
		return nil, fmt.Errorf("quest: clear: update: %w", err)
	}

	// Done event with report payload (possibly empty).
	if err := emitEvent(ctx, conn, projectID, taskID, EventDone, owner, report, now); err != nil {
		return nil, err
	}

	// [completed] <report> note for scroll/pulse history.
	if r := strings.TrimSpace(report); r != "" {
		agent := owner
		if agent == "" {
			agent = "agent"
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			projectID, taskID, agent, NotePrefixCompleted+r, now,
		); err != nil {
			return nil, fmt.Errorf("quest: clear: insert completed note: %w", err)
		}
	}

	// Cascade-unblock pass. This reads the post-mark done-set, so taskID
	// counts as done in the check.
	unblocked, err := findNewlyUnblocked(ctx, conn, projectID)
	if err != nil {
		return nil, err
	}
	if err := flipToNext(ctx, conn, projectID, taskID, unblocked, now); err != nil {
		return nil, err
	}

	// Reload the cleared quest to return a complete view.
	cleared, err := loadTx(ctx, conn, projectID, taskID)
	if err != nil {
		return nil, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: clear: commit: %w", err)
	}
	committed = true
	return &FulfillResult{Cleared: cleared, Unblocked: unblocked}, nil
}
