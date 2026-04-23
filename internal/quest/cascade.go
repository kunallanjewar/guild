package quest

import (
	"context"
	"fmt"
	"sort"
)

// findNewlyUnblocked returns the task_ids of every quest currently in
// status='blocked' whose entire depends_on list is now satisfied by the
// project's done-set. Runs inside a transaction so the read of the
// blocked set and the subsequent UPDATE-to-next by the caller are
// consistent.
//
// This is the heart of the cascade-unblock invariant. The algorithm is:
//
//  1. Compute the current done-set (one query).
//  2. For every quest in status='blocked':
//     a. Resolve its full spec via spec-replay (picks up depends_on).
//     b. If depends_on is non-empty AND every dep is in done-set,
//     record it for unblocking.
//
// The returned slice is sorted by task_id so the cascade order is
// deterministic — important for the emoji output in the CLI layer and
// for test assertions.
func findNewlyUnblocked(ctx context.Context, tx dbTx, projectID string) ([]*Quest, error) {
	doneSet, err := loadDoneSet(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}

	blockedIDs, err := loadBlockedIDs(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}

	var unblocked []*Quest
	for _, bid := range blockedIDs {
		spec, err := loadSpec(ctx, tx, projectID, bid)
		if err != nil {
			return nil, err
		}
		if len(spec.DependsOn) == 0 {
			// A blocked quest with no deps is inconsistent but shouldn't
			// block cascade progress; skip it rather than flipping
			// something we can't justify.
			continue
		}
		allDone := true
		for _, d := range spec.DependsOn {
			if !doneSet[d] {
				allDone = false
				break
			}
		}
		if allDone {
			unblocked = append(unblocked, spec)
		}
	}

	sort.Slice(unblocked, func(i, j int) bool {
		return unblocked[i].ID < unblocked[j].ID
	})
	return unblocked, nil
}

// loadDoneSet returns the set of task_ids in projectID with status='done'.
// Used by findNewlyUnblocked and Post's dep check.
func loadDoneSet(ctx context.Context, tx dbTx, projectID string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT task_id FROM task_status WHERE project_id = ? AND status = 'done'`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: load done set: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, fmt.Errorf("quest: load done set scan: %w", err)
		}
		out[tid] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: load done set iter: %w", err)
	}
	return out, nil
}

// loadBlockedIDs returns every task_id in projectID with status='blocked',
// sorted for determinism.
func loadBlockedIDs(ctx context.Context, tx dbTx, projectID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT task_id FROM task_status
		 WHERE project_id = ? AND status = 'blocked'
		 ORDER BY task_id`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: load blocked: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, fmt.Errorf("quest: load blocked scan: %w", err)
		}
		out = append(out, tid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: load blocked iter: %w", err)
	}
	return out, nil
}

// flipToNext marks each quest in unblocked as status='next' inside tx,
// emits an `unblocked` event citing completedID as the cause, and
// returns the list in the same order. Runs inside the caller's tx so
// partial progress can't leak on failure.
func flipToNext(ctx context.Context, tx dbTx, projectID, completedID string, unblocked []*Quest, now string) error {
	for _, q := range unblocked {
		if _, err := tx.ExecContext(ctx,
			`UPDATE task_status
			 SET status = 'next', updated_at = ?
			 WHERE project_id = ? AND task_id = ? AND status = 'blocked'`,
			now, projectID, q.ID,
		); err != nil {
			return fmt.Errorf("quest: cascade flip %s: %w", q.ID, err)
		}
		q.Status = StatusNext
		if err := emitEvent(ctx, tx, projectID, q.ID, EventUnblocked, "quest", completedID, now); err != nil {
			return err
		}
	}
	return nil
}
