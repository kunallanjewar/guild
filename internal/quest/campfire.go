package quest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CampfireParams holds the fields for a campfire snapshot. At least one
// non-empty field is required.
type CampfireParams struct {
	Hypothesis   string
	Tried        []string // each item is one tried approach
	Next         string
	TokenWarning bool
	Agent        string
}

// Empty returns true when no snapshot field is set.
//
//nolint:gocritic // hugeParam: by-value is intentional — matches UpdateParams.Empty() pattern
func (p CampfireParams) Empty() bool {
	return p.Hypothesis == "" && len(p.Tried) == 0 && p.Next == "" && !p.TokenWarning
}

// Campfire persists a structured mid-task working-state snapshot to
// task_notes for questID, and emits a corresponding task_events row.
//
// Wire format:
//
//	[checkpoint] hypothesis: H | tried: A; B; C | next: N | token_warning: true
//
// Fields are pipe-separated sub-fields; absent fields are omitted. This
// matches the `[checkpoint]` prefix already used by the Accept trail-
// write in accept.go, making the snapshot visible to Scroll's note
// reader without a dedicated prefix parser.
//
//nolint:gocritic // hugeParam: by-value keeps the API ergonomic for callers
func Campfire(ctx context.Context, db *sql.DB, projectID, questID string, p CampfireParams) error {
	if db == nil {
		return fmt.Errorf("quest: campfire: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: campfire: empty project_id")
	}
	questID = strings.ToUpper(strings.TrimSpace(questID))
	if questID == "" {
		return fmt.Errorf("quest: campfire: empty quest_id")
	}
	if p.Empty() {
		return fmt.Errorf("quest: campfire: provide at least one of hypothesis, tried, next, token_warning")
	}

	agent := journalAgent(p.Agent)

	snapshot := buildCampfireSnapshot(p)
	noteText := NotePrefixCheckpoint + snapshot
	now := time.Now().UTC().Format(time.RFC3339Nano)

	conn, rollback, err := beginImmediate(ctx, db, "campfire")
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
		return fmt.Errorf("quest: campfire: probe %s: %w", questID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, questID)
	}

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO task_notes (project_id, task_id, agent_id, note, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		projectID, questID, agent, noteText, now,
	); err != nil {
		return fmt.Errorf("quest: campfire: insert note: %w", err)
	}

	if err := emitEvent(ctx, conn, projectID, questID, "checkpoint", agent, snapshot, now); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("quest: campfire: commit: %w", err)
	}
	committed = true
	return nil
}

// buildCampfireSnapshot constructs the pipe-joined payload string.
//
//nolint:gocritic // hugeParam: by-value is intentional for consistency with CampfireParams.Empty
func buildCampfireSnapshot(p CampfireParams) string {
	var parts []string

	if h := strings.TrimSpace(p.Hypothesis); h != "" {
		parts = append(parts, "hypothesis: "+h)
	}

	if len(p.Tried) > 0 {
		cleaned := make([]string, 0, len(p.Tried))
		for _, t := range p.Tried {
			if t = strings.TrimSpace(t); t != "" {
				cleaned = append(cleaned, t)
			}
		}
		if len(cleaned) > 0 {
			parts = append(parts, "tried: "+strings.Join(cleaned, "; "))
		}
	}

	if n := strings.TrimSpace(p.Next); n != "" {
		parts = append(parts, "next: "+n)
	}

	if p.TokenWarning {
		parts = append(parts, "token_warning: true")
	}

	return strings.Join(parts, " | ")
}
