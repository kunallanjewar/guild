package quest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// RestoreResult summarizes what was imported.
type RestoreResult struct {
	TasksImported  int
	NotesImported  int
	EventsImported int
}

// Restore reads snapshotPath, checks schema_version, and applies the
// quest section into projectID. Existing rows are skipped (idempotent).
//
// snapshotPath is required. The caller (CLI) resolves the default path
// from the project registration.
//
// V1 behavior: re-insert task_status, task_notes, task_events skipping
// duplicates (same projectID+taskID for status; all notes/events are
// re-inserted since they don't have a stable natural key beyond their
// auto-increment id — we import them all and rely on the notes being
// idempotent at the application level).
func Restore(ctx context.Context, db *sql.DB, projectID, snapshotPath string) (*RestoreResult, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: restore: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return nil, fmt.Errorf("quest: restore: empty project_id")
	}
	if snapshotPath == "" {
		return nil, fmt.Errorf("quest: restore: empty snapshot path")
	}

	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("quest: restore: snapshot not found at %s", snapshotPath)
		}
		return nil, fmt.Errorf("quest: restore: read %s: %w", snapshotPath, err)
	}

	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("quest: restore: parse snapshot: %w", err)
	}

	// Version gate — only schema_version 1 supported.
	switch snap.SchemaVersion {
	case 0, 1:
		// 0 is a legacy format without a schema_version field; treat as v1.
	default:
		return nil, fmt.Errorf("quest: restore: unsupported schema_version %d (max=1)", snap.SchemaVersion)
	}

	if snap.Quest == nil {
		// Snapshot has no quest section — nothing to restore, not an error.
		return &RestoreResult{}, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result := &RestoreResult{}

	conn, rollback, err := beginImmediate(ctx, db, "restore")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// --- task_status: skip existing (projectID+taskID PK) ---
	for _, r := range snap.Quest.TaskStatus {
		var exists int
		err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM task_status WHERE project_id = ? AND task_id = ?`,
			projectID, r.TaskID,
		).Scan(&exists)
		if err != nil {
			return nil, fmt.Errorf("quest: restore: probe task_status %s: %w", r.TaskID, err)
		}
		if exists > 0 {
			continue
		}
		updAt := r.UpdatedAt
		if updAt == "" {
			updAt = now
		}
		var claimedBy, claimedAt sql.NullString
		if r.ClaimedBy != "" {
			claimedBy = sql.NullString{String: r.ClaimedBy, Valid: true}
		}
		if r.ClaimedAt != "" {
			claimedAt = sql.NullString{String: r.ClaimedAt, Valid: true}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO task_status (project_id, task_id, status, claimed_by, claimed_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			projectID, r.TaskID, r.Status, claimedBy, claimedAt, updAt,
		); err != nil {
			return nil, fmt.Errorf("quest: restore: insert task_status %s: %w", r.TaskID, err)
		}
		result.TasksImported++
	}

	// --- task_notes: insert all (auto-id, no natural key) ---
	for _, r := range snap.Quest.TaskNotes {
		createdAt := r.CreatedAt
		if createdAt == "" {
			createdAt = now
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			projectID, r.TaskID, r.AgentID, r.Note, createdAt,
		); err != nil {
			return nil, fmt.Errorf("quest: restore: insert task_notes %s: %w", r.TaskID, err)
		}
		result.NotesImported++
	}

	// --- task_events: insert all ---
	for _, r := range snap.Quest.TaskEvents {
		createdAt := r.CreatedAt
		if createdAt == "" {
			createdAt = now
		}
		var agentID, data sql.NullString
		if r.AgentID != "" {
			agentID = sql.NullString{String: r.AgentID, Valid: true}
		}
		if r.Data != "" {
			data = sql.NullString{String: r.Data, Valid: true}
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO task_events (project_id, task_id, event, agent_id, data, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			projectID, r.TaskID, r.Event, agentID, data, createdAt,
		); err != nil {
			return nil, fmt.Errorf("quest: restore: insert task_events %s: %w", r.TaskID, err)
		}
		result.EventsImported++
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("quest: restore: commit: %w", err)
	}
	committed = true
	return result, nil
}
