package release_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the pinned generator and both Ruby DSL paths, including older
// Homebrew's permissive respond_to_missing? behavior. Real Homebrew install
// checks complement this fixture when preparing a packaging change.
func TestRenderedHomebrewCaskCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew release tooling uses Unix hosts")
	}
	for _, name := range []string{"goreleaser", "ruby"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable; native cask validation runs during release preparation", name)
		}
	}
	dir := t.TempDir()
	run(t, dir, "git", "init", "--quiet")
	run(t, dir, "git", "config", "user.name", "Release fixture")
	run(t, dir, "git", "config", "user.email", "release-fixture@example.invalid")
	run(t, dir, "git", "remote", "add", "origin", "https://github.com/example/application.git")
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "guild"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.invalid/release-fixture\n\ngo 1.26.9\n")
	writeFile(t, filepath.Join(dir, "cmd", "guild", "main.go"), "package main\nvar version, commit, date string\nfunc main(){}\n")
	writeFile(t, filepath.Join(dir, "LICENSE"), "Fixture license\n")
	writeFile(t, filepath.Join(dir, "README.md"), "Fixture readme\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-qm", "Fixture")
	run(t, dir, "git", "tag", "v1.2.3")
	config := filepath.Join(dir, "goreleaser.yml")
	run(t, dir, "sh", filepath.Join(sourceRoot(t), ".github", "scripts", "goreleaser-config.sh"), filepath.Join(sourceRoot(t), ".goreleaser.yml"), config)
	run(t, dir, "goreleaser", "release", "--snapshot", "--clean", "--skip=before,publish,sign", "--config", config)
	cask := filepath.Join(dir, "dist", "homebrew", "Casks", "guild.rb")
	fixture := filepath.Join(sourceRoot(t), "tests", "release", "testdata", "cask_compatibility.rb")
	for _, api := range []string{"legacy", "steps"} {
		for _, platform := range []string{"darwin", "linux"} {
			for _, arch := range []string{"amd64", "arm64"} {
				t.Run(api+"/"+platform+"/"+arch, func(t *testing.T) {
					var got struct {
						Calls [][]string `json:"calls"`
						URL   string     `json:"url"`
						SHA   string     `json:"sha"`
					}
					if err := json.Unmarshal([]byte(run(t, dir, "ruby", fixture, cask, api, platform, arch)), &got); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(got.URL, "_"+platform+"_"+arch+".tar.gz") || len(got.SHA) != 64 {
						t.Fatalf("platform archive selection lost: %+v", got)
					}
					if platform == "linux" {
						if len(got.Calls) != 0 {
							t.Fatalf("macOS hook ran for Linux: %+v", got.Calls)
						}
						return
					}
					if len(got.Calls) != 1 || strings.Join(got.Calls[0], " ") != "/usr/bin/xattr -dr com.apple.quarantine /fixture/stage/guild" {
						t.Fatalf("quarantine command changed or duplicated: %+v", got.Calls)
					}
				})
			}
		}
	}
}
