package mcp

import (
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
	// The embedder port (ADR-003 Phase 1, QUEST-219 lazy-reconstruct)
	// is wired as a provider, not a static *EmbedDeps. The provider
	// re-reads meta.embedder_state on every lore tool entry and
	// reconstructs when the state flips mid-session (the guild-init
	// trap captured in LORE-371). Each test-spawned server gets its
	// own provider.
	currentEmbedProvider = newEmbedProvider(openLoreDB, newLogger())
	registerBootstrap(s)
	registerAlwaysOn(s)
}

// currentEmbedProvider is the per-server-rebuild embed lazy-resolver.
// Every lore tool handler pulls *lore.EmbedDeps from it via
// embedFromDeps; a nil *EmbedDeps return (meta.embedder_state !=
// "enabled") branches to BM25+stopwords exactly like Phase 0.
// Reset in Register() so each test-spawned server builds its own
// provider with its own cache + mutex. QUEST-219.
var currentEmbedProvider *embedProvider

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
// is the lazy-reconstruct provider (QUEST-219, ADR-003 Phase 1); the
// lore side calls .ResolveEmbedDeps(ctx) on every handler entry so a
// mid-session meta flip is observed without restarting the MCP
// server. A nil pointer returned from the provider is the Phase-0
// BM25+stopwords fallback and every lore handler tolerates it.
//
// Note: command.Deps.Embed is declared `any`, so a typed-nil
// *embedProvider would become a non-nil interface value and defeat
// the nil-check in embedFromDeps. Only assign the field when the
// provider is genuinely non-nil.
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
	if currentEmbedProvider != nil {
		d.Embed = currentEmbedProvider
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
