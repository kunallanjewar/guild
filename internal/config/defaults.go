// Package config implements guild's layered configuration loader.
//
// Precedence (low → high):
//  1. Built-in defaults (this file)
//  2. ~/.guild/config.toml      (user-wide)
//  3. <repo>/.guild/config.toml (per-project overrides)
//  4. Environment variables      (GUILD_PROJECT, GUILD_NO_USAGE_LOG, GUILD_NO_EMOJI)
//  5. CLI flags                  (via pflag.FlagSet)
package config

// ScoringConfig holds BM25 + recency + title-boost weights used by lore appraise.
type ScoringConfig struct {
	// WFTS is the BM25 full-text-search weight (0.0–1.0).
	WFTS float64 `toml:"w_fts"`
	// WRecency is the recency decay weight (0.0–1.0).
	WRecency float64 `toml:"w_recency"`
	// HalfLifeDays controls recency decay: score halves every N days.
	HalfLifeDays float64 `toml:"half_life_days"`
	// TitleMatchBoost is added to the score when a query exactly matches an entry title.
	TitleMatchBoost float64 `toml:"title_match_boost"`
	// TitleTokenBoost is added to the score when all query tokens appear in the title.
	TitleTokenBoost float64 `toml:"title_token_boost"`
}

// InscribeConfig holds validation thresholds for lore inscribe.
// Keeps principle entries short to preserve oath-wall hygiene.
type InscribeConfig struct {
	// PrincipleMaxWords is the word-count threshold for principle entries;
	// inscribe warns when exceeded (target: principles ≤60 words).
	PrincipleMaxWords int `toml:"principle_max_words"`
	// BloatSevereThreshold is the word count above which lore commune --fix
	// auto-trims bloated principle entries.
	BloatSevereThreshold int `toml:"bloat_severe_threshold"`
	// ValidDays maps an entry kind to the valid_days window stamped onto
	// new entries when the caller does not pass valid_days explicitly.
	// 0 means "never stale" (stored as NULL). Configured per kind:
	//
	//	[inscribe.valid_days]
	//	research = 30
	//	decision = 180
	//
	// Keys must be one of the five canonical kinds (see loreKinds);
	// unknown keys and negative values fail config load. Keys absent
	// from a file keep the value from the layer below (per-key merge,
	// same as every other config knob).
	ValidDays map[string]int `toml:"valid_days"`
}

// loreKinds is the set of canonical entry kinds accepted as keys under
// [inscribe.valid_days]. It mirrors lore.AllKinds(); the list is kept
// local because internal/config sits at the bottom of the dependency
// graph and must not import domain packages (internal/lore deliberately
// does not import internal/config, and the reverse edge would pin the
// two packages together forever). TestValidDaysKindsMatchLore asserts
// this list stays in sync with lore.AllKinds().
var loreKinds = map[string]bool{
	"idea":        true,
	"research":    true,
	"decision":    true,
	"observation": true,
	"principle":   true,
}

// TelemetryConfig controls per-call usage logging to ~/.guild/usage.log.
type TelemetryConfig struct {
	// UsageLog enables writing TSV records to ~/.guild/usage.log.
	// Off by default; set true in [telemetry] config to opt in.
	UsageLog bool `toml:"usage_log"`
}

// Config is the merged, validated configuration for a guild process.
// All fields are safe to read after Load returns; nil pointer dereferences
// cannot happen because Load always fills in defaults before returning.
type Config struct {
	// Project is the active project name.  Resolved from: --project flag →
	// GUILD_PROJECT env → per-project config → session state file.
	// Empty string means "not yet resolved" (MCP server sets it later via
	// guild_session_start).
	Project string `toml:"project"`

	// NoUsageLog is the merged runtime disable bit for usage logging.
	// True whenever Telemetry.UsageLog is false (the default) or GUILD_NO_USAGE_LOG=1 is set.
	NoUsageLog bool `toml:"-"`

	// NoEmoji substitutes ASCII equivalents for emoji prefixes in all output.
	// Driven by GUILD_NO_EMOJI=1 env or --no-emoji flag.
	NoEmoji bool `toml:"-"`

	Scoring   ScoringConfig   `toml:"scoring"`
	Inscribe  InscribeConfig  `toml:"inscribe"`
	Telemetry TelemetryConfig `toml:"telemetry"`
}

// defaults returns a Config populated with the built-in baseline values.
// All other layers in Load apply deltas on top of this.
func defaults() Config {
	return Config{
		Scoring: ScoringConfig{
			WFTS:            0.7,
			WRecency:        0.3,
			HalfLifeDays:    30,
			TitleMatchBoost: 1.0,
			TitleTokenBoost: 0.5,
		},
		Inscribe: InscribeConfig{
			PrincipleMaxWords:    60,
			BloatSevereThreshold: 120,
			// Built-in decay windows: research and decision entries fade;
			// idea/observation/principle never auto-stale (0 = never stale).
			// Must match the kindValidDays fallback in internal/lore so
			// zero-config behavior is byte-identical with or without this
			// map threaded through.
			ValidDays: map[string]int{
				"idea":        0,
				"research":    30,
				"decision":    180,
				"observation": 0,
				"principle":   0,
			},
		},
		Telemetry: TelemetryConfig{
			UsageLog: false,
		},
	}
}
