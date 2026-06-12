package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

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
// errNotImplemented placeholder and never pull in the SDK.
func init() {
	wireDaemonRun()
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
		Version:       version,
		Sessions:      host.ServeSession,
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
