// Package e2e is the Docker end-to-end harness: scripted full-loop guild
// scenarios driven over MCP stdio against an isolated container.
//
// The suite is opt-in. It runs only when GUILD_E2E_DOCKER=1 is set (the
// `make e2e` / `make e2e-docker` targets set it); otherwise every test
// skips immediately so `go test ./...` stays fast and docker-free.
//
// Isolation contract: all guild state lives inside a throwaway container
// (its own /home/guild/.guild), every container runs with --network none,
// and the test process itself swaps HOME to an empty canary directory
// before any scenario runs. TestMain fails the run if the canary picks up
// a .guild directory (or anything else unexpected), proving the suite
// never touched the host's real ~/.guild.
//
// Environment contract:
//
//	GUILD_E2E_DOCKER=1   enable the suite (required)
//	GUILD_E2E_IMAGE      image ref to test (default "guild:latest")
//	GUILD_E2E_UPDATE=1   rewrite golden transcripts instead of comparing
//	GUILD_E2E_MODE       "" | "direct" | "daemon" (see modeNote below)
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// envEnable gates the whole suite.
	envEnable = "GUILD_E2E_DOCKER"
	// envImage selects the image under test.
	envImage = "GUILD_E2E_IMAGE"
	// envUpdate switches golden comparison to golden regeneration.
	envUpdate = "GUILD_E2E_UPDATE"
	// envMode selects the execution mode. "direct" (or empty) drives
	// `guild mcp serve` per session, the only mode that exists today.
	// "daemon" is an intentional forward hook: it is accepted and runs
	// the exact same scenarios, so the future daemon can be dropped in
	// and asserted byte-identical against the same golden transcripts.
	envMode = "GUILD_E2E_MODE"

	defaultImage = "guild:latest"

	modeDirect = "direct"
	modeDaemon = "daemon"
)

// suite holds the process-wide harness configuration resolved in TestMain.
var suite struct {
	enabled bool
	image   string
	update  bool
	mode    string
}

// modeNote is the documented no-op contract for GUILD_E2E_MODE=daemon.
// Until the guild daemon ships, daemon mode runs the identical direct-mode
// scenarios. Once the daemon lands, the harness will start it inside the
// container before opening sessions; the golden transcripts are shared
// between modes on purpose so daemon-up vs daemon-down can be asserted
// byte-identical (the ADR-005 Phase 1 invariant).
const modeNote = "GUILD_E2E_MODE=daemon: daemon not shipped yet; running identical direct-mode scenarios (documented no-op hook)"

// requireE2E skips t unless the suite is enabled. Every scenario test
// calls this first so a plain `go test ./...` never touches docker.
func requireE2E(t *testing.T) {
	t.Helper()
	if !suite.enabled {
		t.Skipf("docker e2e disabled; run `make e2e-docker` (or set %s=1) to enable", envEnable)
	}
}

func TestMain(m *testing.M) {
	suite.enabled = os.Getenv(envEnable) == "1"
	if !suite.enabled {
		// Disabled: no canary, no docker probing. Tests self-skip.
		os.Exit(m.Run())
	}

	canary, err := setupSuite()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	if err := verifyCanary(canary); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: canary HOME violation: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	_ = os.RemoveAll(canary)
	os.Exit(code)
}

// setupSuite resolves the harness configuration, fails fast on missing
// prerequisites (never silently skip when explicitly enabled: that is a
// broken invocation, not a skip), and swaps HOME to the canary
// directory it returns.
func setupSuite() (string, error) {
	suite.image = os.Getenv(envImage)
	if suite.image == "" {
		suite.image = defaultImage
	}
	suite.update = os.Getenv(envUpdate) == "1"

	suite.mode = os.Getenv(envMode)
	switch suite.mode {
	case "", modeDirect:
		suite.mode = modeDirect
	case modeDaemon:
		fmt.Fprintln(os.Stderr, "e2e: "+modeNote)
	default:
		return "", fmt.Errorf("unknown %s=%q (want direct or daemon)", envMode, suite.mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Resolve the docker endpoint while HOME is still the real one: on
	// Docker Desktop the daemon socket lives under the user's home and is
	// found via ~/.docker/config.json contexts. After we swap HOME to the
	// canary the CLI would lose that context, so pin DOCKER_HOST first.
	if os.Getenv("DOCKER_HOST") == "" {
		out, err := exec.CommandContext(ctx,
			"docker", "context", "inspect", "-f", "{{.Endpoints.docker.Host}}").Output()
		if err == nil {
			if host := strings.TrimSpace(string(out)); host != "" {
				if err := os.Setenv("DOCKER_HOST", host); err != nil {
					return "", fmt.Errorf("set DOCKER_HOST: %w", err)
				}
			}
		}
	}

	if err := exec.CommandContext(ctx, "docker", "version").Run(); err != nil {
		return "", fmt.Errorf("%s=1 but docker is not reachable: %w", envEnable, err)
	}
	//nolint:gosec // suite.image comes from the operator's GUILD_E2E_IMAGE env, the same trust level as the test command line
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", suite.image).Run(); err != nil {
		return "", fmt.Errorf(
			"image %q not found; build it first (`make docker-build` or `make e2e-docker`): %w",
			suite.image, err)
	}

	// Canary HOME: the rest of the process (and every subprocess it
	// spawns, docker CLI included) sees an empty HOME. Anything that
	// tries to resolve the host ~/.guild lands here instead, and the
	// post-run audit in TestMain turns that into a hard failure.
	canary, err := os.MkdirTemp("", "guild-e2e-canary-")
	if err != nil {
		return "", fmt.Errorf("create canary HOME: %w", err)
	}
	if err := os.Setenv("HOME", canary); err != nil {
		return "", fmt.Errorf("set HOME to canary: %w", err)
	}
	return canary, nil
}

// verifyCanary asserts the canary HOME stayed untouched. A .guild entry
// is the violation this harness exists to catch; any other entry is
// flagged too, except .docker, which the docker CLI may create for its
// own metadata when pointed at a fresh HOME (tolerated and irrelevant to
// guild state).
func verifyCanary(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".guild")); err == nil {
		return fmt.Errorf("%s/.guild exists: a scenario leaked guild state onto the host", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat canary .guild: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read canary HOME: %w", err)
	}
	for _, e := range entries {
		if e.Name() == ".docker" {
			continue
		}
		return fmt.Errorf("unexpected entry %q appeared in canary HOME %s", e.Name(), dir)
	}
	return nil
}
