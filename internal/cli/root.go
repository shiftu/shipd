package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// NewRoot wires all subcommands and shared flags.
func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "shipd",
		Short:         "AI-native package distribution platform",
		Long:          "shipd ships build artifacts. CLI-first, agent-friendly, single binary.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.PersistentFlags().String("server", os.Getenv("SHIPD_SERVER"), "shipd server URL (or $SHIPD_SERVER)")
	root.PersistentFlags().String("token", os.Getenv("SHIPD_TOKEN"), "auth token (or $SHIPD_TOKEN)")

	root.AddCommand(
		newServeCmd(),
		newPublishCmd(),
		newListCmd(),
		newInfoCmd(),
		newDownloadCmd(),
		newYankCmd(),
		newTokenCmd(),
	)
	return root
}

// signalContext returns a context that is cancelled on SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
