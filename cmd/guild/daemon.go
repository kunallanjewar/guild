package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/cli"
	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/mcp"
)

// wireDaemonRun wires the `guild daemon run` cobra RunE onto
// internal/cli's daemonRunCmd placeholder, mirroring wireMCPServe in
// mcp.go: root.go stays daemon-agnostic, and the assembly of the
// daemon listener + MCP session host happens only in the cmd/guild
// build, where both packages are already linked.
func wireDaemonRun() {
	cli.SetDaemonRunRunE(runDaemonRun)
}

// init runs at binary startup, before cobra.Execute. Same rationale as
// mcp.go's init: tests that import internal/cli directly keep the
// errNotImplemented placeholders and never pull in the SDK. The
// lifecycle verbs (start/stop/status/restart) ride the same seam: the
// command definitions live in internal/cli/daemon_cmd.go, the handlers
// here.
func init() {
	wireDaemonRun()
	cli.SetDaemonLifecycleRunEs(runDaemonStart, runDaemonStop, runDaemonStatus, runDaemonRestart)
}

// runDaemonRun is the cobra RunE for `guild daemon run`. Signal
// handling mirrors runMCPServe: a [signal.NotifyContext] on SIGINT +
// SIGTERM cancels the daemon's ctx, which closes the listener, ends
// every in-flight session, and removes the socket + discovery file
// before Run returns nil, so Ctrl-C and `kill <pid>` exit 0 with a
// clean ~/.guild.
func runDaemonRun(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One host per daemon process: the shared provider bundle behind
	// every session. The daemon server itself stays MCP-agnostic and
	// receives the session handler as a seam.
	host := mcp.NewDaemonHost()
	srv, err := daemon.NewServer(daemon.Config{
		Version:  version,
		Sessions: host.ServeSession,
		// Terminal CLI verbs routed over the JSON-exec RPC execute here
		// with CLI-equivalent Deps; the shared embed providers keep one
		// embedder per daemon process across MCP sessions and routed
		// CLI searches alike.
		Exec:          cli.NewDaemonExecHandler(host.QuestEmbedSource(), host.LoreEmbedSource()),
		EmbedderState: host.EmbedderState,
		Logger:        host.Logger(),
	})
	if err != nil {
		return fmt.Errorf("daemon run: %w", err)
	}

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("daemon run: %w", err)
	}
	return nil
}

// ───────────────────────── lifecycle verbs ──────────────────────────

// daemonStatusExitNotRunning is the exit code `guild daemon status`
// uses when no daemon is running: distinct from 0 (running) and 1
// (probe error), mirroring the LSB/systemctl convention where 3 means
// "program is not running". Documented in the command's Long help in
// internal/cli/daemon_cmd.go.
const daemonStatusExitNotRunning = 3

// runDaemonStart is the cobra RunE for `guild daemon start`.
func runDaemonStart(cmd *cobra.Command, _ []string) error {
	res, err := daemon.Start(daemon.StartOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon start: %w", err)
	}
	printStartResult(cmd.OutOrStdout(), res)
	return nil
}

// printStartResult renders a StartResult: shared by start and restart
// so both verbs report a launch identically.
func printStartResult(out io.Writer, res daemon.StartResult) {
	if res.AlreadyRunning {
		fmt.Fprintf(out, "guild daemon: already running pid=%d socket=%s\n", res.PID, res.SocketPath)
	} else {
		fmt.Fprintf(out, "guild daemon: started pid=%d socket=%s\n", res.PID, res.SocketPath)
	}
	if nudge := daemon.FormatVersionDrift(res.DaemonVersion, version); nudge != "" {
		fmt.Fprintln(out, nudge)
	}
}

// runDaemonStop is the cobra RunE for `guild daemon stop`.
func runDaemonStop(cmd *cobra.Command, _ []string) error {
	res, err := daemon.Stop(daemon.StopOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon stop: %w", err)
	}
	printStopResult(cmd.OutOrStdout(), res)
	return nil
}

// printStopResult renders a StopResult: shared by stop and restart.
func printStopResult(out io.Writer, res daemon.StopResult) {
	switch {
	case !res.WasRunning:
		fmt.Fprintln(out, "guild daemon: not running")
	case res.Killed:
		fmt.Fprintf(out, "guild daemon: stopped pid=%d (escalated to SIGKILL after grace period)\n", res.PID)
	default:
		fmt.Fprintf(out, "guild daemon: stopped pid=%d\n", res.PID)
	}
}

// runDaemonStatus is the cobra RunE for `guild daemon status`.
func runDaemonStatus(cmd *cobra.Command, _ []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	rep, err := daemon.QueryStatus(version, 0)
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}

	out := cmd.OutOrStdout()
	switch {
	case asJSON:
		if err := writeDaemonStatusJSON(out, rep); err != nil {
			return fmt.Errorf("daemon status: %w", err)
		}
	case rep.Running:
		fmt.Fprintf(out, "guild daemon: running pid=%d version=%s uptime=%s sessions=%d embedder=%s socket=%s\n",
			rep.Status.PID, rep.Status.Version, rep.Uptime,
			rep.Status.ActiveSessions, rep.Status.EmbedderState, rep.SocketPath)
		if rep.DriftNudge != "" {
			fmt.Fprintln(out, rep.DriftNudge)
		}
	default:
		fmt.Fprintln(out, "guild daemon: not running")
	}

	if !rep.Running {
		// Distinct exit code for "not running" (documented in Long).
		// os.Exit is safe here: the report is already written and this
		// process holds no state needing teardown; main.go's own error
		// path also exits directly.
		os.Exit(daemonStatusExitNotRunning)
	}
	return nil
}

// daemonStatusView is the --json shape of `guild daemon status`. Keys
// are stable; additions are append-only.
type daemonStatusView struct {
	Running        bool   `json:"running"`
	PID            int    `json:"pid,omitempty"`
	Version        string `json:"version,omitempty"`
	SelfVersion    string `json:"self_version"`
	StartedAt      string `json:"started_at,omitempty"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	ActiveSessions int    `json:"active_sessions"`
	EmbedderState  string `json:"embedder_state,omitempty"`
	SocketPath     string `json:"socket_path,omitempty"`
	VersionDrift   bool   `json:"version_drift"`
}

// writeDaemonStatusJSON emits the one-object JSON form of a status
// report, newline-terminated for line-oriented consumers.
func writeDaemonStatusJSON(out io.Writer, rep daemon.StatusReport) error {
	view := daemonStatusView{
		Running:      rep.Running,
		SelfVersion:  rep.SelfVersion,
		VersionDrift: rep.Drift,
	}
	if rep.Running {
		view.PID = rep.Status.PID
		view.Version = rep.Status.Version
		view.StartedAt = rep.Status.StartedAt.Format(time.RFC3339)
		view.UptimeSeconds = int64(rep.Uptime / time.Second)
		view.ActiveSessions = rep.Status.ActiveSessions
		view.EmbedderState = rep.Status.EmbedderState
		view.SocketPath = rep.SocketPath
	}
	data, err := json.Marshal(view)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

// runDaemonRestart is the cobra RunE for `guild daemon restart`: stop
// (idempotent) then start, reusing the same helpers as the standalone
// verbs.
func runDaemonRestart(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()

	stopRes, err := daemon.Stop(daemon.StopOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	printStopResult(out, stopRes)

	startRes, err := daemon.Start(daemon.StartOptions{SelfVersion: version})
	if err != nil {
		return fmt.Errorf("daemon restart: %w", err)
	}
	printStartResult(out, startRes)
	return nil
}
