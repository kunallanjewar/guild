package quest

import (
	"context"
	"database/sql"

	"github.com/mathomhaus/guild/internal/storage"
)

// beginImmediate is the quest-scoped wrapper over storage.BeginImmediate so
// quest callers get consistent "quest: <op>" error prefixes.
func beginImmediate(ctx context.Context, db *sql.DB, opName string) (*sql.Conn, func(*bool), error) {
	return storage.BeginImmediate(ctx, db, "quest: "+opName)
}
