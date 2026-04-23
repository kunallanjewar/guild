package quest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EpicResult reports which quests had the epic set and which were
// skipped because they don't exist. One line per missing id is printed.
type EpicResult struct {
	Applied []string
	Skipped []string // task_ids not found in task_status
	Epic    string
}

// SetEpic applies epic to every quest in taskIDs. Missing task_ids are
// recorded in Result.Skipped rather than aborting — it prints "error: X
// not found, skipping" and continues to the next id.
//
// Stored as one `[spec] epic: <name>` note per task_id, so future spec
// replay picks up the value as a scalar last-value-wins overwrite.
func SetEpic(ctx context.Context, db *sql.DB, projectID, epic string, taskIDs []string) (*EpicResult, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: epic: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: epic: empty project_id")
	}
	if epic = strings.TrimSpace(epic); epic == "" {
		return nil, fmt.Errorf("quest: epic: empty epic name")
	}
	if len(taskIDs) == 0 {
		return nil, fmt.Errorf("quest: epic: no task_ids")
	}

	result := &EpicResult{Epic: epic}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	agent := agentOrDefault("")

	conn, rollback, err := beginImmediate(ctx, db, "epic")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	for _, raw := range taskIDs {
		tid := strings.ToUpper(strings.TrimSpace(raw))
		if tid == "" {
			continue
		}
		// Existence probe.
		var exists int
		err := conn.QueryRowContext(ctx,
			`SELECT 1 FROM task_status
			 WHERE project_id = ? AND task_id = ?`,
			projectID, tid,
		).Scan(&exists)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				result.Skipped = append(result.Skipped, tid)
				continue
			}
			return nil, fmt.Errorf("quest: epic: probe %s: %w", tid, err)
		}
		if err := insertSpecNote(ctx, conn, projectID, tid, agent, now,
			NotePrefixSpec+"epic: "+epic); err != nil {
			return nil, err
		}
		result.Applied = append(result.Applied, tid)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: epic: commit: %w", err)
	}
	committed = true
	return result, nil
}
