package concurrency_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mathomhaus/guild/internal/quest"
)

// Test_ConcurrentSummon_AllTransfersPersist fires N goroutines that each summon
// a distinct in-progress quest to a new owner. Explicit multi-statement write
// txs previously risked SQLITE_BUSY under pooled contention; after BEGIN
// IMMEDIATE all transfers should succeed and persist their final owner.
func Test_ConcurrentSummon_AllTransfersPersist(t *testing.T) {
	const n = 20

	db, pid := newTestDB(t)
	ctx := context.Background()

	quests := make([]*quest.Quest, n)
	for i := range quests {
		quests[i] = mustPost(t, db, pid, quest.PostParams{
			Subject: fmt.Sprintf("summon-target-%d", i),
		})
		mustAccept(t, db, pid, quests[i].ID, "owner")
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range quests {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = quest.Summon(ctx, db, pid, quests[i].ID, fmt.Sprintf("agent-%d", i), "owner")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Summon[%d]: %v", i, err)
		}
	}
	for i, q := range quests {
		got, err := quest.Load(ctx, db, pid, q.ID)
		if err != nil {
			t.Fatalf("Load %s: %v", q.ID, err)
		}
		wantOwner := fmt.Sprintf("agent-%d", i)
		if got.Owner != wantOwner {
			t.Errorf("%s owner=%q, want %q", q.ID, got.Owner, wantOwner)
		}
		if got.Status != quest.StatusInProgress {
			t.Errorf("%s status=%s, want in_progress", q.ID, got.Status)
		}
	}
}

// Test_ConcurrentEpic_AllWritesPersist fires N goroutines that each apply a
// distinct epic to a distinct quest. The invariant is simple: every write
// succeeds and each quest replays its assigned epic without SQLITE_BUSY leaks.
func Test_ConcurrentEpic_AllWritesPersist(t *testing.T) {
	const n = 20

	db, pid := newTestDB(t)
	ctx := context.Background()

	quests := make([]*quest.Quest, n)
	for i := range quests {
		quests[i] = mustPost(t, db, pid, quest.PostParams{
			Subject: fmt.Sprintf("epic-target-%d", i),
		})
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range quests {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = quest.SetEpic(ctx, db, pid, fmt.Sprintf("campaign-%d", i), []string{quests[i].ID})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("SetEpic[%d]: %v", i, err)
		}
	}
	for i, q := range quests {
		got, err := quest.Load(ctx, db, pid, q.ID)
		if err != nil {
			t.Fatalf("Load %s: %v", q.ID, err)
		}
		wantEpic := fmt.Sprintf("campaign-%d", i)
		if got.Epic != wantEpic {
			t.Errorf("%s epic=%q, want %q", q.ID, got.Epic, wantEpic)
		}
	}
}

// Test_ConcurrentJournal_AllNotesPersist appends N distinct notes to the same
// quest concurrently. The success condition is no SQLITE_BUSY plus every note
// visible in Scroll afterward.
func Test_ConcurrentJournal_AllNotesPersist(t *testing.T) {
	const n = 20

	db, pid := newTestDB(t)
	ctx := context.Background()
	q := mustPost(t, db, pid, quest.PostParams{Subject: "journal-target"})

	texts := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		texts[i] = fmt.Sprintf("journal-note-%d", i)
		go func() {
			defer wg.Done()
			errs[i] = quest.Journal(ctx, db, pid, q.ID, "agent", texts[i])
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Journal[%d]: %v", i, err)
		}
	}

	scroll, err := quest.Scroll(ctx, db, pid, q.ID)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	seen := map[string]bool{}
	for _, n := range scroll.Notes {
		seen[n.Note] = true
	}
	for _, text := range texts {
		if !seen[text] {
			t.Errorf("missing journal note %q after concurrent writes", text)
		}
	}
}

// Test_ConcurrentCampfire_AllSnapshotsPersist appends N distinct checkpoint
// snapshots to the same quest concurrently. Each checkpoint note should survive
// the contention window.
func Test_ConcurrentCampfire_AllSnapshotsPersist(t *testing.T) {
	const n = 20

	db, pid := newTestDB(t)
	ctx := context.Background()
	q := mustPost(t, db, pid, quest.PostParams{Subject: "campfire-target"})

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = quest.Campfire(ctx, db, pid, q.ID, quest.CampfireParams{
				Next:  fmt.Sprintf("resume-%d", i),
				Agent: "agent",
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Campfire[%d]: %v", i, err)
		}
	}

	scroll, err := quest.Scroll(ctx, db, pid, q.ID)
	if err != nil {
		t.Fatalf("Scroll: %v", err)
	}
	for i := range n {
		want := fmt.Sprintf("next: resume-%d", i)
		found := false
		for _, note := range scroll.Notes {
			if strings.Contains(note.Note, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing campfire snapshot containing %q", want)
		}
	}
}

// Test_ConcurrentRestore_IdempotentUnderContention fires N concurrent restore
// calls for the same snapshot into the same target DB. With a serialized write
// tx, exactly one caller should import the task_status row and the others
// should observe it as already present rather than racing into constraint
// failures.
func Test_ConcurrentRestore_IdempotentUnderContention(t *testing.T) {
	const n = 10

	ctx := context.Background()

	srcDB, srcPID := newTestDB(t)
	mustPost(t, srcDB, srcPID, quest.PostParams{Subject: "restore contention sentinel"})

	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")
	if err := quest.Archive(ctx, srcDB, srcPID, snapshotPath); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	dstDB, dstPID := newTestDB(t)
	results := make([]*quest.RestoreResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			results[i], errs[i] = quest.Restore(ctx, dstDB, dstPID, snapshotPath)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Restore[%d]: %v", i, err)
		}
	}

	imported := 0
	for _, r := range results {
		if r != nil {
			imported += r.TasksImported
		}
	}
	if imported != 1 {
		t.Errorf("concurrent restore imported %d tasks total, want exactly 1", imported)
	}

	var count int
	if err := dstDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_status WHERE project_id = ?`,
		dstPID,
	).Scan(&count); err != nil {
		t.Fatalf("count restored tasks: %v", err)
	}
	if count != 1 {
		t.Errorf("restored task_status rows=%d, want 1", count)
	}
}
