package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/DengNaichen/agent-archive/internal/app"
)

func main() { os.Exit(run()) }
func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		return app.WriteError(os.Stderr, err)
	}
	return 0
}
