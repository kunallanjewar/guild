//go:build unix

package mcp

import (
	"context"
	"net"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/quest"
)

// leaseRowCountForSession returns how many task_leases rows the given
// session id holds in the hermetic quest.db.
func leaseRowCountForSession(t *testing.T, sessionID string) int {
	t.Helper()
	ctx := context.Background()
	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_leases WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count leases for session %s: %v", sessionID, err)
	}
	return n
}

// seedProjectAndQuest registers a project and posts one open quest in the
// hermetic quest.db, returning the quest id.
func seedProjectAndQuest(t *testing.T, projID, projDir string) string {
	t.Helper()
	ctx := context.Background()
	registerCWDAsProject(t, projID, projDir)
	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	defer func() { _ = db.Close() }()
	q, err := quest.Post(ctx, db, projID, quest.PostParams{Subject: "lease target"})
	if err != nil {
		t.Fatalf("quest.Post: %v", err)
	}
	return q.ID
}

// runOneShimSession runs ServeSession over an in-memory pipe, runs fn
// against the connected client, then closes cleanly and joins the handler.
func runOneShimSession(t *testing.T, host *DaemonHost, shimPID int, cwd string, fn func(cs *sdkmcp.ClientSession)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverEnd, clientEnd := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- host.ServeSession(ctx,
			daemon.ShimPreamble{Version: "t", CWD: cwd, PID: shimPID}, serverEnd)
	}()

	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "guild-lease-test-client"}, nil)
	cs, err := client.Connect(ctx, &sdkmcp.IOTransport{Reader: clientEnd, Writer: clientEnd}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	fn(cs)

	_ = cs.Close()
	_ = clientEnd.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeSession returned %v; want nil on peer close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeSession did not return after client close")
	}
}

// TestDaemonSession_AcceptWritesSessionLease verifies the end-to-end Phase
// 3 wiring: a daemon-mediated quest_accept writes a task_leases row keyed
// by the session's identity (the shim pid), so the heartbeat tick later
// refreshes exactly the rows the session's accept wrote.
func TestDaemonSession_AcceptWritesSessionLease(t *testing.T) {
	home := isolateHome(t)
	const projID = "leaseproj"
	projDir := home + "/ws/" + projID
	questID := seedProjectAndQuest(t, projID, projDir)

	host := NewDaemonHost()
	t.Cleanup(host.providers.closeHintsEngine)

	const shimPID = 880011
	runOneShimSession(t, host, shimPID, projDir, func(cs *sdkmcp.ClientSession) {
		ctx := context.Background()
		res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
			Name:      "quest_accept",
			Arguments: map[string]any{"quest_id": questID, "project": projID},
		})
		if err != nil {
			t.Fatalf("quest_accept: %v", err)
		}
		if res.IsError {
			t.Fatalf("quest_accept IsError: %s", textOf(res.Content))
		}
	})

	// The lease row is keyed by the session id derived from the shim pid.
	wantSession := daemonSessionID(shimPID)
	if got := leaseRowCountForSession(t, wantSession); got != 1 {
		t.Fatalf("lease rows for session %s = %d, want 1", wantSession, got)
	}
}

// TestDaemonSession_RegistryTracksSession verifies a live ServeSession
// registers in the host's registry and unregisters on detach, so presence
// and the heartbeat tick see an accurate live-connection set.
func TestDaemonSession_RegistryTracksSession(t *testing.T) {
	home := isolateHome(t)
	const projID = "trackproj"
	projDir := home + "/ws/" + projID
	registerCWDAsProject(t, projID, projDir)

	host := NewDaemonHost()
	t.Cleanup(host.providers.closeHintsEngine)

	const shimPID = 880022
	observed := -1
	runOneShimSession(t, host, shimPID, projDir, func(cs *sdkmcp.ClientSession) {
		// Force the session to resolve its project, then read the registry.
		ctx := context.Background()
		if _, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{
			Name: "guild_session_start", Arguments: map[string]any{},
		}); err != nil {
			t.Fatalf("guild_session_start: %v", err)
		}
		observed = host.Registry().Count()
		snap := host.Registry().Snapshot()
		if len(snap) != 1 || snap[0].ID != daemonSessionID(shimPID) {
			t.Fatalf("registry snapshot = %+v, want one session %s", snap, daemonSessionID(shimPID))
		}
	})

	if observed != 1 {
		t.Fatalf("registry count during session = %d, want 1", observed)
	}
	// After detach the session is gone.
	if got := host.Registry().Count(); got != 0 {
		t.Fatalf("registry count after detach = %d, want 0", got)
	}
}

