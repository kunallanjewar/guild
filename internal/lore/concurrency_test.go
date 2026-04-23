package lore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestReforge_ConcurrentPairsAllSucceed exercises concurrent reforge writes on
// distinct entry pairs. Every pair should commit without SQLITE_BUSY leaks, and
// each old entry should end superseded with exactly one supersedes edge.
func TestReforge_ConcurrentPairsAllSucceed(t *testing.T) {
	const n = 20

	ctx := context.Background()
	db := openTestDB(t, "alpha")

	type pair struct {
		oldID int64
		newID int64
	}
	pairs := make([]pair, n)
	for i := range pairs {
		oldE, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: "alpha",
			Kind:      KindDecision,
			Title:     fmt.Sprintf("old reforge target %d", i),
			Summary:   "summary",
			Topic:     "concurrency",
		})
		if err != nil {
			t.Fatalf("inscribe old %d: %v", i, err)
		}
		newE, err := Inscribe(ctx, db, &InscribeParams{
			ProjectID: "alpha",
			Kind:      KindDecision,
			Title:     fmt.Sprintf("new reforge target %d", i),
			Summary:   "summary",
			Topic:     "concurrency",
		})
		if err != nil {
			t.Fatalf("inscribe new %d: %v", i, err)
		}
		pairs[i] = pair{oldID: oldE.Entry.ID, newID: newE.Entry.ID}
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range pairs {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = Reforge(ctx, db, pairs[i].oldID, pairs[i].newID, time.Time{})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Reforge[%d]: %v", i, err)
		}
	}

	for _, p := range pairs {
		var status string
		if err := db.QueryRowContext(ctx,
			`SELECT status FROM entries WHERE id = ?`,
			p.oldID,
		).Scan(&status); err != nil {
			t.Fatalf("load old entry %d: %v", p.oldID, err)
		}
		if status != string(StatusSuperseded) {
			t.Errorf("old entry %d status=%q, want superseded", p.oldID, status)
		}
		var links int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entry_links
			 WHERE from_id = ? AND to_id = ? AND relation = 'supersedes'`,
			p.newID, p.oldID,
		).Scan(&links); err != nil {
			t.Fatalf("count links %d->%d: %v", p.newID, p.oldID, err)
		}
		if links != 1 {
			t.Errorf("supersedes links for %d->%d = %d, want 1", p.newID, p.oldID, links)
		}
	}
}

// TestAppraise_ConcurrentAccessCounterBumps runs N concurrent appraise calls
// against the same entry and asserts the telemetry write path records every
// access. Appraise intentionally swallows bump errors, so the count is the
// only proof the write path survived contention.
func TestAppraise_ConcurrentAccessCounterBumps(t *testing.T) {
	const n = 20

	ctx := context.Background()
	db := openTestDB(t)
	defer func() { _ = db.Close() }()

	ids := seedCorpus(t, ctx, db, []fixtureEntry{
		{"p", "research", "counter contention entry", "summary", "t"},
	})

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = Appraise(ctx, db, AppraiseParams{
				Query:       "counter contention",
				AllProjects: true,
				Now:         time.Now().UTC(),
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Appraise[%d]: %v", i, err)
		}
	}

	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT access_count FROM entries WHERE id = ?`,
		ids[0],
	).Scan(&count); err != nil {
		t.Fatalf("scan access_count: %v", err)
	}
	if count != n {
		t.Fatalf("access_count=%d, want %d", count, n)
	}
}
