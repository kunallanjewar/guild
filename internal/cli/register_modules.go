package cli

// init() here binds the module-registry verbs onto questCmd / loreCmd. Its
// placement in the package init order is load-bearing and chosen by filename:
// Go runs a package's init functions in lexical source-file order, and
// "register_modules.go" sorts after quest.go and the lore_*.go files but
// before root.go. That window is exactly what the pre-cutover hand lists
// relied on:
//
//   - AFTER quest.go: questCmd's persistent --project / -p flag is already
//     set, so each bound quest verb inherits -p and command.BindCobra skips
//     re-declaring a colliding local one.
//   - AFTER lore_*.go: the bespoke (non-registry) lore subcommands
//     (appraise/study/archive/restore/init) are already attached.
//   - BEFORE root.go: questCmd and loreCmd are NOT yet attached to rootCmd,
//     so rootCmd's persistent flags (--agent Bool, --no-emoji, --verbose)
//     are not yet inherited into the verb flagsets. This matters: quest
//     journal/orders/campfire/summon declare a local string --agent
//     (agent identity). Binding them before the root Bool --agent is
//     inherited keeps the local flag registration clean, byte-identical to
//     when quest.go's init bound these verbs ahead of root.go.
//
// The single bindModuleVerbs loop replaces the four per-file hand lists of
// bindRegistryVerb / bindLoreRegistryVerb calls (ADR-006 Phase 2).
func init() {
	bindModuleVerbs()
}
