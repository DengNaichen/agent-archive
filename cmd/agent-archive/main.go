package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DengNaichen/agent-archive/internal/app"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := app.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		message := fault.Message(err)
		code := 1
		if errors.Is(err, context.Canceled) {
			message = "interrupted; rerun sync to resume by checking remote events"
			code = 130
		}
		if e := json.NewEncoder(os.Stderr).Encode(map[string]any{"success": false, "error": message}); e != nil {
			os.Exit(1)
		}
		os.Exit(code)
	}
}
