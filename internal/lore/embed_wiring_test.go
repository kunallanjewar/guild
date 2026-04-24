package lore

import (
	"context"
	"runtime"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
)

// stubCommandDeps builds a command.Deps carrying the given *EmbedDeps
// in its Embed field. Lets the test round-trip the wiring through the
// same path production handlers see (embedFromDeps(d command.Deps)).
func stubCommandDeps(e *EmbedDeps) command.Deps {
	if e == nil {
		return command.Deps{}
	}
	return command.Deps{Embed: e}
}

// TestWireEmbedDeps_FallbackPaths covers every documented non-wired
// outcome of WireEmbedDeps table-driven. Each case mutates meta to a
// pre-state then asserts WireEmbedDeps returns (nil, status with the
// matching Reason tag). This is the adapter-layer contract the MCP +
// CLI callers depend on: on any non-enabled state, the caller threads
// nil into command.Deps.Embed and the Phase-0 BM25+stopwords path runs
// deterministically.
//
// The "enabled" probe path is covered by the embed package integration
// tests (probe_test / backfill_test). Wiring this test to the real BGE
// runtime would need bundled asset bytes that only land under
// -tags=withembed; see the separate TestWireEmbedDeps_EnabledIntegration
// in that build variant for end-to-end validation.
func TestWireEmbedDeps_FallbackPaths(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		setupMeta   func(t *testing.T, db interface{}) // no-op for most cases; the raw SQL lives inline to keep table lean
		wantReason  string
		wantWired   bool
		seedStateTo string // "" leaves the schema-seeded default; otherwise overrides the embedder_state row
	}{
		{
			name:       "fresh_db_meta_not_enabled",
			wantReason: "meta_not_enabled",
			wantWired:  false,
			// schema seed: embedder_state='disabled' (see migration 003).
		},
		{
			name:        "explicit_disabled",
			seedStateTo: "disabled",
			wantReason:  "meta_not_enabled",
			wantWired:   false,
		},
		{
			name:        "enabled_without_bundled_assets",
			seedStateTo: "enabled",
			// On default builds HasAssets()=false so the reason depends
			// on platform. On windows PrepareAndProbe short-circuits to
			// "platform_disabled"; elsewhere the wiring checks
			// HasAssets() before probing and returns "no_bundled_assets".
			wantWired: false,
			wantReason: func() string {
				if runtime.GOOS == "windows" {
					return "no_bundled_assets" // HasAssets() gates first on non-embed builds even on windows
				}
				return "no_bundled_assets"
			}(),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Minimal project id so storage.Migrate runs cleanly.
			//nolint:contextcheck // openTestDB wraps context.Background internally; other lore tests follow the same pattern.
			db := openTestDB(t, "wire-test")

			if tc.seedStateTo != "" {
				_, err := db.ExecContext(ctx,
					`INSERT INTO meta (key, value) VALUES ('embedder_state', ?)
					 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
					tc.seedStateTo,
				)
				if err != nil {
					t.Fatalf("seed embedder_state=%q: %v", tc.seedStateTo, err)
				}
			}

			deps, status, err := WireEmbedDeps(ctx, db, EmbedWireOptions{
				Async:     true,
				LoadIndex: true,
			})
			if err != nil {
				t.Fatalf("WireEmbedDeps returned err: %v", err)
			}
			if status.Wired != tc.wantWired {
				t.Errorf("status.Wired = %v, want %v", status.Wired, tc.wantWired)
			}
			if status.Reason != tc.wantReason {
				t.Errorf("status.Reason = %q, want %q", status.Reason, tc.wantReason)
			}
			if tc.wantWired {
				if deps == nil {
					t.Errorf("expected non-nil *EmbedDeps when Wired=true")
				}
			} else {
				if deps != nil {
					t.Errorf("expected nil *EmbedDeps when Wired=false; got %+v", deps)
				}
			}

			// Nil-safety: embedFromDeps must tolerate both paths without
			// a type assertion panic. Hand it through the commandDeps-
			// style opaque field to exercise the full round trip.
			roundTrip := embedFromDeps(stubCommandDeps(deps))
			if tc.wantWired && roundTrip == nil {
				t.Errorf("embedFromDeps round-trip dropped a non-nil EmbedDeps")
			}
			if !tc.wantWired && roundTrip != nil {
				t.Errorf("embedFromDeps round-trip fabricated an EmbedDeps")
			}
		})
	}
}

// TestWireEmbedDeps_NilDB covers the defensive guard: nil db returns a
// structured "nil_db" reason rather than panicking. Matches the MCP
// startup contract where an open-DB failure upstream must not crash
// server boot.
func TestWireEmbedDeps_NilDB(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deps, status, err := WireEmbedDeps(ctx, nil, EmbedWireOptions{})
	if err != nil {
		t.Fatalf("expected nil error on nil db, got %v", err)
	}
	if deps != nil {
		t.Errorf("expected nil *EmbedDeps for nil db")
	}
	if status.Wired {
		t.Errorf("status.Wired should be false for nil db")
	}
	if status.Reason != "nil_db" {
		t.Errorf("status.Reason = %q, want %q", status.Reason, "nil_db")
	}
}
