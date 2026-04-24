package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

// TestAutoBackfill_EndToEnd is the QUEST-229 regression gate: a post-
// upgrade user boots an MCP server against a DB where lore has pending
// entries and quest_vectors is entirely empty. The provider's first
// wired resolve fires maybeTriggerAutoBackfill; within seconds the
// goroutines drive both corpora to full coverage.
//
// The test uses a DeterministicEmbedder so it works on default builds
// (no -tags=withembed required) and seeds both DBs through the same
// migration path production uses.
func TestAutoBackfill_EndToEnd(t *testing.T) {
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	// Reset package-level state so this test sees a clean once-guard.
	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	// Redirect the openLoreDB / openQuestDB path resolvers so the
	// init()-registered targets find our temp DBs.
	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	// Seed lore.db: 5 pending entries, embedder_state='enabled'.
	seedLoreForAutoBackfill(t, loreDBPath, 5)

	// Seed quest.db: 3 task_status rows with [spec] notes. quest_vectors
	// starts empty (the whole point of QUEST-229).
	seedQuestForAutoBackfill(t, questDBPath, 3)

	// Pin a JSON-handler slog to a buffer so we can assert the
	// "auto-backfill started" / "auto-backfill complete" lines.
	var logBuf safeBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Construct a *lore.EmbedDeps by hand. In production the provider
	// reconstruct path returns one of these via lore.WireEmbedDeps +
	// PrepareAndProbe; for the test we synthesize an equivalent deps
	// carrying a DeterministicEmbedder so the default-build test runner
	// does not need bundled ORT assets.
	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	// Fire the trigger. Non-blocking: returns immediately.
	start := time.Now()
	maybeTriggerAutoBackfill(deps, logger)

	// Wait for completion (bounded). The sync.WaitGroup inside
	// runAutoBackfill closes autoBackfillDoneCh when every per-corpus
	// goroutine finishes.
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("auto-backfill did not finish within 30s; logs:\n%s", logBuf.String())
	}
	totalElapsed := time.Since(start)
	t.Logf("auto-backfill total wall time: %s", totalElapsed)

	// Assert: lore coverage == 1.0 (5/5).
	assertCorpusCoverage(t, loreDBPath, embed.LoreCorpus{}, 5, 5)
	// Assert: quest coverage == 1.0 (3/3). QuestCorpus coverage_den is
	// reconciled by Backfill's ReconcileDen step; we seeded 3 rows and
	// expect 3 indexed.
	assertCorpusCoverage(t, questDBPath, embed.QuestCorpus{}, 3, 3)

	// Assert: slog samples contain the started+complete lines for both
	// corpora. Parse each JSON line and accumulate the (msg, corpus)
	// pairs.
	got := collectAutoBackfillEvents(t, logBuf.String())
	mustHaveEvent(t, got, "auto-backfill started", "lore")
	mustHaveEvent(t, got, "auto-backfill started", "quest")
	mustHaveEvent(t, got, "auto-backfill complete", "lore")
	mustHaveEvent(t, got, "auto-backfill complete", "quest")

	// Print a sample transcript so the quest_fulfill report can cite
	// the slog text verbatim.
	t.Logf("slog sample:\n%s", logBuf.String())
}

// TestAutoBackfill_ExactlyOnce proves the sync.Once gate: ten concurrent
// triggers produce exactly one goroutine per corpus, not ten.
func TestAutoBackfill_ExactlyOnce(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	seedLoreForAutoBackfill(t, loreDBPath, 2)
	seedQuestForAutoBackfill(t, questDBPath, 2)

	// Counting embedder: wraps DeterministicEmbedder and atomically
	// increments on every Embed call. Exactly-once trigger means the
	// counter hits (pending_lore + pending_quest) once, not twice.
	counted := &countingEmbedder{inner: embed.NewDeterministicEmbedder()}

	logger := slog.New(slog.NewJSONHandler(newSafeBuffer(), &slog.HandlerOptions{Level: slog.LevelInfo}))
	deps := &lore.EmbedDeps{
		Embedder: counted,
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Async:    true,
		Logger:   logger,
	}

	// Race ten goroutines through maybeTriggerAutoBackfill. sync.Once
	// must serialize them.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			maybeTriggerAutoBackfill(deps, logger)
		}()
	}
	wg.Wait()

	select {
	case <-autoBackfillDoneCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("auto-backfill did not finish within 10s")
	}

	// Expected encodes: 2 lore + 2 quest = 4. Anything higher is a
	// sync.Once regression.
	got := counted.count.Load()
	if got != 4 {
		t.Errorf("embed count: got %d, want 4 (2 lore + 2 quest). sync.Once may be leaking", got)
	}
	_ = ctx
}

