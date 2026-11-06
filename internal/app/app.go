// Package app owns the CLI workflow and its human/machine presentation.
// Source interpretation, SQLite and ClickHouse protocols remain in their modules.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"
	"github.com/DengNaichen/agent-archive/internal/cloud"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
	"github.com/DengNaichen/agent-archive/internal/job"
	"github.com/DengNaichen/agent-archive/internal/runstate"
)

const Version = "0.3.0-dev"
const defaultBatchRows = 500
const defaultBatchBytes = 4 << 20
const defaultDelaySeconds = 2.0

type stringsFlag []string

func (s *stringsFlag) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string, out, stderr io.Writer) (err error) {
	var db, config string
	jsonMode, version := false, false
	command := ""
	defer func() {
		if err != nil {
			err = &commandError{err: err, json: jsonMode, command: command}
		}
	}()
	shared := func(f *flag.FlagSet) {
		f.StringVar(&db, "snapshot", db, "snapshot directory or database")
		f.StringVar(&db, "local-db", db, "alias for --snapshot")
		f.StringVar(&db, "db", db, "alias for --snapshot")
		f.StringVar(&config, "config", config, "private ClickHouse connection JSON")
		f.BoolVar(&jsonMode, "json", jsonMode, "newline-delimited JSON output")
		f.BoolVar(&version, "version", version, "print version")
	}
	root := flag.NewFlagSet("agent-archive", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	shared(root)
	if e := root.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return help(out, "")
		}
		return parseError(e, "")
	}
	rest := root.Args()
	if version {
		return (&presenter{out: out, json: jsonMode}).emit(map[string]any{"version": Version})
	}
	if len(rest) == 0 {
		return help(out, "")
	}
	command = rest[0]
	if command == "help" {
		if len(rest) > 2 {
			return usage("help accepts one command name", "Use 'agent-archive help' or 'agent-archive help sync'.")
		}
		topic := ""
		if len(rest) == 2 {
			topic = rest[1]
		}
		return help(out, topic)
	}
	if _, ok := spec(command); !ok {
		message := "unknown command"
		for _, c := range commands {
			if distance(command, c.name) <= 2 {
				message += "; did you mean '" + c.name + "'?"
				break
			}
		}
		return usage(message, "Run 'agent-archive help' to see available commands.")
	}
	hint := "Run 'agent-archive help " + command + "' for usage and examples."
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	shared(f)
	var ids stringsFlag
	var all bool
	var source, home, output, stateDir, codexHome, claudeHome string
	var incremental bool
	retries := 5
	retryDelay := 2.0
	batchRows, batchBytes, delay := defaultBatchRows, defaultBatchBytes, defaultDelaySeconds
	if command == "collect" {
		f.BoolVar(&incremental, "incremental", false, "update an existing private snapshot")
		f.StringVar(&source, "source", "", "codex or claude-code")
		f.StringVar(&home, "home", "", "override source home directory")
		f.StringVar(&output, "output", "", "new snapshot directory")
	}
	if needsSelection(command) {
		f.BoolVar(&all, "all", false, "select every session")
		f.Var(&ids, "session", "select session (repeatable)")
	}
	if command == "sync" || command == "verify" || command == "run" || command == "status" {
		f.StringVar(&stateDir, "state", "", "task state directory")
	}
	if command == "sync" || command == "verify" || command == "run" {
		f.IntVar(&retries, "retries", 5, "retries per session on temporary connection errors")
		f.Float64Var(&retryDelay, "retry-delay", 2, "seconds between retries")
	}
	if command == "run" {
		f.StringVar(&codexHome, "codex-home", "", "Codex source home")
		f.StringVar(&claudeHome, "claude-home", "", "Claude Code source home")
	}
	if command == "sync" || command == "run" {
		f.IntVar(&batchRows, "batch-rows", defaultBatchRows, "maximum events per batch")
		f.IntVar(&batchBytes, "batch-bytes", defaultBatchBytes, "maximum uncompressed bytes per batch")
		f.Float64Var(&delay, "delay", defaultDelaySeconds, "seconds after each successful batch")
	}
	if e := f.Parse(rest[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return help(out, command)
		}
		return parseError(e, command)
	}
	if f.NArg() != 0 {
		return usage("unexpected positional argument; this command uses named options", hint)
	}
	p := &presenter{out: out, progress: stderr, json: jsonMode, command: command, output: output}
	emit := p.emit
	if version || command == "version" {
		return emit(map[string]any{"version": Version})
	}
	// Validate the complete request before opening files or making network calls.
	if needsSnapshot(command) && db == "" && !(command == "status" && stateDir != "") {
		example := "agent-archive " + command + " --snapshot ./codex-backup"
		if needsCloud(command) {
			example += " --config ./connection.local.json"
		}
		example += selectionExample(command)
		return usage("--snapshot is required", "Example: "+example+"\n"+hint)
	}
	if needsCloud(command) && config == "" {
		return usage("--config is required", "Pass a private connection JSON file. See 'agent-archive help "+command+"'.")
	}
	if needsSelection(command) && (all == (len(ids) > 0)) {
		return usage("choose either --all or one or more --session ID options", hint)
	}
	if db != "" && !needsSnapshot(command) {
		return usage("--snapshot does not apply to this command", hint)
	}
	if config != "" && !needsCloud(command) {
		return usage("--config does not apply to this command", hint)
	}
	if (command == "sync" || command == "run") && (batchRows <= 0 || batchBytes <= 0 || math.IsNaN(delay) || math.IsInf(delay, 0) || delay < 0 || delay > 86400) {
		return usage("batch limits must be positive; --delay must be between 0 and 86400 seconds", hint)
	}
	if retries < 0 || retries > 100 || math.IsNaN(retryDelay) || math.IsInf(retryDelay, 0) || retryDelay < 0 || retryDelay > 86400 {
		return usage("--retries must be 0..100 and --retry-delay must be 0..86400 seconds", hint)
	}
	if command == "run" && stateDir == "" {
		return usage("--state is required", hint)
	}
	if command == "status" && stateDir != "" {
		if db != "" {
			return usage("choose --snapshot or --state", hint)
		}
		// Job status and direct sync status are local JSON records, never cloud queries.
		for _, name := range []string{"job.json", "task.json"} {
			b, e := os.ReadFile(filepath.Join(stateDir, name))
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil {
				return e
			}
			var state map[string]any
			if e = json.Unmarshal(b, &state); e != nil {
				return fault.New("task state is invalid")
			}
			if state["status"] == "running" || state["status"] == "retry_wait" {
				lockName := "lock.sqlite"
				if name == "job.json" {
					lockName = "job-lock.sqlite"
				}
				busy, e := runstate.Busy(ctx, filepath.Join(stateDir, lockName))
				if e != nil {
					return e
				}
				if !busy {
					state["status"] = "interrupted"
					state["error"] = "previous process exited; rerun the same command to resume"
				}
			}
			return emit(map[string]any{"task": state})
		}
		return fault.New("no task state found in --state directory")
	}
	if command == "run" {
		user, e := os.UserHomeDir()
		if e != nil {
			return e
		}
		if codexHome == "" {
			codexHome = os.Getenv("CODEX_HOME")
			if codexHome == "" {
				codexHome = filepath.Join(user, ".codex")
			}
		}
		if claudeHome == "" {
			claudeHome = filepath.Join(user, ".claude")
		}
		codexHome, e = filepath.Abs(codexHome)
		if e != nil {
			return e
		}
		claudeHome, e = filepath.Abs(claudeHome)
		if e != nil {
			return e
		}
		client, e := loadClient(config)
		if e != nil {
			return e
		}
		result, e := job.Run(ctx, job.Options{StateDir: stateDir, CodexHome: codexHome, ClaudeHome: claudeHome, Client: client, Sync: cloud.SyncOptions{BatchRows: batchRows, BatchBytes: batchBytes, Delay: time.Duration(delay * float64(time.Second)), Retries: retries, RetryDelay: time.Duration(retryDelay * float64(time.Second))}, Progress: emit})
		if e != nil {
			return e
		}
		return emit(map[string]any{"task": result})
	}
	if command == "collect" {
		if source != "codex" && source != "claude-code" && source != "claude_code" {
			return usage("--source must be codex or claude-code", hint)
		}
		if output == "" {
			return usage("--output is required and must name a new directory", hint)
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
		result, e := history.Collect(ctx, history.CollectOptions{Source: source, Home: home, Output: output, Incremental: incremental, Progress: emit})
		if e != nil {
			return e
		}
		return emit(result)
	}
	if command == "ping" || command == "init" {
		client, e := loadClient(config)
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
	path := db
	info, e := os.Stat(path)
	if e != nil {
		return fault.New("snapshot path cannot be read; pass an existing snapshot directory or database")
	}
	if info.IsDir() {
		path = filepath.Join(path, "cleaned.sqlite")
		if _, e = os.Stat(path); e != nil {
			return fault.New("snapshot directory has no readable cleaned.sqlite; choose a completed collection")
		}
	}
	store, e := history.Open(path)
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
	client, e := loadClient(config)
	if e != nil {
		return e
	}
	if stateDir != "" {
		snapshotDir := filepath.Dir(path)
		// Keep progress records outside immutable snapshot evidence.
		abs, e := filepath.Abs(stateDir)
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(snapshotDir, abs)
		if e != nil {
			return e
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return usage("--state must be outside the snapshot", hint)
		}
	}
	result, e := cloud.Synchronize(ctx, store, client, selected, cloud.SyncOptions{StateDir: stateDir, Destination: client.Destination(), Retries: retries, RetryDelay: time.Duration(retryDelay * float64(time.Second)), Upload: command == "sync", BatchRows: batchRows, BatchBytes: batchBytes, Delay: time.Duration(delay * float64(time.Second)), Progress: emit})
	if e != nil {
		return e
	}
	return emit(result)
}
func selectionExample(command string) string {
	if needsSelection(command) {
		return " --all"
	}
	return ""
}
func parseError(err error, command string) error {
	hint := "Run 'agent-archive help' for usage."
	if command != "" {
		hint = "Run 'agent-archive help " + command + "' for usage and examples."
	}
	text := err.Error()
	message := "invalid option or option value"
	for prefix, word := range map[string]string{"flag provided but not defined: ": "unknown option ", "flag needs an argument: ": "missing value for "} {
		if strings.HasPrefix(text, prefix) {
			name := strings.TrimLeft(strings.TrimPrefix(text, prefix), "-")
			if safeFlag(name) {
				message = word + "--" + name
			}
		}
	}
	if i := strings.Index(text, " for flag -"); i >= 0 {
		name := strings.SplitN(text[i+11:], ":", 2)[0]
		if safeFlag(name) {
			message = "invalid value for --" + name
		}
	}
	return usage(message, hint)
}
func safeFlag(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		row := make([]int, len(b)+1)
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			row[j] = min(row[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = row
	}
	return prev[len(b)]
}

func loadClient(path string) (*cloud.Client, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fault.New("connection config cannot be read; copy connection.example.json to a private file and pass --config")
	}
	return cloud.Load(path)
}