// TestDaemonBootGraceRenewsExistingLeases verifies the host's boot-grace
// seam renews every pre-existing lease (so a lease left by a crashed daemon
// gets one grace window before any reaper could observe it as expired).
func TestDaemonBootGraceRenewsExistingLeases(t *testing.T) {
	home := isolateHome(t)
	const projID = "bootproj"
	projDir := home + "/ws/" + projID
	questID := seedProjectAndQuest(t, projID, projDir)

	ctx := context.Background()

	// Plant a lease that already expired (heartbeat stopped well before now).
	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	stale := time.Now().UTC().Add(-time.Hour)
	if err := quest.AcquireLease(ctx, db, projID, questID, "old-session", "alice", stale, time.Minute); err != nil {
		_ = db.Close()
		t.Fatalf("plant stale lease: %v", err)
	}
	_ = db.Close()

	// Build the host with a known TTL and run its boot-grace seam directly.
	const ttl = 10 * time.Minute
	host := NewDaemonHostWithLeases(ttl, 30*time.Second)
	t.Cleanup(host.providers.closeHintsEngine)
	if err := host.bootGraceSeam()(ctx); err != nil {
		t.Fatalf("boot grace: %v", err)
	}

	// The previously-expired lease now sits a full TTL in the future.
	checkDB, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("reopen quest db: %v", err)
	}
	defer func() { _ = checkDB.Close() }()
	var expiresAt string
	if err := checkDB.QueryRowContext(ctx,
		`SELECT expires_at FROM task_leases WHERE project_id = ? AND task_id = ?`,
		projID, questID).Scan(&expiresAt); err != nil {
		t.Fatalf("read renewed lease: %v", err)
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", expiresAt, err)
	}
	if !exp.After(time.Now().UTC()) {
		t.Fatalf("boot grace did not extend expiry: expires_at = %v is not in the future", exp)
	}
}

// TestDaemonSession_RenewSeamRefreshesHeldLease verifies the heartbeat
// renewal seam (the one the registry tick calls) pushes a held lease's
// expiry forward.
func TestDaemonSession_RenewSeamRefreshesHeldLease(t *testing.T) {
	home := isolateHome(t)
	const projID = "renewproj"
	projDir := home + "/ws/" + projID
	questID := seedProjectAndQuest(t, projID, projDir)

	ctx := context.Background()
	sessionID := daemonSessionID(880033)

	db, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("open quest db: %v", err)
	}
	soon := time.Now().UTC().Add(time.Second)
	if err := quest.AcquireLease(ctx, db, projID, questID, sessionID, "alice", time.Now().UTC().Add(-time.Minute), 61*time.Second); err != nil {
		_ = db.Close()
		t.Fatalf("acquire near-expiry lease: %v", err)
	}
	_ = db.Close()

	host := NewDaemonHostWithLeases(10*time.Minute, 30*time.Second)
	t.Cleanup(host.providers.closeHintsEngine)
	if err := host.renewLeasesSeam()(ctx, sessionID); err != nil {
		t.Fatalf("renew seam: %v", err)
	}

	checkDB, err := openQuestDB(ctx)
	if err != nil {
		t.Fatalf("reopen quest db: %v", err)
	}
	defer func() { _ = checkDB.Close() }()
	var expiresAt string
	if err := checkDB.QueryRowContext(ctx,
		`SELECT expires_at FROM task_leases WHERE session_id = ?`, sessionID).Scan(&expiresAt); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	exp, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !exp.After(soon.Add(time.Minute)) {
		t.Fatalf("renew seam did not extend the lease far enough: expires_at = %v", exp)
	}
}
