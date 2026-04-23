package lore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrInvalidRelation is returned by Link when the caller passes a
// relation outside the Relation enum.
var ErrInvalidRelation = errors.New("lore: invalid relation")

// ErrSelfLink is returned when fromID equals toID. entry_links has a
// composite primary key (from_id, to_id) that allows it at the DB
// level, but a self-edge is never meaningful provenance and is almost
// always a CLI typo.
var ErrSelfLink = errors.New("lore: cannot link entry to itself")

// LinkEntries inserts a row into entry_links expressing `from informs
// to` (or supersedes/contradicts if the caller overrides rel). The design
// explicitly permits cross-project links: entry_links has no project_id
// column and the lookup is WHERE id = ? with no project filter, so agents
// can build provenance graphs that span projects.
//
// Note: the function is named LinkEntries rather than Link because
// the domain package already exports a `Link` struct type (one row
// of entry_links) — re-using the same identifier for both would
// shadow the type. The CLI surface still presents the verb as
// `lore link`.
//
// INSERT OR IGNORE: re-linking an already-linked pair is a no-op success
// (not a UNIQUE-violation error), which keeps `lore link` idempotent
// under retries.
func LinkEntries(ctx context.Context, db *sql.DB, fromID, toID int64, rel Relation) error {
	if db == nil {
		return fmt.Errorf("lore: link: nil db")
	}
	if rel == "" {
		rel = RelationInforms
	}
	if !isValidRelation(rel) {
		return fmt.Errorf("%w: %q", ErrInvalidRelation, string(rel))
	}
	if fromID == toID {
		return fmt.Errorf("%w: %s", ErrSelfLink, formatEntryID(fromID))
	}

	// Verify both entries exist in SOME project (cross-project allowed).
	if err := requireEntryExists(ctx, db, fromID); err != nil {
		return err
	}
	if err := requireEntryExists(ctx, db, toID); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO entry_links (from_id, to_id, relation)
		 VALUES (?, ?, ?)`,
		fromID, toID, string(rel),
	); err != nil {
		return fmt.Errorf("lore: link: insert: %w", err)
	}
	return nil
}

type entryLookup interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// requireEntryExists returns nil if id exists in entries (regardless of
// project), or a wrapped ErrEntryNotFound otherwise. Used by Link and
// Reforge to reject IDs before doing the mutation.
func requireEntryExists(ctx context.Context, db entryLookup, id int64) error {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM entries WHERE id = ?`, id,
	).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrEntryNotFound, EntryID(id))
		}
		return fmt.Errorf("lore: lookup %s: %w", EntryID(id), err)
	}
	return nil
}

// validRelations is the allowlist enforced by Link/Reforge. Kept as a
// map so callers can add new relations in one place without touching
// this function.
var validRelations = map[Relation]struct{}{
	RelationInforms:     {},
	RelationSupersedes:  {},
	RelationContradicts: {},
}

func isValidRelation(r Relation) bool {
	_, ok := validRelations[r]
	return ok
}
