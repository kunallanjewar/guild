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

func mustAccept(t *testing.T, db *sql.DB, pid, taskID, owner string) *quest.Quest {
	t.Helper()
	q, err := quest.Accept(context.Background(), db, pid, taskID, owner)
	if err != nil {
		t.Fatalf("Accept %s: %v", taskID, err)
	}
	return q
}

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

// Test_ConcurrentPost_AllIDsUnique fires N goroutines each posting a distinct
// quest. Under DEFERRED isolation, nextQuestID's MAX(id) scan can read a stale
// snapshot and two goroutines can pick the same ID — the PK rejects the second,
// surfacing SQLITE_BUSY or a unique-constraint error to the caller. With
// BEGIN IMMEDIATE, writers serialize at lock-acquisition time, so every Post
// reads the already-committed MAX and emits a genuinely monotonic ID.
//
// Invariant: all N posts succeed and all N returned IDs are unique.
func Test_ConcurrentPost_AllIDsUnique(t *testing.T) {
	for _, n := range []int{10, 100} {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			db, pid := newTestDB(t)
			ctx := context.Background()

			results := make([]*quest.Quest, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			wg.Add(n)
			for i := range n {
				i := i
				go func() {
					defer wg.Done()
					results[i], errs[i] = quest.Post(ctx, db, pid, quest.PostParams{
						Subject: fmt.Sprintf("quest-%d", i),
					})
				}()
			}
			wg.Wait()

			// All posts must have succeeded — no SQLITE_BUSY or PK-conflict errors.
			for i, err := range errs {
				if err != nil {
					t.Errorf("Post[%d]: %v", i, err)
				}
			}

			// All returned IDs must be unique.
			seen := map[string]int{}
			for i, q := range results {
				if q == nil {
					continue
				}
				if prev, dup := seen[q.ID]; dup {
					t.Errorf("ID collision: Post[%d] and Post[%d] both got %s", prev, i, q.ID)
				}
				seen[q.ID] = i
			}
		})
	}
}

// Test_ConcurrentUpdate_NoLostWrites fires N goroutines each appending a
// distinct acceptance criterion to the same quest. Under DEFERRED isolation
// two goroutines can read the same snapshot and emit conflicting writes,
// surfacing SQLITE_BUSY to callers. With BEGIN IMMEDIATE, writers serialize so
// every Update commits exactly once.
//
// Invariant: all N updates succeed and the final quest carries all N criteria.
func Test_ConcurrentUpdate_NoLostWrites(t *testing.T) {
	for _, n := range []int{10, 100} {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			db, pid := newTestDB(t)
			ctx := context.Background()

			q := mustPost(t, db, pid, quest.PostParams{Subject: "target"})

			errs := make([]error, n)
			var wg sync.WaitGroup
			wg.Add(n)
			for i := range n {
				i := i
				go func() {
					defer wg.Done()
					_, errs[i] = quest.Update(ctx, db, pid, q.ID, quest.UpdateParams{
						Acceptance: []string{fmt.Sprintf("criterion-%d", i)},
					})
				}()
			}
			wg.Wait()

			// All updates must have succeeded — no SQLITE_BUSY leaking to callers.
			for i, err := range errs {
				if err != nil {
					t.Errorf("Update[%d]: %v", i, err)
				}
			}

			// All N criteria must be present in the final quest.
			final, err := quest.Load(ctx, db, pid, q.ID)
			if err != nil {
				t.Fatalf("Load after updates: %v", err)
			}
			critSet := map[string]bool{}
			for _, c := range final.Acceptance {
				critSet[c] = true
			}
			for i := range n {
				want := fmt.Sprintf("criterion-%d", i)
				if !critSet[want] {
					t.Errorf("criterion-%d missing from final quest (lost write)", i)
				}
			}
		})
	}
}

// Test_ConcurrentForfeit_OnlyReleasesOnce fires N goroutines all trying to
// forfeit the same in_progress quest. Only one goroutine should produce a
// non-AlreadyNext result (the one that actually executes the release path);
// all others should see status=next and return AlreadyNext=true. Under
// DEFERRED isolation, multiple goroutines can read in_progress simultaneously
// and all execute the release UPDATE, losing idempotency. With BEGIN IMMEDIATE,
// only one writer holds the lock at a time.
//
// Invariant: exactly one goroutine returns AlreadyNext=false; the rest return
// AlreadyNext=true (or an error). No SQLITE_BUSY errors surface to callers.
func Test_ConcurrentForfeit_OnlyReleasesOnce(t *testing.T) {
	for _, n := range []int{10, 100} {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			db, pid := newTestDB(t)
			ctx := context.Background()

			q := mustPost(t, db, pid, quest.PostParams{Subject: "claimable"})
			mustAccept(t, db, pid, q.ID, "worker")

			results := make([]*quest.ForfeitResult, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			wg.Add(n)
			for i := range n {
				i := i
				go func() {
					defer wg.Done()
					results[i], errs[i] = quest.Forfeit(ctx, db, pid, q.ID, "")
				}()
			}
			wg.Wait()

			// No SQLITE_BUSY errors must surface.
			for i, err := range errs {
				if err != nil {
					t.Errorf("Forfeit[%d]: %v", i, err)
				}
			}

			// Exactly one goroutine should have performed the actual release.
			released := 0
			for _, r := range results {
				if r != nil && !r.AlreadyNext {
					released++
				}
			}
			if released != 1 {
				t.Errorf("expected exactly 1 goroutine to release the quest, got %d", released)
			}
		})
	}
}
