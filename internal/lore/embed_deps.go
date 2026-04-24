package lore

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore/embed"
)

// embedFromDeps extracts the *EmbedDeps the adapter layer (MCP or CLI)
// stashed into command.Deps.Embed. command.Deps carries Embed as `any`
// to keep the command package free of a lore dependency; this helper
// centralizes the type assertion so every lore handler pulls it the
// same way and the failure mode (nil on mismatch) is uniform. A nil
// return is the documented Phase-0 fallback path.
func embedFromDeps(d command.Deps) *EmbedDeps {
	if d.Embed == nil {
		return nil
	}
	e, _ := d.Embed.(*EmbedDeps)
	return e
}

// EmbedDeps bundles the optional embedding-pipeline dependencies that
// the lore write and read paths accept. All fields are optional; a
// fully-zeroed EmbedDeps (or a nil *EmbedDeps) means "no embedder is
// configured" and the package behaves exactly like Phase 0 (BM25 +
// stopwords only).
//
// The indirection exists because lore is a long-lived domain package
// used by both the MCP server (long-lived, warmed index, async Tx2)
// and the CLI surface (short-lived, no index, sync Tx2). The same
// Inscribe / Update / Reforge / Appraise functions serve both
// surfaces; the caller picks the concurrency shape via Async.
//
// Ownership:
//
//   - The caller (cmd/guild or internal/mcp) constructs EmbedDeps
//     once per process and threads it through. EmbedDeps is NOT a
//     package global.
//   - QUEST-210 wires this at the CLI surface; QUEST-211 wires the
//     MCP surface. Until those land, the lore public API accepts nil
//     and remains backwards compatible.
//
// Hexagonal: the types live in internal/lore/embed (adapter) and
// this struct is the port that internal/lore consumes.
type EmbedDeps struct {
	// Embedder is the sentence embedder the hot path encodes
	// summaries with. nil means no vector writes happen.
	Embedder embed.Embedder

	// Index is the per-process in-memory index the hot path splices
	// into after a successful write, and the read path queries. nil
	// is the CLI-surface default: a short-lived process does not
	// pay the index warm cost.
	Index *embed.Index

	// ModelID is the binary's canonical model_id, compared against
	// meta.embedder_model_id at Tx2 open. Empty string disables the
	// vector write path for safety.
	ModelID string

	// Async controls whether inscribe/update/reforge run the Tx2
	// vector write in a fire-and-forget goroutine (true, MCP surface)
	// or synchronously in the caller's goroutine (false, CLI surface).
	// The row-insert transaction always commits first; Async only
	// affects the secondary vector-write transaction.
	Async bool

	// Logger receives structured diagnostic lines from the hot path.
	// nil defaults to slog.Default.
	Logger *slog.Logger
}

// Enabled reports whether the caller has wired enough of EmbedDeps to
// actually write vectors. Absence (nil pointer or empty Embedder) is
// the BM25-only fallback path per ADR-003.
func (d *EmbedDeps) Enabled() bool {
	return d != nil && d.Embedder != nil && d.ModelID != ""
}

// hotDeps converts EmbedDeps into the subset the embed package's
// WriteVector helper expects. Called only when Enabled() is true.
func (d *EmbedDeps) hotDeps() embed.HotDeps {
	return embed.HotDeps{
		Embedder: d.Embedder,
		Index:    d.Index,
		ModelID:  d.ModelID,
		Logger:   d.Logger,
	}
}

// runTx2 dispatches the vector write according to d.Async. Always a
// no-op when d is nil or not Enabled. Logs once on failure via
// d.Logger (or slog.Default) so the caller does not have to.
//
// The async path spawns a goroutine that uses context.Background() so
// caller cancellation (e.g. an HTTP timeout on the MCP surface) does
// not abort an in-flight vector write. ADR-003 "Dataflow: MCP surface"
// is explicit that Tx2 failure is not a user-visible error.
func (d *EmbedDeps) runTx2(ctx context.Context, db *sql.DB, entryID int64, summary string) {
	if !d.Enabled() {
		return
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	write := func(bgCtx context.Context) {
		if _, err := embed.WriteVector(bgCtx, db, d.hotDeps(), entryID, summary); err != nil {
			logger.Warn("lore: Tx2 vector write failed",
				"entry_id", entryID,
				"err", err,
			)
		}
	}
	if d.Async {
		//nolint:contextcheck // Tx2 must survive caller cancellation; ADR-003 "Dataflow: MCP surface" is explicit that the background write uses a detached context.
		go write(context.Background())
		return
	}
	write(ctx)
}
