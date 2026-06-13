package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"
)

// userConfigDir returns the path to ~/.guild/config.toml (layer 2).
func userConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".guild", "config.toml"), nil
}

// repoConfigPath returns the path to <repo>/.guild/config.toml (layer 3).
// It derives the repo root by walking up from dir looking for a ".guild"
// directory.  The caller passes the result of os.Getwd() or a test fixture.
// Returns ("", nil) when no per-project config directory is found — this is
// NOT an error (the directory is opt-in).
func repoConfigPath(startDir string) (string, error) {
	dir := startDir
	for {
		candidate := filepath.Join(dir, ".guild", "config.toml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding .guild/
			return "", nil
		}
		dir = parent
	}
}

// fileLayer decodes a TOML file at path into dst, applying ONLY the keys
// declared in the file (using BurntSushi/toml MetaData.IsDefined for
// granular per-key detection).  Missing file is silently ignored (not an
// error).
//
// The function updates each field of dst individually so that keys absent
// from the TOML file do NOT clobber values already present from a lower layer.
func fileLayer(path string, dst *Config) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	// Decode into a temporary Config so we can inspect MetaData.IsDefined.
	var tmp Config
	md, err := toml.Decode(string(raw), &tmp)
	if err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Apply only the keys that were present in this file.
	if md.IsDefined("project") {
		dst.Project = tmp.Project
	}
	if md.IsDefined("scoring", "w_fts") {
		dst.Scoring.WFTS = tmp.Scoring.WFTS
	}
	if md.IsDefined("scoring", "w_recency") {
		dst.Scoring.WRecency = tmp.Scoring.WRecency
	}
	if md.IsDefined("scoring", "half_life_days") {
		dst.Scoring.HalfLifeDays = tmp.Scoring.HalfLifeDays
	}
	if md.IsDefined("scoring", "title_match_boost") {
		dst.Scoring.TitleMatchBoost = tmp.Scoring.TitleMatchBoost
	}
	if md.IsDefined("scoring", "title_token_boost") {
		dst.Scoring.TitleTokenBoost = tmp.Scoring.TitleTokenBoost
	}
	if md.IsDefined("inscribe", "principle_max_words") {
		dst.Inscribe.PrincipleMaxWords = tmp.Inscribe.PrincipleMaxWords
	}
	if md.IsDefined("inscribe", "bloat_severe_threshold") {
		dst.Inscribe.BloatSevereThreshold = tmp.Inscribe.BloatSevereThreshold
	}
	if md.IsDefined("inscribe", "valid_days") {
		if err := mergeValidDays(path, tmp.Inscribe.ValidDays, dst); err != nil {
			return err
		}
	}
	if md.IsDefined("telemetry", "usage_log") {
		dst.Telemetry.UsageLog = tmp.Telemetry.UsageLog
	}
	if md.IsDefined("daemon", "autostart") {
		dst.Daemon.Autostart = tmp.Daemon.Autostart
	}
	if md.IsDefined("daemon", "watch") {
		dst.Daemon.Watch = tmp.Daemon.Watch
	}
	if md.IsDefined("daemon", "renewal_cap_per_pass") {
		dst.Daemon.RenewalCapPerPass = tmp.Daemon.RenewalCapPerPass
	}
	if md.IsDefined("daemon", "watch_debounce_ms") {
		dst.Daemon.WatchDebounceMS = tmp.Daemon.WatchDebounceMS
	}
	if md.IsDefined("daemon", "lease_ttl") {
		dst.Daemon.LeaseTTLSeconds = tmp.Daemon.LeaseTTLSeconds
	}
	if md.IsDefined("daemon", "heartbeat_interval") {
		dst.Daemon.HeartbeatIntervalSeconds = tmp.Daemon.HeartbeatIntervalSeconds
	}
	if md.IsDefined("daemon", "reap_interval") {
		dst.Daemon.ReapIntervalSeconds = tmp.Daemon.ReapIntervalSeconds
	}
	if md.IsDefined("sleep", "enabled") {
		dst.Sleep.Enabled = tmp.Sleep.Enabled
	}
	if md.IsDefined("sleep", "idle_minutes") {
		dst.Sleep.IdleMinutes = tmp.Sleep.IdleMinutes
	}
	if md.IsDefined("sleep", "pass_budget_seconds") {
		dst.Sleep.PassBudgetSeconds = tmp.Sleep.PassBudgetSeconds
	}
	return nil
}

