package lore

import (
	"context"
	"database/sql"

	"github.com/mathomhaus/guild/internal/storage"
)

// beginImmediate is the lore-scoped wrapper over storage.BeginImmediate so
// lore callers get consistent "lore: <op>" error prefixes.
func beginImmediate(ctx context.Context, db *sql.DB, opName string) (*sql.Conn, func(*bool), error) {
	return storage.BeginImmediate(ctx, db, "lore: "+opName)
}
