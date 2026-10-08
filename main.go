package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"chizu/src/cmd"
)

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := cmd.New(os.Stdout, os.Stderr).Run(ctx, os.Args)
	stop()

	// Exit codes follow grep: 1 means no results, 2 means an error.
	switch {
	case err == nil:
	case errors.Is(err, cmd.ErrNoResults):
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "chizu:", err)
		os.Exit(2)
	}
}