// mergeValidDays validates the [inscribe.valid_days] table decoded from
// the file at path and applies it onto dst per key, so kinds absent from
// this file keep the value from the layer below. Unknown kind keys and
// negative values fail the load with an error naming the bad key/value;
// keys are visited in sorted order so the first-named offender is
// deterministic.
func mergeValidDays(path string, fromFile map[string]int, dst *Config) error {
	kinds := make([]string, 0, len(fromFile))
	for kind := range fromFile {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if !loreKinds[kind] {
			return fmt.Errorf(
				"config: %s: [inscribe.valid_days]: unknown kind %q (valid: idea, research, decision, observation, principle)",
				path, kind)
		}
		if days := fromFile[kind]; days < 0 {
			return fmt.Errorf(
				"config: %s: [inscribe.valid_days]: %s = %d is negative (use 0 for never stale)",
				path, kind, days)
		}
	}
	if dst.Inscribe.ValidDays == nil {
		dst.Inscribe.ValidDays = make(map[string]int, len(fromFile))
	}
	for _, kind := range kinds {
		dst.Inscribe.ValidDays[kind] = fromFile[kind]
	}
	return nil
}

// envLayer applies environment-variable overrides (layer 4).
//
// Variables honoured:
//   - GUILD_PROJECT        → Config.Project
//   - GUILD_NO_USAGE_LOG=1 → Config.NoUsageLog = true; also sets Telemetry.UsageLog = false
//   - GUILD_NO_EMOJI=1     → Config.NoEmoji = true
//   - GUILD_NO_DAEMON=1    → Config.Daemon.Autostart = false (also stops the
//     shim from dialing or spawning a daemon for this process)
//   - GUILD_NO_SLEEP=1     → Config.Sleep.Enabled = false (the running daemon
//     keeps serving but never fires an idle dream pass)
//   - GUILD_NO_WATCH=1     → Config.Daemon.Watch = false (the running daemon
//     keeps serving but never starts a project watcher; staleness falls back
//     to the query-time check)
func envLayer(dst *Config) {
	if v := os.Getenv("GUILD_PROJECT"); v != "" {
		dst.Project = v
	}
	if parseBoolEnv("GUILD_NO_USAGE_LOG") {
		dst.NoUsageLog = true
		dst.Telemetry.UsageLog = false
	}
	if parseBoolEnv("GUILD_NO_EMOJI") {
		dst.NoEmoji = true
	}
	if parseBoolEnv("GUILD_NO_DAEMON") {
		dst.Daemon.Autostart = false
	}
	if parseBoolEnv("GUILD_NO_SLEEP") {
		dst.Sleep.Enabled = false
	}
	if parseBoolEnv("GUILD_NO_WATCH") {
		dst.Daemon.Watch = false
	}
}

// parseBoolEnv returns true for env values "1", "true", "yes" (case-insensitive).
func parseBoolEnv(key string) bool {
	v := os.Getenv(key)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		// Accept "1" / "yes" as truthy beyond what ParseBool handles.
		return v == "1" || v == "yes" || v == "YES"
	}
	return b
}

// flagLayer applies CLI flag overrides (layer 5).
//
// Flags consulted (all optional — if not defined in the FlagSet they are
// silently skipped):
//   - --project / -p         → Config.Project
//   - --no-emoji              → Config.NoEmoji
//   - --no-usage-log          → Config.NoUsageLog
//   - --no-daemon             → Config.Daemon.Autostart = false
//   - --w-fts                 → Config.Scoring.WFTS
//   - --w-recency             → Config.Scoring.WRecency
func flagLayer(flags *pflag.FlagSet, dst *Config) {
	if flags == nil {
		return
	}
	applyStringFlag(flags, "project", &dst.Project)
	applyBoolFlag(flags, "no-emoji", &dst.NoEmoji)
	applyBoolFlag(flags, "no-usage-log", &dst.NoUsageLog)
	// --no-daemon is a disable switch: when set it forces autostart off.
	// Absent, it leaves whatever the lower layers resolved untouched.
	applyDisableFlag(flags, "no-daemon", &dst.Daemon.Autostart)
	applyFloat64Flag(flags, "w-fts", &dst.Scoring.WFTS)
	applyFloat64Flag(flags, "w-recency", &dst.Scoring.WRecency)
}

