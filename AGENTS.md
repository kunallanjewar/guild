# guild — agent bootstrap

This is the source repo for the `guild` binary. The MCP server's
`INSTRUCTIONS` string (loaded when an MCP client connects to guild)
carries the full operating contract — tool discipline, kind taxonomy,
autonomy rules, narration. This file only adds repo-specific context
for contributors.

## First action — bootstrap guild MCP

```
mcp__guild__guild_session_start(project="guild")
```

Surfaces the last briefing + oath + top bounty for work on this repo.
Every subsequent guild MCP tool inherits `project="guild"` automatically.

## Repo facts

- Module: `github.com/mathomhaus/guild`
- Language: Go 1.25+
- Main libs: `spf13/cobra`, `log/slog`, `modernc.org/sqlite` (pure-Go),
  `github.com/modelcontextprotocol/go-sdk` (version: see go.mod)
- Entry points: `cmd/guild/main.go`, `cmd/sqlcheck/main.go`
- Test gate: `make check` (fmt + vet + lint + sqlcheck + test-race)
- CI reproducer: `make ci`
- Release: goreleaser via `make release-snapshot` locally or v*.*.* tag push

## Tool discipline

- Use guild MCP tools (`mcp__guild__*`) for all guild operations
- Bash CLI (`guild lore ...`, `guild quest ...`) only as last resort if
  MCP is unrecoverable
- Never bypass the tool to write raw SQL against `~/.guild/*.db`

## When onboarding

Contributors new to the repo should also skim:

- `README.md` — install + setup from an end-user perspective
- `Makefile` — the canonical developer commands (`make help` for the list)
- `internal/mcp/instructions.go` — the constant string every MCP client reads on connect

## guild workflow

guild coordinates tasks (quest) and persistent knowledge (lore) across sessions and agents.

**BEFORE ANY OTHER ACTION** — before reading files, editing code, or
responding to the user — call the MCP tool `guild_session_start(project="guild")`.
It returns the full agent contract, active principles (oath), and the
current top bounty. Follow what it returns.

If `guild_session_start` is not visible in your tool list, run your
host's tool-search for `guild` first — some hosts lazy-load MCP tools.
Do NOT fall back to CLI; the MCP server is available.

### Core rules (full contract is returned by session_start)

- **Never use built-in task tools** (TaskCreate / TaskUpdate / TaskList) —
  they're session-scoped. Use `quest_post` / `quest_accept` / `quest_list` instead.
- **Accept before working on a quest** — `quest_accept(quest_id=...)` prevents
  parallel-agent collisions.
- **Appraise before researching** — `lore_appraise(query=..., all_projects=true)`
  first. If current entries exist, use them.
- **Brief before session end** — when wrapping up or compaction is near,
  call `quest_brief("what was done, what's next, gotchas")` without being asked.

MCP namespace: `mcp__guild__*`. CLI fallback: `guild --help` (last resort only).
