package concurrency_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mathomhaus/guild/internal/quest"
	"github.com/mathomhaus/guild/internal/storage"
)

// newTestDB opens a file-backed SQLite DB under t.TempDir, applies migrations,
// and registers a dummy project. A file DB (not :memory:) is required because
// modernc.org/sqlite gives each goroutine an independent in-memory DB when
// connection-string sharing isn't active — only a file path exercises real
// concurrent write contention across the pool.
func newTestDB(t *testing.T) (db *sql.DB, projectID string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quest.db")
	var err error
	db, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err = storage.Migrate(ctx, db, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, path, tasks_file) VALUES (?, ?, ?)`,
		"testproj", t.TempDir(), "TASKS.md",
	); err != nil {
		t.Fatalf("register project: %v", err)
	}
	return db, "testproj"
}

func mustPost(t *testing.T, db *sql.DB, pid string, params quest.PostParams) *quest.Quest {
	t.Helper()
	q, err := quest.Post(context.Background(), db, pid, params)
	if err != nil {
		t.Fatalf("Post %q: %v", params.Subject, err)
	}
	return q
}

func mustStatus(t *testing.T, db *sql.DB, pid, taskID string) quest.Status {
	t.Helper()
	var s sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT status FROM task_status WHERE project_id = ? AND task_id = ?`,
		pid, taskID,
	).Scan(&s)
	if err != nil {
		t.Fatalf("mustStatus %s: %v", taskID, err)
	}
	return quest.Status(s.String)
}

// Test_ConcurrentFulfillCascade_ChildrenFlipUnderContention fires N goroutines,
// each fulfilling a distinct parent quest that blocks a dedicated child. After
// the barrier, every child must be status=next — not blocked.
//
// Acceptance criteria from QUEST-188:
//  1. All N children flip to next despite concurrent parent fulfillments.
//  2. A shared sentinel depending on all N parents flips to next exactly once,
//     after the last concurrent Fulfill commits.
func Test_ConcurrentFulfillCascade_ChildrenFlipUnderContention(t *testing.T) {
	for _, n := range []int{10, 100} {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			db, pid := newTestDB(t)
			ctx := context.Background()

			// Post N parent quests (all status=next; no deps).
			parents := make([]*quest.Quest, n)
			for i := range parents {
				parents[i] = mustPost(t, db, pid, quest.PostParams{
					Subject: fmt.Sprintf("parent-%d", i),
				})
			}

			// Post N dedicated children, each blocking on its own parent.
			children := make([]*quest.Quest, n)
			for i := range children {
				children[i] = mustPost(t, db, pid, quest.PostParams{
					Subject:   fmt.Sprintf("child-%d", i),
					DependsOn: []string{parents[i].ID},
				})
				if children[i].Status != quest.StatusBlocked {
					t.Fatalf("child[%d] pre-condition: status=%s, want blocked", i, children[i].Status)
				}
			}

			// Post a shared sentinel that depends on ALL N parents.
			parentIDs := make([]string, n)
			for i, p := range parents {
				parentIDs[i] = p.ID
			}
			sentinel := mustPost(t, db, pid, quest.PostParams{
				Subject:   "sentinel",
				DependsOn: parentIDs,
			})
			if sentinel.Status != quest.StatusBlocked {
				t.Fatalf("sentinel pre-condition: status=%s, want blocked", sentinel.Status)
			}

			// Concurrently fulfill all N parents.
			var wg sync.WaitGroup
			errs := make([]error, n)
			wg.Add(n)
			for i := range parents {
				i := i
				go func() {
					defer wg.Done()
					_, errs[i] = quest.Fulfill(ctx, db, pid, parents[i].ID, "")
				}()
			}
			wg.Wait()

			// Every Fulfill must have succeeded — no SQLITE_BUSY leaking to callers.
			for i, err := range errs {
				if err != nil {
					t.Errorf("Fulfill parent[%d]: %v", i, err)
				}
			}

			// Criterion 1: every dedicated child must be next, not blocked.
			var stuck []string
			for i, child := range children {
				got := mustStatus(t, db, pid, child.ID)
				if got != quest.StatusNext {
					stuck = append(stuck, fmt.Sprintf("child[%d]=%s(%s)", i, child.ID, got))
				}
			}
			if len(stuck) > 0 {
				t.Errorf("N=%d: %d/%d children still blocked after concurrent fulfills: %v",
					n, len(stuck), n, stuck)
			}

			// Criterion 2: the shared sentinel must be next (all N parents done).
			if got := mustStatus(t, db, pid, sentinel.ID); got != quest.StatusNext {
				t.Errorf("N=%d: sentinel status=%s, want next (all %d parents done)", n, got, n)
			}
		})
	}
}
