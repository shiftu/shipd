package main

import (
	"os"

	"github.com/shiftu/shipd/internal/cli"
)

var version = "dev"

func main() {
	root := cli.NewRoot(version)
	if err := root.Execute(); err != nil {
		// cobra already printed the error since SilenceErrors=false
		os.Exit(1)
	}
}