// TestAutoBackfill_NoOpWhenCovered verifies the gate: a corpus already
// at >= 0.90 coverage with zero pending entries does not spawn a
// goroutine and does not emit the "auto-backfill started" line.
func TestAutoBackfill_NoOpWhenCovered(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	loreDBPath := filepath.Join(tmp, "lore.db")
	questDBPath := filepath.Join(tmp, "quest.db")

	resetAutoBackfillState()
	t.Cleanup(resetAutoBackfillState)

	origLdb := ldbPath
	origQdb := qdbPath
	ldbPath = func() (string, error) { return loreDBPath, nil }
	qdbPath = func() (string, error) { return questDBPath, nil }
	t.Cleanup(func() {
		ldbPath = origLdb
		qdbPath = origQdb
	})

	// Seed lore with zero pending entries and coverage=1/1.
	seedEmptyLoreAlreadyCovered(t, loreDBPath)
	// Seed quest with zero pending entities.
	seedEmptyQuestAlreadyCovered(t, questDBPath)

	var logBuf safeBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	deps := &lore.EmbedDeps{
		Embedder: embed.NewDeterministicEmbedder(),
		ModelID:  "bge-small-en-v1.5-int8-cls",
		Logger:   logger,
	}

	maybeTriggerAutoBackfill(deps, logger)
	select {
	case <-autoBackfillDoneCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("auto-backfill did not finish within 5s")
	}
	if strings.Contains(logBuf.String(), `"auto-backfill started"`) {
		t.Errorf("expected silence for already-covered corpora; got started line:\n%s", logBuf.String())
	}
	_ = ctx
}

// ----- helpers --------------------------------------------------------

// safeBuffer is a concurrency-safe bytes.Buffer wrapper. Multiple
// goroutines log into the same buffer during auto-backfill, so the test
// logger needs a synchronized writer. bytes.Buffer.Write alone is not
// goroutine-safe.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSafeBuffer() *safeBuffer { return &safeBuffer{} }

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// countingEmbedder wraps an Embedder and counts calls atomically. Used
// to prove sync.Once collapses N concurrent triggers to one pass.
type countingEmbedder struct {
	inner embed.Embedder
	count atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.count.Add(1)
	return c.inner.Embed(ctx, text)
}

func (c *countingEmbedder) Dimension() int { return c.inner.Dimension() }

