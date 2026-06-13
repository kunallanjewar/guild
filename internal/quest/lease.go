package quest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mathomhaus/guild/internal/command"
)

// Quest leases are the liveness layer for daemon-mediated claims
// (ADR-005 Part 1, "Why a daemon" item 3). A bare claim on task_status
// (claimed_by, claimed_at) carries no notion of whether the claimant is
// still alive, so a crashed agent's in_progress quest rots as a zombie.
// A daemon that sees every session holds a lease with a heartbeat; when
// the heartbeat stops the lease expires and a watcher can forfeit the
// stale claim. The schema is internal/storage/migrations/011_task_leases.
//
// This file is the schema + acquire/renew/release/expiry primitives
// only. Heartbeat scheduling, reaping, and presence are follow-on quests
// in the daemon Phase 3 campaign; the TTL/interval constants below are
// hardcoded here and lift into config when the heartbeat quest lands.
//
// HARD INVARIANT (no-daemon byte-identical): task_leases rows are
// written ONLY by daemon-mediated accepts. The default quest_accept path
// runs with a nil lease seam and creates zero lease rows, so its DB
// effects stay byte-identical to today. A claim accepted without the
// daemon has no lease and therefore can never be falsely forfeited as
// expired later. See accept_cmd.go for the best-effort wire-in.

const (
	// DefaultLeaseTTL is how long a freshly acquired or heartbeated
	// lease stays valid without a renewal. A daemon heartbeats well
	// inside this window; a crashed agent stops heartbeating and the
	// lease lapses after one TTL. Hardcoded for the acquire slice;
	// the heartbeat quest lifts it into config.
	DefaultLeaseTTL = 90 * time.Second

	// DefaultHeartbeatInterval is the cadence the daemon's heartbeat
	// loop renews held leases at. Kept well below DefaultLeaseTTL so a
	// single missed beat does not expire a live lease. Consumed by the
	// follow-on heartbeat quest; defined here so the TTL/interval pair
	// lives in one place.
	DefaultHeartbeatInterval = 30 * time.Second
)

// Lease is one row of task_leases: a held claim plus its liveness
// timestamps. Returned by ExpiredLeases for the reaper to act on.
type Lease struct {
	ProjectID   string
	TaskID      string
	SessionID   string
	Holder      string
	AcquiredAt  string
	HeartbeatAt string
	ExpiresAt   string
}

// LeaseAcquirer is the seam quest_accept calls after a claim commits.
// command.Deps carries it as `any` (the field is Lease) to keep the
// command package free of a quest dependency, mirroring the Embed port
// (command.go Deps.Embed). The default seam is nil: no daemon, no lease,
// byte-identical to today. Only the daemon wires a non-nil implementation,
// pre-bound to its per-session identity, so the accept handler passes
// only what it knows (project, task, holder) and never has to source a
// session id it does not own.
//
// AcquireLease is best-effort from quest_accept's perspective: a failure
// is logged and never converts a committed claim into an API error, the
// same invariant as the accept trail writer (accept.go writeAcceptTrail).
type LeaseAcquirer interface {
	AcquireLease(ctx context.Context, projectID, taskID, holder string) error
}

// DBLeaseAcquirer is the *sql.DB-backed LeaseAcquirer the daemon wires
// into command.Deps.Lease. SessionID is the daemon's per-session
// identity (from the multi-session serving loop); the daemon constructs
// one acquirer per session so the accept handler stays session-agnostic.
// Now is injectable for deterministic expiry math in tests (defaults to
// time.Now().UTC() when nil), mirroring the command.Deps.Now precedent.
//
// Terminal CLI accepts routed through the daemon are one-shot processes
// with no persistent session to heartbeat; v1 deliberately wires NO
// acquirer for them, preserving exactly today's terminal semantics with
// no false forfeits. This is a conscious v1 choice the daemon enforces
// by leaving Deps.Lease nil on that path.
type DBLeaseAcquirer struct {
	DB        *sql.DB
	SessionID string
	TTL       time.Duration
	Now       func() time.Time
}

// leaseFromDeps extracts the LeaseAcquirer the daemon stashed into
// command.Deps.Lease. command.Deps carries Lease as `any` to keep the
// command package free of a quest dependency; this helper centralizes
// the type assertion so the failure mode (nil on absence or mismatch) is
// uniform. A nil return is the documented no-daemon path: no lease row is
// ever written, so accept stays byte-identical to today.
func leaseFromDeps(d command.Deps) LeaseAcquirer {
	if d.Lease == nil {
		return nil
	}
	if a, ok := d.Lease.(LeaseAcquirer); ok {
		return a
	}
	return nil
}

// AcquireLease writes the lease row for a just-committed claim, binding
// it to the acquirer's SessionID. It is the LeaseAcquirer implementation
// the accept seam invokes.
func (a *DBLeaseAcquirer) AcquireLease(ctx context.Context, projectID, taskID, holder string) error {
	ttl := a.TTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowFn := a.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return AcquireLease(ctx, a.DB, projectID, taskID, a.SessionID, holder, nowFn().UTC(), ttl)
}