// applyStringFlag copies a flag value into dst only when the flag is
// registered in fs AND was explicitly set on the command line.
func applyStringFlag(fs *pflag.FlagSet, name string, dst *string) {
	f := fs.Lookup(name)
	if f == nil || !f.Changed {
		return
	}
	*dst = f.Value.String()
}

// applyBoolFlag copies a bool flag value into dst only when explicitly set.
func applyBoolFlag(fs *pflag.FlagSet, name string, dst *bool) {
	f := fs.Lookup(name)
	if f == nil || !f.Changed {
		return
	}
	b, err := strconv.ParseBool(f.Value.String())
	if err == nil {
		*dst = b
	}
}

// applyDisableFlag forces dst to false when a "--no-x" disable flag is
// registered and was passed truthy on the command line. Used for knobs
// whose flag name is the negation of the config field (e.g. --no-daemon
// turning Daemon.Autostart off); an unset or false flag leaves dst at
// the value the lower layers resolved.
func applyDisableFlag(fs *pflag.FlagSet, name string, dst *bool) {
	f := fs.Lookup(name)
	if f == nil || !f.Changed {
		return
	}
	if b, err := strconv.ParseBool(f.Value.String()); err == nil && b {
		*dst = false
	}
}

// applyFloat64Flag copies a float64 flag value into dst only when explicitly set.
func applyFloat64Flag(fs *pflag.FlagSet, name string, dst *float64) {
	f := fs.Lookup(name)
	if f == nil || !f.Changed {
		return
	}
	fv, err := strconv.ParseFloat(f.Value.String(), 64)
	if err == nil {
		*dst = fv
	}
}

// Load builds the merged Config by applying all five layers in order.
//
// flags may be nil (CLI callers pass cobra's FlagSet; the MCP server passes nil).
// Missing config files are not errors — built-in defaults fill the gaps.
//
// Callers own the returned *Config and may mutate it freely.
func Load(flags *pflag.FlagSet) (*Config, error) {
	// Layer 1 — built-in defaults.
	cfg := defaults()

	// Layer 2 — user-wide ~/.guild/config.toml.
	userPath, err := userConfigDir()
	if err != nil {
		return nil, err
	}
	if err := fileLayer(userPath, &cfg); err != nil {
		return nil, err
	}

	// Layer 3 — per-project <repo>/.guild/config.toml.
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("config: getwd: %w", err)
	}
	repoPath, err := repoConfigPath(cwd)
	if err != nil {
		return nil, err
	}
	if err := fileLayer(repoPath, &cfg); err != nil {
		return nil, err
	}

	// Layer 4 — environment variables.
	envLayer(&cfg)

	// Layer 5 — CLI flags (highest precedence).
	flagLayer(flags, &cfg)

	// Reconcile convenience booleans: if any layer set NoUsageLog=true,
	// keep Telemetry.UsageLog consistent (the canonical storage for
	// persistence to TOML; NoUsageLog is the merged runtime bool).
	if !cfg.Telemetry.UsageLog {
		cfg.NoUsageLog = true
	}

	// A non-positive lease TTL, heartbeat interval, or reap interval is
	// meaningless (a zero or negative window would expire every lease
	// instantly, or spin a sweep loop), so a bad value silently falls back
	// to the built-in default rather than failing the load: the daemon's
	// liveness layer must not be disarmed by a typo in config.toml.
	// Defaults are read from the same baseline every other layer applies
	// deltas on top of.
	base := defaults()
	if cfg.Daemon.LeaseTTLSeconds <= 0 {
		cfg.Daemon.LeaseTTLSeconds = base.Daemon.LeaseTTLSeconds
	}
	if cfg.Daemon.HeartbeatIntervalSeconds <= 0 {
		cfg.Daemon.HeartbeatIntervalSeconds = base.Daemon.HeartbeatIntervalSeconds
	}
	if cfg.Daemon.ReapIntervalSeconds <= 0 {
		cfg.Daemon.ReapIntervalSeconds = base.Daemon.ReapIntervalSeconds
	}

	return &cfg, nil
}
