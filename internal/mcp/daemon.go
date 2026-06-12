package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/session"
)

// DaemonHost is the per-process state behind `guild daemon run`: the
// one provider bundle every connection's MCP server shares, so the
// embedder, hints engine, and auto-backfill trigger are constructed
// once per daemon process instead of once per attached harness.
//
// Each accepted connection still gets its OWN *sdkmcp.Server (tool
// registration is cheap and in-memory) with its own session identity
// (the shim's pid) and its own INSTRUCTIONS (resolved against the
// shim's cwd). Only the provider bundle is shared: exactly the
// multi-session contract Options.Providers documents.
type DaemonHost struct {
	providers *Providers
	logger    *slog.Logger
}

// Compile-time check: ServeSession satisfies the daemon package's
// session-handler seam without an adapter.
var _ daemon.SessionHandler = (*DaemonHost)(nil).ServeSession

// NewDaemonHost constructs the shared bundle for one daemon process.
// Call once per `guild daemon run`; pass ServeSession and
// EmbedderState into daemon.Config.
func NewDaemonHost() *DaemonHost {
	return &DaemonHost{providers: NewProviders(), logger: newLogger()}
}

// Logger exposes the env-configured stderr logger (GUILD_MCP_LOG_FORMAT
// / GUILD_MCP_LOG_LEVEL) so cmd/guild can hand the daemon listener the
// same logger the per-session servers use.
func (h *DaemonHost) Logger() *slog.Logger { return h.logger }

// ServeSession serves one complete MCP session over conn (the raw
// newline-delimited JSON-RPC stream that follows the shim preamble)
// and blocks until the peer disconnects or ctx is cancelled. It is the
// production daemon.SessionHandler.
//
// Identity impersonation per the daemon contract: the session file is
// keyed by the SHIM's pid (so a reconnecting shim resumes its own
// active project, and concurrent shims never clobber each other), and
// INSTRUCTIONS plus project auto-inference anchor on the SHIM's cwd.
func (h *DaemonHost) ServeSession(ctx context.Context, shim daemon.ShimPreamble, conn io.ReadWriteCloser) error {
	store, err := daemonSessionStore(shim.PID)
	if err != nil {
		return err
	}

	instructions := resolveInstructionsFor(ctx, h.logger, store, shim.CWD)

	srv, err := NewServer(Options{
		Instructions: instructions,
		Sessions:     store,
		Providers:    h.providers,
		CWD:          shim.CWD,
	})
	if err != nil {
		return fmt.Errorf("mcp: daemon session (shim pid %d): %w", shim.PID, err)
	}

	// IOTransport speaks the same ndjson framing as StdioTransport, so
	// the byte stream after the preamble is exactly what the stdio
	// server would have read on stdin, with no re-framing layer.
	if err := srv.Run(ctx, &sdkmcp.IOTransport{Reader: conn, Writer: conn}); err != nil {
		// Cancelled ctx is daemon shutdown; EOF is the peer hanging up.
		// Both are clean ends of a session, not failures.
		if ctx.Err() != nil || errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("mcp: daemon session (shim pid %d): %w", shim.PID, err)
	}
	return nil
}

// EmbedderState reports meta.embedder_state from lore.db for the
// daemon's status line ("enabled", "disabled", ...), or "unknown" when
// the value cannot be read. It deliberately reads the durable meta row
// rather than the provider cache: the status line should reflect what
// the next lore call will resolve, not what the last one did.
func (h *DaemonHost) EmbedderState(ctx context.Context) string {
	db, err := openLoreDB(ctx)
	if err != nil {
		return "unknown"
	}
	defer func() { _ = db.Close() }()

	state, err := readMetaValue(ctx, db, "embedder_state")
	if err != nil || state == "" {
		return "unknown"
	}
	return state
}

// daemonSessionStore builds the per-connection session identity: the
// standard ~/.guild/sessions layout keyed by the SHIM's pid, mirroring
// what the shim's own in-process MCP server would have used. $HOME is
// resolved per connection (not cached) for the same reason
// session.defaultManager resolves it lazily.
func daemonSessionStore(pid int) (session.Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return session.Manager{}, fmt.Errorf("mcp: daemon session store: resolve home: %w", err)
	}
	return session.Manager{BaseDir: filepath.Join(home, ".guild"), PID: pid}, nil
}
