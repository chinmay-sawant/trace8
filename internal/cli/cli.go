package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/chinmay-sawant/trace8/internal/server"
)

const Version = "0.0.1"

func Run(args []string) int {
	fs := flag.NewFlagSet("trace8", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print version and exit")
	serveMode := fs.Bool("server", false, "run the local HTTP server and block")
	addr := fs.String("addr", ":8080", "listen address for --server")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "Usage: trace8 [--version] [--server] [--addr listen-addr] [name]")
		return 2
	}
	if *showVersion {
		fmt.Println("trace8 " + Version)
		return 0
	}
	if *serveMode {
		cfg := server.Config{Addr: *addr, FrontendDir: "frontend"}
		fmt.Printf("trace8 server on %s (frontend %s)\n", cfg.Addr, cfg.FrontendDir)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := server.New(cfg).Run(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	name := "world"
	if rest := fs.Args(); len(rest) > 0 {
		name = rest[0]
	}
	fmt.Printf("hello %s\n", name)
	return 0
}
