package cli

import (
	"log"
	"os"

	"github.com/shiftu/shipd/internal/mcp"
	"github.com/spf13/cobra"
)

// `shipd mcp serve` runs an MCP server on stdio. It is meant to be launched by
// an MCP client (Claude Desktop, Cursor, etc.) which reads/writes JSON-RPC on
// the spawned process's stdio.
//
// All logging goes to stderr to keep stdout clean for the protocol.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol bridge for Agents",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run a stdio MCP server that exposes shipd verbs as tools",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()

			logger := log.New(os.Stderr, "[shipd-mcp] ", log.LstdFlags)
			s := mcp.NewServer("shipd", versionFromCmd(cmd), logger)
			mcp.RegisterShipdTools(s, c, c.BaseURL)
			logger.Printf("ready")
			return s.Serve(ctx, os.Stdin, os.Stdout)
		},
	})
	return cmd
}

// versionFromCmd surfaces the root command's Version (set in NewRoot) so the
// MCP serverInfo block matches the rest of the CLI.
func versionFromCmd(cmd *cobra.Command) string {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Version != "" {
			return c.Version
		}
	}
	return "dev"
}
