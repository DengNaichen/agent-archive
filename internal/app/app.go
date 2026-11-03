// Package app maps CLI requests to history and cloud operations; it does not
// interpret source formats or implement storage/network protocols.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/cloud"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
	"io"
	"os"
	"path/filepath"
	"time"
)

const Version = "0.1.0-dev"

type stringsFlag []string

func (s *stringsFlag) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string, out, stderr io.Writer) (err error) {
	root := flag.NewFlagSet("agent-archive", flag.ContinueOnError)
	root.SetOutput(stderr)
	var db, config string
	root.StringVar(&db, "local-db", "", "snapshot cleaned.sqlite path")
	root.StringVar(&db, "db", "", "snapshot cleaned.sqlite path")
	root.StringVar(&config, "config", "", "private ClickHouse connection JSON")
	root.Usage = func() {
		fmt.Fprintln(stderr, "agent-archive [--local-db PATH] [--config PATH] <command> [options]\ncommands: collect, status, plan, verify-local, ping, init, sync, verify, version\ncollect --source codex|claude-code --output NEW_DIR [--home SOURCE_HOME]\nplan/sync/verify require --all or repeatable --session ID\nlocal commands never contact the cloud; sync performs actual uploads")
	}
	if e := root.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	rest := root.Args()
	if len(rest) == 0 {
		root.Usage()
		return fault.New("a command is required")
	}
	command := rest[0]
	emit := func(v any) error {
		if e := json.NewEncoder(out).Encode(v); e != nil {
			return fmt.Errorf("write output: %w", e)
		}
		return nil
	}
	if command == "version" {
		return emit(map[string]any{"version": Version})
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	var ids stringsFlag
	var all bool
	batchRows, batchBytes, delay := 500, 4<<20, 2.0
	var source, home, output string
	switch command {
	case "collect":
		f.StringVar(&source, "source", "", "codex or claude-code")
		f.StringVar(&home, "home", "", "source home directory")
		f.StringVar(&output, "output", "", "new private snapshot directory")
	case "plan", "sync", "verify":
		f.BoolVar(&all, "all", false, "select all sessions")
		f.Var(&ids, "session", "select session (repeatable)")
		if command == "sync" {
			f.IntVar(&batchRows, "batch-rows", 500, "maximum rows per batch")
			f.IntVar(&batchBytes, "batch-bytes", 4<<20, "maximum uncompressed bytes per batch")
			f.Float64Var(&delay, "delay", 2, "seconds after each batch")
		}
	case "status", "verify-local", "ping", "init":
	default:
		return fault.New("unknown command")
	}
	if e := f.Parse(rest[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return nil
		}
		return e
	}
	if f.NArg() != 0 {
		return fault.New("unexpected positional arguments")
	}
	if command == "collect" {
		if source != "codex" && source != "claude-code" && source != "claude_code" {
			return fault.New("choose --source codex or claude-code")
		}
		if output == "" {
			return fault.New("--output is required")
		}
		if home == "" {
			user, e := os.UserHomeDir()
			if e != nil {
				return e
			}
			home = filepath.Join(user, ".claude")
			if source == "codex" {
				home = os.Getenv("CODEX_HOME")
				if home == "" {
					home = filepath.Join(user, ".codex")
				}
			}
		}
		result, e := history.Collect(ctx, history.CollectOptions{Source: source, Home: home, Output: output, Progress: emit})
		if e != nil {
			return e
		}
		return emit(result)
	}
	if command == "ping" || command == "init" {
		if config == "" {
			return fault.New("--config is required")
		}
		client, e := cloud.Load(config)
		if e != nil {
			return e
		}
		start := time.Now()
		if command == "ping" {
			e = client.Ping(ctx)
		} else {
			e = client.Init(ctx)
		}
		if e != nil {
			return e
		}
		return emit(map[string]any{"success": true, "command": command, "elapsed_seconds": time.Since(start).Seconds()})
	}
	if db == "" {
		return fault.New("--local-db is required")
	}
	store, e := history.Open(db)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	switch command {
	case "status":
		result, e := store.Status(ctx)
		if e != nil {
			return e
		}
		return emit(result)
	case "verify-local":
		result, e := store.Verify(ctx, emit)
		if e != nil {
			return e
		}
		return emit(result)
	}
	selected, e := store.Sessions(ctx, ids, all)
	if e != nil {
		return e
	}
	if command == "plan" {
		result, e := store.Plan(ctx, selected)
		if e != nil {
			return e
		}
		return emit(result)
	}
	if config == "" {
		return fault.New("--config is required")
	}
	if delay < 0 || delay > 86400 {
		return fault.New("delay must be between zero and 86400 seconds")
	}
	client, e := cloud.Load(config)
	if e != nil {
		return e
	}
	result, e := cloud.Synchronize(ctx, store, client, selected, cloud.SyncOptions{Upload: command == "sync", BatchRows: batchRows, BatchBytes: batchBytes, Delay: time.Duration(delay * float64(time.Second)), Progress: emit})
	if e != nil {
		return e
	}
	return emit(result)
}
