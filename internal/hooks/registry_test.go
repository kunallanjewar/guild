package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultBase_Contract pins the exact default base config: event
// set, matchers, and command strings. These strings are the contract
// the payload verbs ship against; any drift here is a breaking change
// to every installed hook.
func TestDefaultBase_Contract(t *testing.T) {
	base := DefaultBase()

	if len(base) != 3 {
		t.Fatalf("DefaultBase has %d events; want 3 (SessionStart, PreCompact, UserPromptSubmit)", len(base))
	}

	cases := []struct {
		event   string
		matcher string
		command string
	}{
		{"SessionStart", "startup|resume|clear|compact", "guild quest brief --auto"},
		{"PreCompact", "auto|manual", "guild quest brief --auto --capture"},
		{"UserPromptSubmit", "", "guild lore appraise --inject --from-stdin-json"},
	}
	for _, c := range cases {
		groups, ok := base[c.event]
		if !ok {
			t.Errorf("DefaultBase missing event %s", c.event)
			continue
		}
		if len(groups) != 1 {
			t.Errorf("%s: %d groups; want 1", c.event, len(groups))
			continue
		}
		g := groups[0]
		if g.Matcher != c.matcher {
			t.Errorf("%s matcher = %q; want %q", c.event, g.Matcher, c.matcher)
		}
		if len(g.Hooks) != 1 {
			t.Errorf("%s: %d hooks; want 1", c.event, len(g.Hooks))
			continue
		}
		if g.Hooks[0].Type != "command" {
			t.Errorf("%s hook type = %q; want %q", c.event, g.Hooks[0].Type, "command")
		}
		if g.Hooks[0].Command != c.command {
			t.Errorf("%s command = %q; want %q", c.event, g.Hooks[0].Command, c.command)
		}
	}
}

// TestDefaultBase_NoMatcherOnUserPromptSubmit asserts the matcher field
// is OMITTED from the JSON rendering of UserPromptSubmit: the event
// does not support a matcher, and harnesses may reject one.
func TestDefaultBase_NoMatcherOnUserPromptSubmit(t *testing.T) {
	data, err := json.Marshal(DefaultBase()["UserPromptSubmit"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "matcher") {
		t.Errorf("UserPromptSubmit rendering contains a matcher field: %s", data)
	}
}

// TestEnsureBase_CreatesFileWithDefaults covers the first-install path:
// the file lands under ~/.guild with owner-only perms and round-trips
// to the default config.
func TestEnsureBase_CreatesFileWithDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, created, err := EnsureBase()
	if err != nil {
		t.Fatalf("EnsureBase: %v", err)
	}
	if !created {
		t.Error("EnsureBase created = false on first run; want true")
	}

	path := filepath.Join(home, ".guild", "hooks-base.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("hooks-base.json perm = %o; want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Join(home, ".guild"))
	if err != nil {
		t.Fatalf("stat ~/.guild: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("~/.guild perm = %o; want 700", perm)
	}

	want, err := json.Marshal(DefaultBase())
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}
	got, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal loaded config: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("EnsureBase config != DefaultBase:\n got %s\nwant %s", got, want)
	}

	// Second run: no-op, created=false.
	_, created, err = EnsureBase()
	if err != nil {
		t.Fatalf("EnsureBase (second): %v", err)
	}
	if created {
		t.Error("EnsureBase created = true on second run; want false")
	}
}

// TestLoadBase_MissingFileReturnsDefaults: read-only verbs must work
// before install ever ran.
func TestLoadBase_MissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := LoadBase()
	if err != nil {
		t.Fatalf("LoadBase: %v", err)
	}
	if len(cfg) != len(DefaultBase()) {
		t.Errorf("LoadBase on missing file returned %d events; want defaults (%d)", len(cfg), len(DefaultBase()))
	}
}

// TestLoadBase_CorruptFileIsError: never silently fall back to defaults
// over a config the user was editing.
func TestLoadBase_CorruptFileIsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".guild")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks-base.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBase(); err == nil {
		t.Error("LoadBase on corrupt file returned nil error; want parse error")
	}
}

// TestLoadBase_UserOverrideWins: an existing file replaces the built-in
// defaults wholesale.
func TestLoadBase_UserOverrideWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	custom := Config{
		"SessionStart": {{Matcher: "startup", Hooks: []Command{{Type: "command", Command: "guild quest brief --auto"}}}},
	}
	if err := SaveBase(custom); err != nil {
		t.Fatalf("SaveBase: %v", err)
	}
	got, err := LoadBase()
	if err != nil {
		t.Fatalf("LoadBase: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadBase returned %d events; want 1 (the override, not defaults)", len(got))
	}
	if got["SessionStart"][0].Matcher != "startup" {
		t.Errorf("override matcher = %q; want %q", got["SessionStart"][0].Matcher, "startup")
	}
}

// TestWriteFileAtomic covers the write primitive: content lands, perms
// hold, an existing file is replaced, and no tmp files leak.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := WriteFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic (overwrite): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "two" {
		t.Errorf("content = %q; want %q", got, "two")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o; want 600", perm)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("tmp file leaked: %s", e.Name())
		}
	}
}
