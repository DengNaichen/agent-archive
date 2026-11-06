package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/DengNaichen/agent-archive/internal/cloud"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"github.com/DengNaichen/agent-archive/internal/history"
)

type usageError struct{ message, hint string }

func (e *usageError) Error() string    { return e.message }
func usage(message, hint string) error { return &usageError{message, hint} }

type commandError struct {
	err     error
	json    bool
	command string
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

// WriteError is the single process-boundary error formatter. Usage errors are
// safe by construction; arbitrary parser/SQL/HTTP details still pass through fault.
func WriteError(w io.Writer, err error) int {
	code := 1
	message := fault.Message(err)
	hint := ""
	jsonMode := false
	var command *commandError
	if errors.As(err, &command) {
		jsonMode = command.json
		hint = "Run 'agent-archive help " + command.command + "' for usage."
	}
	var invalid *usageError
	if errors.As(err, &invalid) {
		code = 2
		message = invalid.message
		hint = invalid.hint
	}
	if errors.Is(err, context.Canceled) {
		code = 130
		message = "interrupted"
		hint = "Completed uploads remain stored. Rerun the same sync command to resume."
		if command != nil && command.command == "collect" {
			hint = "Partial output is retained. With --incremental, rerun the same command; otherwise collect into a new directory."
		}
	}
	if jsonMode {
		v := map[string]any{"success": false, "error": message, "exit_code": code}
		if hint != "" {
			v["hint"] = hint
		}
		if e := json.NewEncoder(w).Encode(v); e != nil {
			return 1
		}
	} else {
		if _, e := fmt.Fprintf(w, "Error: %s\n", message); e != nil {
			return 1
		}
		if hint != "" {
			if _, e := fmt.Fprintf(w, "\n%s\n", hint); e != nil {
				return 1
			}
		}
	}
	return code
}

type presenter struct {
	out, progress   io.Writer
	json            bool
	command, output string
}

func (p *presenter) emit(v any) error {
	if p.json {
		return json.NewEncoder(p.out).Encode(v)
	}
	switch r := v.(type) {
	case history.CollectReport:
		_, e := fmt.Fprintf(p.out, "Snapshot collected and verified.\n\n  Source:       %s\n  Files:        %s\n  Skipped files: %s\n  Occurrences:  %s\n  Source issues: %s\n  Changed files: %s\n  Snapshot:     %s\n\nNext: agent-archive status --snapshot %s\n", sourceName(r.Source), count(r.Files), count(r.Skipped), count(r.Occurrences), count(r.Issues), count(r.Changed), strconv.Quote(p.output), shellQuote(p.output))
		return e
	case history.Stats:
		_, e := fmt.Fprintf(p.out, "Local snapshot\n\n  Source:    %s\n  Sessions:  %s\n  Events:    %s\n  Turns:     %s\n  Database:  %s\n", sourceName(r.Source), count(r.Sessions), count(r.Events), count(r.Turns), size(r.SnapshotBytes))
		return e
	case history.Plan:
		_, e := fmt.Fprintf(p.out, "Local upload plan (no cloud connection)\n\n  Source:      %s\n  Sessions:    %s\n  Events:      %s\n  Raw JSON:    %s\n\nExcludes upload fields, provenance, compression and cloud storage overhead.\n", sourceName(r.Source), count(r.Sessions), count(r.Events), size(r.Bytes))
		return e
	case history.Verification:
		_, e := fmt.Fprintf(p.out, "Local verification passed.\n\n  Source:         %s\n  Verified files: %s\n  Verified events: %s\n  Changed files:  %s\n", sourceName(r.Source), count(r.Files), count(r.Events), count(r.Changed))
		if e != nil {
			return e
		}
		keys := []string{}
		for k := range r.Issues {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			_, e = fmt.Fprintln(p.out, "  Source issues:  none")
			return e
		}
		if _, e = fmt.Fprintln(p.out, "\nSource issues retained in original snapshots:"); e != nil {
			return e
		}
		for _, k := range keys {
			if _, e = fmt.Fprintf(p.out, "  %s: %s\n", k, count(r.Issues[k])); e != nil {
				return e
			}
		}
		return nil
	case cloud.SyncResult:
		title := "Cloud verification passed."
		if p.command == "sync" {
			title = "Sync completed and cloud content verified."
		}
		_, e := fmt.Fprintf(p.out, "%s\n\n  Source:          %s\n  Sessions:        %s\n  Verified events: %s\n  Uploaded now:    %s\n  Already present: %s\n", title, sourceName(r.Source), count(r.Sessions), count(r.Verified), count(r.Inserted), count(r.AlreadyPresent))
		return e
	case map[string]any:
		if task, ok := r["task"]; ok {
			return p.task(task)
		}
		switch r["event"] {
		case "job_stage":
			_, e := fmt.Fprintf(p.progress, "Stage %v/%v: %v\n", r["stage"], r["total_stages"], r["phase"])
			return e
		case "retry_wait":
			_, e := fmt.Fprintf(p.progress, "Temporary connection error; retrying after %v seconds (attempt %v).\n", r["retry_in_seconds"], r["attempt"])
			return e
		case "session_resumed":
			_, e := fmt.Fprintf(p.progress, "Resumed verified sessions: %v/%v\n", r["completed_sessions"], r["total_sessions"])
			return e
		case "collection_progress":
			_, e := fmt.Fprintf(p.progress, "Collecting files: %v/%v (%v occurrences)\n", r["files"], r["total_files"], r["occurrences"])
			return e
		case "verify_sources":
			_, e := fmt.Fprintf(p.progress, "Checking source files: %v/%v\n", r["files"], r["total_files"])
			return e
		case "verify_content":
			_, e := fmt.Fprintf(p.progress, "Checking event content: %v events\n", r["events"])
			return e
		case "batch_uploaded":
			_, e := fmt.Fprintf(p.progress, "Uploaded batch: %v events\n", r["rows"])
			return e
		case "session_verified":
			_, e := fmt.Fprintf(p.progress, "Verified sessions: %v/%v (%v events)\n", r["completed_sessions"], r["total_sessions"], r["events"])
			return e
		}
		if version, ok := r["version"]; ok {
			_, e := fmt.Fprintf(p.out, "Agent Archive %v\n", version)
			return e
		}
		if r["command"] == "ping" {
			_, e := fmt.Fprintf(p.out, "ClickHouse connection OK (%.2f s).\n", r["elapsed_seconds"])
			return e
		}
		if r["command"] == "init" {
			_, e := fmt.Fprintln(p.out, "ClickHouse schema initialized. Existing objects were kept.")
			return e
		}
	}
	return fmt.Errorf("unsupported presentation type")
}
func sourceName(s string) string {
	if s == "codex" {
		return "Codex"
	}
	if s == "claude_code" {
		return "Claude Code"
	}
	return s
}
func count(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
func size(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for _, u := range units {
		v /= 1024
		if v < 1024 || u == "TiB" {
			return fmt.Sprintf("%.2f %s (%d bytes)", v, u, n)
		}
	}
	return ""
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (p *presenter) task(value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	var v struct {
		Status    string                      `json:"status"`
		Phase     string                      `json:"phase"`
		Session   string                      `json:"current_session"`
		Updated   string                      `json:"updated_at"`
		Error     string                      `json:"error"`
		Attempt   int                         `json:"attempt"`
		Completed map[string]cloud.SyncResult `json:"completed"`
		Results   map[string]cloud.SyncResult `json:"results"`
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return e
	}
	if _, e = fmt.Fprintf(p.out, "Task status: %s\n  Updated: %s\n", v.Status, v.Updated); e != nil {
		return e
	}
	if v.Phase != "" {
		if _, e = fmt.Fprintf(p.out, "  Phase: %s\n", v.Phase); e != nil {
			return e
		}
	}
	if v.Session != "" {
		if _, e = fmt.Fprintf(p.out, "  Current session: %s\n  Attempt: %d\n  Verified sessions: %d\n", v.Session, v.Attempt, len(v.Completed)); e != nil {
			return e
		}
	}
	keys := []string{}
	for k := range v.Results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := v.Results[k]
		if _, e = fmt.Fprintf(p.out, "  %s: %s sessions, %s verified events, %s acknowledged uploads\n", k, count(r.Sessions), count(r.Verified), count(r.Inserted)); e != nil {
			return e
		}
	}
	if v.Error != "" {
		_, e = fmt.Fprintf(p.out, "  Error: %s\n", v.Error)
	}
	return e
}
