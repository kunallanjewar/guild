package mcp

import (
	"context"
	"log/slog"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// Register wires every guild tool onto s. Called exactly once from
// build() in server.go. bootstrap → always-on; no deferred tier.
func Register(s *sdkmcp.Server) {
	// Reset so hintsBridge builds one engine per server rebuild and all
	// Deps builders in this Register share it. See hints.go comment.
	currentHintsEngine = nil
	// The embedder port (ADR-003 Phase 1) is constructed once at
	// server boot and shared across every lore tool handler via
	// command.Deps.Embed. currentEmbedDeps holds the wired pointer
	// (or nil when meta.embedder_state != "enabled").
	currentEmbedDeps = wireEmbedDepsOnce()
	registerBootstrap(s)
	registerAlwaysOn(s)
}

// currentEmbedDeps is the per-server-rebuild EmbedDeps constructed by
// wireEmbedDepsOnce. Nil is the documented Phase-0 fallback: every
// lore tool handler pulls it via embedFromDeps and branches to
// BM25+stopwords when nil. Reset in Register() so each test-spawned
// server builds its own wiring.
var currentEmbedDeps *lore.EmbedDeps

// wireEmbedDepsOnce opens the lore DB, reads meta.embedder_state, and
// constructs *lore.EmbedDeps when state == "enabled". Emits exactly one
// structured slog line describing the outcome so operators can correlate
// a session's retrieval mode with the startup event. Runs with a
// bounded 15 s context so a pathological DB cannot wedge server boot.
//
// Nil-return is always safe: every downstream lore handler tolerates a
// nil Embed field. ADR-003 invariant: adapter startup never fails just
// because the embedder is off.
//
//nolint:contextcheck // wiring runs at server boot; the caller has no context yet. A bounded background context is the documented pattern (see buildWithContext which also opens lore.db without threading a user context).
func wireEmbedDepsOnce() *lore.EmbedDeps {
	logger := newLogger()

	// Open lore.db on a short-lived connection that lives only for the
	// duration of the wire call. The returned EmbedDeps does not
	// retain this handle; hot-path callers (Inscribe/Appraise) open
	// their own.
	bootCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	db, err := openLoreDB(bootCtx)
	if err != nil {
		logger.Info("embedder inactive",
			slog.String("reason", "lore_db_open_failed"),
			slog.String("err", err.Error()),
		)
		return nil
	}
	defer func() { _ = db.Close() }()

	deps, status, _ := lore.WireEmbedDeps(bootCtx, db, lore.EmbedWireOptions{
		Async:     true, // MCP surface: fire-and-forget Tx2.
		LoadIndex: true, // warm once, reuse across every appraise.
		Logger:    logger,
	})
	if !status.Wired {
		logger.Info("embedder inactive",
			slog.String("reason", status.Reason),
			slog.String("model_id", status.ModelID),
		)
		return nil
	}
	logger.Info("embedder wired",
		slog.String("model_id", status.ModelID),
		slog.Int("index_len", status.IndexLen),
		slog.Bool("async", true),
	)
	return deps
}

// buildMCPCommandDeps constructs the quest-side Deps bundle. The
// registry's OpenDB opens quest.db; ResolveProj uses the auto-bootstrap
// resolver so MCP reconnects are invisible (QUEST-65). RecordTelemetry
// wires the MCP usage.log emitter so every tool call produces a row.
// PrependNarration enables the auto-bootstrap narration path in the MCP
// handler wrapper — when auto-bootstrap fires, the narration line is
// prepended to the tool's output body. OpenLoreDB is wired so
// quest_post's spec= param (QUEST-63) can atomically inscribe a
// kind=decision lore entry alongside the quest.
func buildMCPCommandDeps() command.Deps {
	return command.Deps{
		OpenDB:           openQuestDB,
		ResolveProj:      resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  recordMCPTelemetry,
		PrependNarration: true,
		OpenLoreDB:       openLoreDB,
		EvaluateHints:    hintsBridge(),
	}
}

// buildMCPLoreDeps is the lore-side sibling. Identical ResolveProj with
// auto-bootstrap (QUEST-65), but OpenDB opens lore.db. RecordMiss wires
// the misses.log emitter for lore_appraise zero-result queries. Embed
// is the wired-once *lore.EmbedDeps (Phase 1, ADR-003); nil is the
// Phase-0 BM25+stopwords fallback and every lore handler tolerates it.
//
// Note: command.Deps.Embed is declared `any`, so a typed-nil
// *lore.EmbedDeps would become a non-nil interface value and defeat
// the nil-check in embedFromDeps. Only assign the field when the
// pointer is genuinely non-nil.
func buildMCPLoreDeps() command.Deps {
	d := command.Deps{
		OpenDB:           openLoreDB,
		ResolveProj:      resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  recordMCPTelemetry,
		RecordMiss:       recordMCPMiss,
		PrependNarration: true,
		EvaluateHints:    hintsBridge(),
	}
	if currentEmbedDeps != nil {
		d.Embed = currentEmbedDeps
	}
	return d
}

// registerBootstrap wires the tools that agents MUST be able to call
// before any active project exists: session start, mid-session project
// switch, and guild_status (re-orientation without re-bootstrapping).
func registerBootstrap(s *sdkmcp.Server) {
	registerSessionStart(s)
	registerSetProject(s)
	registerGuildStatus(s)
}

// registerAlwaysOn wires all guild tools. Full surface advertised at init.
func registerAlwaysOn(s *sdkmcp.Server) {
	// --- lore (read + write, common) ---
	loreDeps := buildMCPLoreDeps()
	lore.AppraiseCommand.BindMCP(s, loreDeps)
	lore.StudyCommand.BindMCP(s, loreDeps)
	lore.OathCommand.BindMCP(s, loreDeps)
	lore.ListCommand.BindMCP(s, loreDeps)
	lore.DossierCommand.BindMCP(s, loreDeps)
	lore.InscribeCommand.BindMCP(s, loreDeps)
	lore.ReforgeCommand.BindMCP(s, loreDeps)
	lore.UpdateCommand.BindMCP(s, loreDeps)
	lore.EchoesCommand.BindMCP(s, loreDeps)
	lore.WhispersCommand.BindMCP(s, loreDeps)
	lore.LinkCommand.BindMCP(s, loreDeps)    // provenance graph — write half
	lore.RipplesCommand.BindMCP(s, loreDeps) // provenance graph — read half
	// --- lore (hygiene) ---
	lore.InquestCommand.BindMCP(s, loreDeps)
	lore.MeldCommand.BindMCP(s, loreDeps)
	lore.CommuneCommand.BindMCP(s, loreDeps)
	lore.SealCommand.BindMCP(s, loreDeps)
	lore.CatalogCommand.BindMCP(s, loreDeps)
	// --- lore (embedder health, Phase 1.6 ADR-003) ---
	lore.EmbedderHealthCommand.BindMCP(s, loreDeps)
	lore.EmbedRebuildCommand.BindMCP(s, loreDeps)
	// --- quest (common flow) ---
	mcpDeps := buildMCPCommandDeps()
	quest.PostCommand.BindMCP(s, mcpDeps)
	quest.UpdateCommand.BindMCP(s, mcpDeps)
	quest.AcceptCommand.BindMCP(s, mcpDeps)
	quest.JournalCommand.BindMCP(s, mcpDeps)
	quest.CampfireCommand.BindMCP(s, mcpDeps)
	quest.FulfillCommand.BindMCP(s, mcpDeps)
	// quest_clear is kept as a backward-compat MCP alias (same handler,
	// different tool name) so agents trained on the pre-QUEST-106 verb
	// still work. Tool discovery surfaces both; new agents prefer fulfill.
	quest.ClearCommand.BindMCP(s, mcpDeps)
	quest.BriefCommand.BindMCP(s, mcpDeps)
	quest.SummonCommand.BindMCP(s, mcpDeps)
	quest.OrdersCommand.BindMCP(s, mcpDeps)
	registerQuestBounties(s)
	quest.ListCommand.BindMCP(s, mcpDeps)
	quest.ScrollCommand.BindMCP(s, mcpDeps)
	quest.PulseCommand.BindMCP(s, mcpDeps)
	quest.GuildCommand.BindMCP(s, mcpDeps)
	quest.EpicCommand.BindMCP(s, mcpDeps)
	quest.ActiveCommand.BindMCP(s, mcpDeps)
	quest.ForfeitCommand.BindMCP(s, mcpDeps)
	// archive/restore is CLI-only (QUEST-45) — see tools_guild.go comment.
}
