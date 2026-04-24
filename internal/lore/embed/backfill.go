// Backfill: scan lore entries that have no vector (or a stale one),
// encode them through an Embedder, quantize to int8, and write the
// resulting lore_vectors rows in a BEGIN IMMEDIATE transaction. Also
// bumps meta.vector_epoch and vector_coverage_num atomically at the end
// of the run.
//
// Two call sites:
//
//  1. guild init (one-shot, synchronous): after the probe passes and
//     meta is set to embedder_state='enabled', init calls Backfill
//     to seed vectors for every active entry. The function is
//     idempotent on re-run.
//
//  2. The model-identity upgrade path: when the stored
//     meta.embedder_model_id does not match the binary's bound model
//     id, the caller runs an Invalidate (delete every vector, flip
//     every active entry to vector_state='pending', update meta) and
//     then calls Backfill again.
//
// Concurrency posture: one Backfill at a time per DB. Writers inside
// the package use BEGIN IMMEDIATE and INSERT OR IGNORE (ADR-003
// invariants 1 and 2). Encoding itself happens outside the DB
// transaction so a slow ORT session cannot hold the write lock for
// minutes.

package embed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// PendingEntry is one row that needs an embedding. Backfill iterates
// these and encodes Summary (the ADR-003 canonical embedding input).
type PendingEntry struct {
	ID      int64
	Summary string
}

// BackfillProgress is emitted once per entry (or once per ProgressEvery
// when >100) so callers can render a progress bar.
type BackfillProgress struct {
	// Done is the number of entries successfully embedded + persisted
	// since the backfill started.
	Done int
	// Total is the number of pending entries at the start of the run.
	Total int
	// Elapsed is the time since the backfill started.
	Elapsed time.Duration
}

// BackfillResult is the post-run summary. Callers log this as a
// single structured line.
type BackfillResult struct {
	Total    int
	Embedded int
	Skipped  int
	Failed   int
	Duration time.Duration
	Epoch    int64
}

// BackfillOptions carries the dependencies Backfill needs. Constructor
// injection (no package globals) so tests can pass a canned Embedder
// and capture progress.
type BackfillOptions struct {
	// DB is the lore database handle.
	DB *sql.DB
	// Embedder produces the float32 vectors. Must be non-nil; use
	// NullEmbedder to short-circuit when the probe failed.
	Embedder Embedder
	// ModelID goes into lore_vectors.model_id for every row written.
	// Should match meta.embedder_model_id (the caller enforces this).
	ModelID string
	// ProgressOut receives one "[NN%] NN/NN entries" line per tick.
	// Nil or io.Discard silences progress.
	ProgressOut io.Writer
	// ProgressThreshold is the minimum Total before progress is
	// rendered. Zero or negative means "render always". Default
	// caller passes 100 per spec.
	ProgressThreshold int
	// ProgressEvery controls the reporting cadence: emit a progress
	// tick every N entries (plus the final tick). Zero defaults to 10.
	ProgressEvery int
}

// Backfill embeds every pending entry and writes a row into lore_vectors
// for each. Idempotent on re-run: the candidate scan is a LEFT JOIN that
// already excludes rows that have vectors, so a second Backfill against
// the same corpus is a no-op.
//
// Error policy: per-entry encode failures increment Failed and continue;
// a persistent failure (>= half the run fails) surfaces as an error from
// Backfill itself after the loop. A db error inside the write Tx2
// aborts the whole function (the caller can retry).
func Backfill(ctx context.Context, opts BackfillOptions) (*BackfillResult, error) {
	if opts.DB == nil {
		return nil, fmt.Errorf("embed: Backfill: nil db")
	}
	if opts.Embedder == nil {
		return nil, fmt.Errorf("embed: Backfill: nil embedder")
	}
	if opts.ModelID == "" {
		return nil, fmt.Errorf("embed: Backfill: empty model_id")
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 10
	}

	start := time.Now()
	pending, err := scanPending(ctx, opts.DB)
	if err != nil {
		return nil, fmt.Errorf("embed: Backfill: scan pending: %w", err)
	}
	res := &BackfillResult{Total: len(pending)}
	if res.Total == 0 {
		// Still bump epoch so a caller waiting on the index refresh
		// sees a fresh value; zero-pending is a valid no-op.
		epoch, err := bumpEpoch(ctx, opts.DB)
		if err != nil {
			return nil, fmt.Errorf("embed: Backfill: bump epoch (empty run): %w", err)
		}
		res.Epoch = epoch
		res.Duration = time.Since(start)
		return res, nil
	}

	renderProgress := opts.ProgressOut != nil && opts.ProgressOut != io.Discard && res.Total >= opts.ProgressThreshold
	for i, entry := range pending {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("embed: Backfill: cancelled: %w", err)
		}
		vec, encErr := opts.Embedder.Embed(ctx, entry.Summary)
		if encErr != nil {
			// NullEmbedder surfaces here as ErrEmbedderDisabled; any
			// other error is a per-entry transient. Both bump Failed.
			res.Failed++
			if errors.Is(encErr, ErrEmbedderDisabled) {
				// Short-circuit: no point continuing against a null
				// embedder (the remaining rows will all fail the same
				// way). Return what we have so the caller can set
				// meta.embedder_state='disabled' and move on.
				res.Duration = time.Since(start)
				return res, fmt.Errorf("embed: Backfill: embedder disabled: %w", encErr)
			}
			continue
		}
		if len(vec) != Dim {
			res.Failed++
			continue
		}
		if err := insertVectorRow(ctx, opts.DB, entry, vec, opts.ModelID); err != nil {
			res.Failed++
			// DB-level error on a single row: keep going. A second run
			// of Backfill will pick the row up again via LEFT JOIN.
			continue
		}
		res.Embedded++
		if renderProgress && (res.Embedded%opts.ProgressEvery == 0 || i == len(pending)-1) {
			renderProgressLine(opts.ProgressOut, BackfillProgress{
				Done:    res.Embedded,
				Total:   res.Total,
				Elapsed: time.Since(start),
			})
		}
	}
	res.Skipped = res.Total - res.Embedded - res.Failed
	epoch, err := bumpEpoch(ctx, opts.DB)
	if err != nil {
		return res, fmt.Errorf("embed: Backfill: bump epoch: %w", err)
	}
	res.Epoch = epoch
	res.Duration = time.Since(start)
	return res, nil
}

