package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/storage"
)

// TestBuildMCPLoreDeps_EmbedField confirms the MCP-side Deps builder
// threads currentEmbedDeps into command.Deps.Embed. The test drives
// two shapes: (1) embedder state "disabled" so wireEmbedDepsOnce
// should land nil; (2) manual injection simulating the wired path.
// Default-build machines can't probe the real BGE path without
// -tags=withembed bundled bytes, so the wired assertion uses a manual
// sentinel and trusts the wiring helper's own tests (see
// internal/lore/embed_wiring_test.go) for the meta-probe truth table.
func TestBuildMCPLoreDeps_EmbedField(t *testing.T) {
	// Route lore.db through a temp file so wireEmbedDepsOnce does not
	// hit the user's real ~/.guild/lore.db.
	tmpDir := t.TempDir()
	origLdb := ldbPath
	ldbPath = func() (string, error) {
		return filepath.Join(tmpDir, "lore.db"), nil
	}
	t.Cleanup(func() { ldbPath = origLdb })

	// Bring a fresh lore.db into existence with default meta seeds
	// (embedder_state='disabled').
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(tmpDir, "lore.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := storage.Migrate(ctx, db, "lore"); err != nil {
		t.Fatalf("storage.Migrate: %v", err)
	}
	_ = db.Close()

	t.Run("disabled_meta_yields_nil_embed", func(t *testing.T) {
		// Reset and call the production entry point.
		currentEmbedDeps = wireEmbedDepsOnce()
		if currentEmbedDeps != nil {
			t.Errorf("wireEmbedDepsOnce should return nil when meta.embedder_state='disabled'; got %+v", currentEmbedDeps)
		}

		deps := buildMCPLoreDeps()
		if deps.Embed != nil {
			t.Errorf("buildMCPLoreDeps().Embed = %+v, want nil", deps.Embed)
		}
	})

	t.Run("wired_embed_threads_into_deps", func(t *testing.T) {
		// Simulate a successful wire: manually inject a non-nil
		// *lore.EmbedDeps (empty, but non-nil pointer matters for the
		// threading assertion). A real enabled-path test requires
		// bundled assets, covered by the withembed integration run.
		currentEmbedDeps = &lore.EmbedDeps{ModelID: "sentinel-model-id"}
		t.Cleanup(func() { currentEmbedDeps = nil })

		deps := buildMCPLoreDeps()
		if deps.Embed == nil {
			t.Fatalf("buildMCPLoreDeps().Embed is nil; want the wired sentinel")
		}
		e, ok := deps.Embed.(*lore.EmbedDeps)
		if !ok {
			t.Fatalf("deps.Embed has type %T, want *lore.EmbedDeps", deps.Embed)
		}
		if e.ModelID != "sentinel-model-id" {
			t.Errorf("deps.Embed.ModelID = %q, want sentinel-model-id", e.ModelID)
		}
	})
}
