package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// BeginImmediate acquires a dedicated *sql.Conn from db, then issues
// BEGIN IMMEDIATE with a capped-backoff retry loop.
//
// Callers that open a write transaction with BEGIN DEFERRED take a read
// snapshot the first time they touch the DB. Under concurrent writers this
// can surface SQLITE_BUSY mid-transaction or let two goroutines derive their
// writes from stale state. BEGIN IMMEDIATE serializes writers at
// lock-acquisition time instead.
//
// errPrefix should name the calling operation (for example "quest: post").
// The returned rollback function must be called with the caller's committed
// flag, and conn.Close() must be deferred by the caller.
func BeginImmediate(ctx context.Context, db *sql.DB, errPrefix string) (*sql.Conn, func(*bool), error) {
	if errPrefix == "" {
		errPrefix = "storage"
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: acquire conn: %w", errPrefix, err)
	}

	const maxAttempts = 20
	var beginErr error
	for attempt := range maxAttempts {
		_, beginErr = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if beginErr == nil {
			break
		}
		if !isBusyErr(beginErr.Error()) {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("%s: begin immediate: %w", errPrefix, beginErr)
		}

		// Capped linear backoff: 10ms × (attempt+1), max 200ms.
		base := time.Duration(attempt+1) * 10 * time.Millisecond
		if base > 200*time.Millisecond {
			base = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, nil, fmt.Errorf("%s: begin immediate: %w", errPrefix, ctx.Err())
		case <-time.After(base):
		}
	}
	if beginErr != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("%s: begin immediate (contended out after %d attempts): %w",
			errPrefix, maxAttempts, beginErr)
	}

	rollback := func(committed *bool) { //nolint:contextcheck // rollback must survive caller cancellation
		if !*committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	return conn, rollback, nil
}

func isBusyErr(msg string) bool {
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database is locked")
}