func seedLoreForAutoBackfill(t *testing.T, path string, pendingCount int) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("migrate lore: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// Insert pending entries.
	for i := 1; i <= pendingCount; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO entries
			   (project_id, topic, kind, title, summary, status, vector_state, created_at, updated_at)
			 VALUES ('p', 'topic', 'observation', 'title', ?, 'current', 'pending', datetime('now'), datetime('now'))`,
			"summary text "+strings.Repeat("x", i),
		)
		if err != nil {
			t.Fatalf("seed entry %d: %v", i, err)
		}
	}
	// Mark lore embedder as enabled so the per-corpus promotion path
	// recognizes the state is already good and skips the identity
	// rewrite.
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "embedder_model_id", "bge-small-en-v1.5-int8-cls")
	upsertMeta(t, db, "vector_coverage_num", "0")
	upsertMeta(t, db, "vector_coverage_den", "0")
}

func seedQuestForAutoBackfill(t *testing.T, path string, count int) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("migrate quest: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO projects (id, path) VALUES ('p', '/tmp/p')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for i := 1; i <= count; i++ {
		taskID := "QUEST-AB" + strings.Repeat("0", 3-len(intToStr(i))) + intToStr(i)
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_status (project_id, task_id, status) VALUES ('p', ?, 'next')`,
			taskID,
		); err != nil {
			t.Fatalf("seed task_status %s: %v", taskID, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO task_notes (project_id, task_id, agent_id, note)
			 VALUES ('p', ?, 'test', ?)`,
			taskID, "[spec] subject: auto-backfill test quest "+intToStr(i),
		); err != nil {
			t.Fatalf("seed task_notes %s: %v", taskID, err)
		}
		// Trigger tasks_fts_status_ai populates tasks_fts_rows; guard
		// with INSERT OR IGNORE so an already-populated row is fine.
		if _, err := db.ExecContext(ctx,
			`INSERT OR IGNORE INTO tasks_fts_rows (task_id) VALUES (?)`,
			taskID,
		); err != nil {
			t.Fatalf("seed tasks_fts_rows %s: %v", taskID, err)
		}
	}
	// Leave quest.embedder_state at its migration-005 default
	// ('disabled'); the ensureCorpusStateEnabled helper in the auto-
	// backfill path must promote it. This mirrors the upgrade scenario
	// QUEST-229 targets.
}

func seedEmptyLoreAlreadyCovered(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open lore db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("migrate lore: %v", err)
	}
	upsertMeta(t, db, "embedder_state", "enabled")
	upsertMeta(t, db, "embedder_model_id", "bge-small-en-v1.5-int8-cls")
	upsertMeta(t, db, "vector_coverage_num", "0")
	upsertMeta(t, db, "vector_coverage_den", "0")
}

func seedEmptyQuestAlreadyCovered(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := storage.Migrate(ctx, db, "quest"); err != nil {
		t.Fatalf("migrate quest: %v", err)
	}
	// No tasks_fts_rows seeded; coverage_den stays zero via migration
	// seeds. assessCorpus treats den==0 as fully covered.
}

func upsertMeta(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		t.Fatalf("upsertMeta(%s=%s): %v", key, value, err)
	}
}

func assertCorpusCoverage(t *testing.T, path string, corpus embed.VectorCorpus, wantNum, wantDen int64) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open db for coverage assert: %v", err)
	}
	defer func() { _ = db.Close() }()

	var num, den int64
	for _, row := range []struct {
		field embed.MetaField
		dst   *int64
	}{
		{embed.FieldVectorCoverageNum, &num},
		{embed.FieldVectorCoverageDen, &den},
	} {
		var v string
		err := db.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, corpus.MetaKey(row.field),
		).Scan(&v)
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("read coverage %v: %v", corpus.MetaKey(row.field), err)
		}
		if v == "" {
			continue
		}
		n, perr := parseInt64(v)
		if perr != nil {
			t.Fatalf("parse coverage %s=%q: %v", corpus.MetaKey(row.field), v, perr)
		}
		*row.dst = n
	}
	if num != wantNum || den != wantDen {
		t.Errorf("%s coverage: got %d/%d, want %d/%d", corpus.Name(), num, den, wantNum, wantDen)
	}
}

type autoBackfillEvent struct {
	Msg        string `json:"msg"`
	CorpusName string `json:"corpus_name"`
}

func collectAutoBackfillEvents(t *testing.T, logs string) []autoBackfillEvent {
	t.Helper()
	var out []autoBackfillEvent
	for _, line := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev autoBackfillEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if strings.HasPrefix(ev.Msg, "auto-backfill ") {
			out = append(out, ev)
		}
	}
	return out
}

func mustHaveEvent(t *testing.T, events []autoBackfillEvent, msg, corpus string) {
	t.Helper()
	for _, ev := range events {
		if ev.Msg == msg && ev.CorpusName == corpus {
			return
		}
	}
	t.Errorf("missing event: msg=%q corpus=%q; got events=%+v", msg, corpus, events)
}

// intToStr is a local itoa to avoid importing strconv for one call.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