// AcquireLease records (or refreshes) the lease for taskID held by
// sessionID. acquired_at and heartbeat_at are set to now; expires_at to
// now+ttl. INSERT OR REPLACE so a daemon restart re-acquiring under a
// new session id refreshes the single (project_id, task_id) row rather
// than duplicating it.
//
// SQLITE_BUSY is retried up to three times, the same shape as the accept
// trail writer (accept.go writeAcceptTrail), because lease writes
// contend with the same WAL as concurrent accepts.
func AcquireLease(ctx context.Context, db *sql.DB, projectID, taskID, sessionID, holder string, now time.Time, ttl time.Duration) error {
	if db == nil {
		return fmt.Errorf("quest: acquire lease: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: acquire lease: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return fmt.Errorf("quest: acquire lease: empty task_id")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return fmt.Errorf("quest: acquire lease: empty session_id")
	}
	holder = agentOrDefault(holder)
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expStr := now.UTC().Add(ttl).Format(time.RFC3339Nano)

	return withBusyRetry(func() error {
		_, err := db.ExecContext(ctx,
			`INSERT OR REPLACE INTO task_leases
			   (project_id, task_id, session_id, holder, acquired_at, heartbeat_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			projectID, taskID, sessionID, holder, nowStr, nowStr, expStr,
		)
		if err != nil {
			return fmt.Errorf("quest: acquire lease: insert: %w", err)
		}
		return nil
	})
}

// RenewLeasesForSession pushes heartbeat_at to now and expires_at to
// now+ttl for every lease sessionID holds. The daemon's heartbeat loop
// calls this on its interval. Returns the number of leases renewed.
func RenewLeasesForSession(ctx context.Context, db *sql.DB, sessionID string, now time.Time, ttl time.Duration) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("quest: renew leases: nil db")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return 0, fmt.Errorf("quest: renew leases: empty session_id")
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	expStr := now.UTC().Add(ttl).Format(time.RFC3339Nano)

	var affected int64
	err := withBusyRetry(func() error {
		res, err := db.ExecContext(ctx,
			`UPDATE task_leases
			 SET heartbeat_at = ?, expires_at = ?
			 WHERE session_id = ?`,
			nowStr, expStr, sessionID,
		)
		if err != nil {
			return fmt.Errorf("quest: renew leases: update: %w", err)
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quest: renew leases: rows affected: %w", err)
		}
		return nil
	})
	return affected, err
}

// ReleaseLeasesForSession deletes every lease sessionID holds. The
// daemon calls this on a clean session shutdown so the session's claims
// drop their leases without waiting for expiry. Returns the number of
// leases released. Releasing a lease does NOT touch task_status: the
// claim's lifecycle (forfeit / fulfill) is independent of its lease.
func ReleaseLeasesForSession(ctx context.Context, db *sql.DB, sessionID string) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("quest: release leases: nil db")
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID == "" {
		return 0, fmt.Errorf("quest: release leases: empty session_id")
	}
	var affected int64
	err := withBusyRetry(func() error {
		res, err := db.ExecContext(ctx,
			`DELETE FROM task_leases WHERE session_id = ?`,
			sessionID,
		)
		if err != nil {
			return fmt.Errorf("quest: release leases: delete: %w", err)
		}
		affected, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("quest: release leases: rows affected: %w", err)
		}
		return nil
	})
	return affected, err
}

// ExpiredLeases returns every lease whose expires_at is at or before now,
// ordered by (project_id, task_id) for deterministic output. The reaper
// (a follow-on quest) walks these to forfeit the underlying stale claims.
func ExpiredLeases(ctx context.Context, db *sql.DB, now time.Time) ([]Lease, error) {
	if db == nil {
		return nil, fmt.Errorf("quest: expired leases: nil db")
	}
	nowStr := now.UTC().Format(time.RFC3339Nano)
	rows, err := db.QueryContext(ctx,
		`SELECT project_id, task_id, session_id, holder, acquired_at, heartbeat_at, expires_at
		 FROM task_leases
		 WHERE expires_at <= ?
		 ORDER BY project_id, task_id`,
		nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("quest: expired leases: query: %w", err)
	}
	defer rows.Close()

	var out []Lease
	for rows.Next() {
		var l Lease
		if err := rows.Scan(
			&l.ProjectID, &l.TaskID, &l.SessionID, &l.Holder,
			&l.AcquiredAt, &l.HeartbeatAt, &l.ExpiresAt,
		); err != nil {
			return nil, fmt.Errorf("quest: expired leases: scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("quest: expired leases: iterate: %w", err)
	}
	return out, nil
}

// DeleteLease removes the lease for one (project_id, task_id). The reaper
// calls this after it has forfeited the stale claim. Deleting a missing
// lease is a no-op, not an error.
func DeleteLease(ctx context.Context, db *sql.DB, projectID, taskID string) error {
	if db == nil {
		return fmt.Errorf("quest: delete lease: nil db")
	}
	if projectID = strings.TrimSpace(projectID); projectID == "" {
		return fmt.Errorf("quest: delete lease: empty project_id")
	}
	taskID = strings.ToUpper(strings.TrimSpace(taskID))
	if taskID == "" {
		return fmt.Errorf("quest: delete lease: empty task_id")
	}
	return withBusyRetry(func() error {
		_, err := db.ExecContext(ctx,
			`DELETE FROM task_leases WHERE project_id = ? AND task_id = ?`,
			projectID, taskID,
		)
		if err != nil {
			return fmt.Errorf("quest: delete lease: delete: %w", err)
		}
		return nil
	})
}

// withBusyRetry runs fn up to three times, retrying only on SQLITE_BUSY.
// Mirrors the accept trail writer's contention handling (accept.go
// writeAcceptTrail) so lease writes survive WAL contention with
// concurrent accepts without endangering any primary claim.
func withBusyRetry(fn func() error) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isBusyErr(err.Error()) {
			return err
		}
	}
	return lastErr
}