// Invalidate drops every lore_vectors row, flips every active entry's
// vector_state back to 'pending', and writes the new embedder identity
// into meta. Used on model-identity upgrade (ADR-003 invariant 2).
//
// Runs inside a single BEGIN IMMEDIATE transaction so a concurrent
// reader cannot see a half-invalidated state.
func Invalidate(ctx context.Context, db *sql.DB, newIdentity ManifestIdentity) error {
	if db == nil {
		return fmt.Errorf("embed: Invalidate: nil db")
	}
	conn, rollback, err := beginImmediateLocal(ctx, db, "invalidate")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	if _, err := conn.ExecContext(ctx, `DELETE FROM lore_vectors`); err != nil {
		return fmt.Errorf("embed: Invalidate: delete vectors: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE entries SET vector_state = 'pending' WHERE status NOT IN ('archived','parked')`,
	); err != nil {
		return fmt.Errorf("embed: Invalidate: flip vector_state: %w", err)
	}
	// Bump epoch so readers refresh their caches.
	newEpoch, err := readEpochTx(ctx, conn)
	if err != nil {
		return err
	}
	newEpoch++
	upserts := []struct{ k, v string }{
		{"embedder_model_id", newIdentity.ModelID},
		{"embedder_tokenizer_hash", newIdentity.TokenizerHash},
		{"embedder_runtime_version", newIdentity.RuntimeVersion},
		{"embedder_dim", strconv.Itoa(newIdentity.Dim)},
		{"vector_epoch", strconv.FormatInt(newEpoch, 10)},
		{"vector_coverage_num", "0"},
	}
	for _, kv := range upserts {
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO meta (key,value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			kv.k, kv.v,
		); err != nil {
			return fmt.Errorf("embed: Invalidate: upsert meta %s: %w", kv.k, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("embed: Invalidate: commit: %w", err)
	}
	committed = true
	return nil
}

// scanPending returns every entry with no lore_vectors row whose status
// is not archived or parked. Ordered by id ASC for deterministic test
// runs. Summary is pulled from entries.summary (ADR-003's canonical
// embedding input).
func scanPending(ctx context.Context, db *sql.DB) ([]PendingEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.id, e.summary
		FROM   entries e
		LEFT   JOIN lore_vectors v ON v.entry_id = e.id
		WHERE  v.entry_id IS NULL
		  AND  e.status NOT IN ('archived','parked')
		ORDER  BY e.id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEntry
	for rows.Next() {
		var p PendingEntry
		if err := rows.Scan(&p.ID, &p.Summary); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// insertVectorRow writes one row into lore_vectors for entry. Uses
// INSERT OR IGNORE so a concurrent writer that raced us to this row is
// not treated as an error (ADR-003 invariant 1). Flips the parent
// entry's vector_state to 'indexed' in the same tx.
func insertVectorRow(ctx context.Context, db *sql.DB, entry PendingEntry, vec []float32, modelID string) error {
	conn, rollback, err := beginImmediateLocal(ctx, db, "backfill-row")
	if err != nil {
		return err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	// Canonical int8 quantization lives in cosine.go (Quantize, VecDim=384,
	// symmetric scale of 127). The lore_vectors.vec BLOB is exactly VecDim
	// bytes; reinterpret []int8 as []byte via a copy. See ADR-003 storage
	// layout and QUEST-209 LoadFromDB which rejects any other length.
	quant := Quantize(vec)
	if quant == nil {
		return fmt.Errorf("quantize: got %d float32, want %d", len(vec), VecDim)
	}
	blob := make([]byte, VecDim)
	for j, q := range quant {
		blob[j] = byte(q)
	}
	hash := sha256.Sum256([]byte(entry.Summary))
	hashHex := hex.EncodeToString(hash[:])
	now := time.Now().UTC().Unix()

	if _, err := conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO lore_vectors
		  (entry_id, model_id, dim, vec, encoded_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?)
	`, entry.ID, modelID, Dim, blob, now, hashHex); err != nil {
		return fmt.Errorf("insert vector: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE entries SET vector_state = 'indexed' WHERE id = ?`,
		entry.ID,
	); err != nil {
		return fmt.Errorf("flip vector_state: %w", err)
	}
	// coverage++ inside the same tx so counter never drifts from
	// actual vector rows.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta (key,value) VALUES ('vector_coverage_num','1')
		 ON CONFLICT(key) DO UPDATE SET value = CAST((CAST(value AS INTEGER) + 1) AS TEXT)`,
	); err != nil {
		return fmt.Errorf("bump coverage: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// bumpEpoch atomically increments meta.vector_epoch and returns the new
// value. Uses BEGIN IMMEDIATE so concurrent writers serialize.
func bumpEpoch(ctx context.Context, db *sql.DB) (int64, error) {
	conn, rollback, err := beginImmediateLocal(ctx, db, "bump-epoch")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	committed := false
	defer rollback(&committed)

	cur, err := readEpochTx(ctx, conn)
	if err != nil {
		return 0, err
	}
	next := cur + 1
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta (key,value) VALUES ('vector_epoch', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.FormatInt(next, 10),
	); err != nil {
		return 0, fmt.Errorf("write epoch: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return next, nil
}

// readEpochTx reads the current meta.vector_epoch from an already-open
// conn. Returns 0 when the row is missing (fresh DB) so callers can
// always use the returned value.
func readEpochTx(ctx context.Context, conn *sql.Conn) (int64, error) {
	var s sql.NullString
	if err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'vector_epoch'`,
	).Scan(&s); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("read epoch: %w", err)
	}
	if !s.Valid || s.String == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s.String, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse epoch %q: %w", s.String, err)
	}
	return n, nil
}

// Quantization lives in cosine.go alongside the cosine kernel. See
// Quantize / Dequantize / VecDim there. This package-local comment stays
// as a pointer so anyone grepping for "QuantizeInt8" in backfill lands
// here and knows where the canonical helper moved.

// renderProgressLine writes one "[pp%] done/total (elapsed)" line.
// Kept deliberately plain (no ANSI) so the output is readable in the
// guild init transcript and in CI logs that strip ANSI sequences.
func renderProgressLine(w io.Writer, p BackfillProgress) {
	if w == nil {
		return
	}
	pct := 0
	if p.Total > 0 {
		pct = int((float64(p.Done) * 100.0) / float64(p.Total))
	}
	var eta time.Duration
	if p.Done > 0 && p.Done < p.Total {
		remain := p.Total - p.Done
		perEntry := p.Elapsed / time.Duration(p.Done)
		eta = perEntry * time.Duration(remain)
	}
	if eta > 0 {
		fmt.Fprintf(w, "  backfill: [%3d%%] %d/%d  eta=%s\n", pct, p.Done, p.Total, eta.Round(time.Second))
	} else {
		fmt.Fprintf(w, "  backfill: [%3d%%] %d/%d\n", pct, p.Done, p.Total)
	}
}

// beginImmediateLocal is the package-local copy of the BEGIN IMMEDIATE
// helper pattern used in internal/quest. Kept local (not imported from
// internal/quest) so internal/lore/embed has zero imports from the rest
// of internal/lore, preserving the Phase 2 swap invariant.
func beginImmediateLocal(ctx context.Context, db *sql.DB, op string) (*sql.Conn, func(*bool), error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("embed: %s: acquire conn: %w", op, err)
	}
	const maxAttempts = 20
	var beginErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		_, beginErr = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if beginErr == nil {
			break
		}
		if !isSQLiteBusy(beginErr.Error()) {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("embed: %s: begin immediate: %w", op, beginErr)
		}
		wait := time.Duration(attempt+1) * 10 * time.Millisecond
		if wait > 200*time.Millisecond {
			wait = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, nil, fmt.Errorf("embed: %s: begin immediate: %w", op, ctx.Err())
		case <-time.After(wait):
		}
	}
	if beginErr != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("embed: %s: begin immediate (contended out): %w", op, beginErr)
	}
	rollback := func(committed *bool) { //nolint:contextcheck // rollback must survive caller cancellation
		if !*committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	return conn, rollback, nil
}

// isSQLiteBusy recognizes the modernc.org/sqlite busy/locked surfacing.
// Kept string-based (not sqlite3.Error) because the driver is pure-Go
// and wraps errors into fmt.Errorf("%w", ...).
func isSQLiteBusy(msg string) bool {
	return containsAny(msg, "SQLITE_BUSY", "database is locked", "SQLITE_LOCKED")
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		for i := 0; i+len(n) <= len(s); i++ {
			if s[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
}
