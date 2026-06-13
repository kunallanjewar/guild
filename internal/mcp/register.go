package mcp

import (
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/quest"
)

// loreValidDaysFromConfig loads the merged config and returns the
// per-kind valid_days windows ([inscribe.valid_days]) for lore writes.
// Returns nil on load failure so the built-in kind defaults apply: a
// broken config file must never block an inscribe (matching the
// swallow-and-degrade posture of recordMCPTelemetry). Wired as
// command.Deps.LoreValidDays and therefore called lazily per tool
// invocation, so the long-lived server observes config edits without a
// restart.
func loreValidDaysFromConfig() map[string]int {
	cfg, err := config.Load(nil)
	if err != nil {
		return nil
	}
	return cfg.Inscribe.ValidDays
}

// Register wires every guild tool onto s against a default per-process
// core: per-PID session identity plus a fresh provider bundle, exactly
// what the stdio server runs with. Hosts that need injectable seams
// (explicit session identity, shared provider bundle) construct through
// NewServer instead, which routes the same registration with their
// options. bootstrap → always-on; no deferred tier.
func Register(s *sdkmcp.Server) {
	// Reset the once-per-process sleep-autopass gate so each default
	// server construction starts with a fresh gate, mirroring how the
	// auto-backfill gate is reset for test-spawned servers. Production
	// builds one server per process, so this runs once; tests that build
	// several servers via Register each get a clean gate.
	resetSleepAutopassState()
	registerAll(s, &serverCore{
		sessions:  processSessionStore{},
		providers: newProcessProviders(),
	})
}

// registerAll wires every guild tool onto s against core. Called once
// per constructed server (NewServer or Register); every handler closure
// and Deps bundle built here references this core and no package-level
// state, so two servers in one process cannot clobber each other.
func registerAll(s *sdkmcp.Server, core *serverCore) {
	core.registerBootstrap(s)
	core.registerAlwaysOn(s)
}

// buildMCPCommandDeps constructs the quest-side Deps bundle. The
// registry's OpenDB opens quest.db; ResolveProj uses the auto-bootstrap
// resolver so MCP reconnects are invisible (QUEST-65). RecordTelemetry
// wires the MCP usage.log emitter so every tool call produces a row.
// PrependNarration enables the auto-bootstrap narration path in the MCP
// handler wrapper: when auto-bootstrap fires, the narration line is
// prepended to the tool's output body. OpenLoreDB is wired so
// quest_post's spec= param (QUEST-63) can atomically inscribe a
// kind=decision lore entry alongside the quest.
// Embed carries the questEmbedProvider so quest_search can reach the
// RRF arm when meta.quest.embedder_state="enabled" (QUEST-258).
func (c *serverCore) buildMCPCommandDeps() command.Deps {
	d := command.Deps{
		OpenDB:           openQuestDB,
		ResolveProj:      c.resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  c.recordMCPTelemetry,
		PrependNarration: true,
		OpenLoreDB:       openLoreDB,
		EvaluateHints:    c.providers.hintsBridge(),
		LoreValidDays:    loreValidDaysFromConfig,
	}
	if c.providers.questEmbed != nil {
		d.Embed = c.providers.questEmbed
	}
	// Lease is the daemon's per-session quest-lease port (ADR-005 Phase
	// 3): an accept under this session writes a lease, a mutating call
	// renews it. Nil for the stdio and in-process paths, the
	// byte-identical no-daemon contract. The field is `any` and only set
	// when genuinely non-nil so a typed nil never defeats the
	// leaseFromDeps nil-check (same care as Embed above).
	if c.lease != nil {
		d.Lease = c.lease
	}
	return d
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
func (c *serverCore) buildMCPLoreDeps() command.Deps {
	d := command.Deps{
		OpenDB:           openLoreDB,
		ResolveProj:      c.resolveProjectAutoBootstrap,
		Now:              time.Now,
		RecordTelemetry:  c.recordMCPTelemetry,
		RecordMiss:       recordMCPMiss,
		PrependNarration: true,
		EvaluateHints:    c.providers.hintsBridge(),
		LoreValidDays:    loreValidDaysFromConfig,
	}
	if c.providers.embed != nil {
		d.Embed = c.providers.embed
	}
	return d
}

// registerBootstrap wires the tools that agents MUST be able to call
// before any active project exists: session start, mid-session project
// switch, and guild_status (re-orientation without re-bootstrapping).
func (c *serverCore) registerBootstrap(s *sdkmcp.Server) {
	c.registerSessionStart(s)
	c.registerSetProject(s)
	c.registerGuildStatus(s)
}

// registerAlwaysOn wires all guild tools. Full surface advertised at init.
func (c *serverCore) registerAlwaysOn(s *sdkmcp.Server) {
	// --- lore (read + write, common) ---
	loreDeps := c.buildMCPLoreDeps()
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
	lore.LinkCommand.BindMCP(s, loreDeps)    // provenance graph: write half
	lore.UnlinkCommand.BindMCP(s, loreDeps)  // provenance graph: remove edge
	lore.RipplesCommand.BindMCP(s, loreDeps) // provenance graph: read half
	// --- lore (hygiene) ---
	lore.InquestCommand.BindMCP(s, loreDeps)
	lore.MeldCommand.BindMCP(s, loreDeps)
	lore.CommuneCommand.BindMCP(s, loreDeps)
	lore.SealCommand.BindMCP(s, loreDeps)
	lore.CatalogCommand.BindMCP(s, loreDeps)
	// --- lore (embedder health, Phase 1.6 ADR-003) ---
	lore.EmbedderHealthCommand.BindMCP(s, loreDeps)
	lore.EmbedRebuildCommand.BindMCP(s, loreDeps)
	lore.CoverageReconcileCommand.BindMCP(s, loreDeps)
	// --- quest (common flow) ---
	mcpDeps := c.buildMCPCommandDeps()
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
	c.registerQuestBounties(s)
	quest.ListCommand.BindMCP(s, mcpDeps)
	quest.ScrollCommand.BindMCP(s, mcpDeps)
	quest.PulseCommand.BindMCP(s, mcpDeps)
	quest.GuildCommand.BindMCP(s, mcpDeps)
	quest.EpicCommand.BindMCP(s, mcpDeps)
	quest.ActiveCommand.BindMCP(s, mcpDeps)
	quest.ForfeitCommand.BindMCP(s, mcpDeps)
	quest.SearchCommand.BindMCP(s, mcpDeps)
	// archive/restore is CLI-only (QUEST-45) — see tools_guild.go comment.
}
