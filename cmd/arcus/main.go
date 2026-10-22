// Command arcus manages an Arch Linux system as layered modules.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/grunclepug/arcus/internal/cli"
)

var Version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("arcus", Version)
		return
	}

	// Trap SIGINT, SIGTERM and SIGHUP instead of dying mid-operation: the
	// context is cancelled, the layer engine stops between files and rolls
	// back, and a running package manager (which gets a terminal Ctrl-C
	// itself) finishes or aborts its own transaction first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := cli.Run(ctx, Version, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "arcus:", err)
		os.Exit(1)
	}
}
