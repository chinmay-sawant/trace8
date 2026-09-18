package main

import (
	"os"

	"github.com/chinmay-sawant/trace8/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
