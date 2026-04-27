package cli

import (
	"log"
	"os"

	"github.com/shiftu/shipd/internal/server"
	"github.com/shiftu/shipd/internal/storage"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var (
		addr        string
		dataDir     string
		publicReads bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the shipd server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := storage.Open(dataDir)
			if err != nil {
				return err
			}
			defer st.Close()
			ctx, cancel := signalContext()
			defer cancel()
			s := server.New(server.Config{
				Addr:           addr,
				PublicReads:    publicReads,
				BootstrapToken: os.Getenv("SHIPD_BOOTSTRAP_TOKEN"),
			}, st, log.Default())
			return s.ListenAndServe(ctx)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	cmd.Flags().StringVar(&dataDir, "data-dir", "./data", "directory for SQLite + blobs")
	cmd.Flags().BoolVar(&publicReads, "public-reads", false, "allow unauthenticated read endpoints")
	return cmd
}
