package cli

import (
	"errors"
	"fmt"

	"github.com/shiftu/shipd/internal/client"
	"github.com/spf13/cobra"
)

func clientFromFlags(cmd *cobra.Command) (*client.Client, error) {
	server, _ := cmd.Flags().GetString("server")
	token, _ := cmd.Flags().GetString("token")
	if server == "" {
		return nil, errors.New("--server (or $SHIPD_SERVER) is required")
	}
	return client.New(server, token), nil
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// humanSize formats bytes as B/KiB/MiB/GiB.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffix)
}
