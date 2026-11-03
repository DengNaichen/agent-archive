package app

import (
	"fmt"
	"io"
)

type commandHelp struct{ name, summary, usage, details, options, examples string }

var commands = []commandHelp{
	{"collect", "Create and verify a private snapshot of local agent history", "collect --source <codex|claude-code> --output <new-directory> [options]",
		"Copies source logs, preserves complete event JSON and source locations, then\nchecks the snapshot. Original logs stay unchanged. Does not contact ClickHouse.\nThe output directory must not exist and must be outside the source home.",
		"  --source NAME       Required. codex or claude-code\n  --output DIR        Required. New private snapshot directory\n  --home DIR          Override source home (default: CODEX_HOME or ~/.codex for\n                      Codex; ~/.claude for Claude Code)",
		"  agent-archive collect --source codex --output ./codex-backup\n  agent-archive collect --source claude-code --output ./claude-backup"},
	{"status", "Show the source, session count and size of a local snapshot", "status --snapshot <directory-or-database> [options]",
		"Reads a local snapshot. Does not check all hashes or contact ClickHouse.\nUse verify-local to check snapshot integrity.", "",
		"  agent-archive status --snapshot ./codex-backup"},
	{"plan", "Measure selected local events before uploading", "plan --snapshot <path> (--all | --session <id>...) [options]",
		"Reads local event counts and raw JSON bytes only. Does not contact ClickHouse.\nThis is not a cloud diff or a compressed storage estimate.", "",
		"  agent-archive plan --snapshot ./codex-backup --all\n  agent-archive plan --snapshot ./codex-backup --session SESSION_ID"},
	{"verify-local", "Check local source snapshots, event content and provenance", "verify-local --snapshot <directory-or-database> [options]",
		"Checks database integrity, source-file checksums and event content.\nDoes not contact ClickHouse. Large snapshots can take several minutes.", "",
		"  agent-archive verify-local --snapshot ./codex-backup"},
	{"ping", "Test a ClickHouse connection without changing data", "ping --config <connection.json> [options]",
		"Runs SELECT 1 against the configured ClickHouse service. Reads only.", "",
		"  agent-archive ping --config ./connection.local.json"},
	{"init", "Create missing ClickHouse tables and views", "init --config <connection.json> [options]",
		"Creates the agent_history database and missing tables/views in ClickHouse.\nWrites schema only; it does not upload conversations, delete data or alter\nexisting objects. Run this before the first sync to a new service.", "",
		"  agent-archive init --config ./connection.local.json"},
	{"sync", "Upload missing events and verify their stored content", "sync --snapshot <path> --config <file> (--all | --session <id>...) [options]",
		"WRITES to ClickHouse. Initialize a new service with init first.\nChecks existing IDs/hashes, uploads missing events in gzip batches, then\nverifies stored content. Conflicts cause refusal, not overwrite.\nAfter interruption, rerun the same command to resume. Run one uploader at a time.",
		"  --batch-rows N      Maximum events per batch (default: 500)\n  --batch-bytes N     Maximum uncompressed bytes per batch (default: 4194304, 4 MiB)\n  --delay SECONDS     Pause after each successful batch (default: 2)\n                      An event above the byte limit is refused whole.",
		"  agent-archive sync --snapshot ./codex-backup --config ./connection.local.json --session SESSION_ID\n  agent-archive sync --snapshot ./codex-backup --config ./connection.local.json --all --delay 2"},
	{"verify", "Compare selected local events with ClickHouse", "verify --snapshot <path> --config <file> (--all | --session <id>...) [options]",
		"Reads ClickHouse event IDs, stored hashes and hashes of actual event content.\nDoes not upload or change data. Missing, extra or conflicting events fail verification.\nUse verify-local for offline snapshot checks.", "",
		"  agent-archive verify --snapshot ./codex-backup --config ./connection.local.json --all"},
	{"version", "Print the CLI version", "version [--json]", "", "", "  agent-archive version"},
}

func spec(name string) (commandHelp, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return commandHelp{}, false
}
func help(w io.Writer, name string) error {
	if name == "" {
		_, e := fmt.Fprintln(w, `Agent Archive — collect, verify and archive Codex / Claude Code history.

Usage:
  agent-archive <command> [options]
  agent-archive help [command]

Start locally (no cloud account needed):
  agent-archive collect --source codex --output ./codex-backup
  agent-archive status --snapshot ./codex-backup
  agent-archive verify-local --snapshot ./codex-backup

Local commands:
  collect        Create and verify a private snapshot
  status         Show snapshot counts and size
  plan           Measure selected events before upload
  verify-local   Check local files, content and provenance

ClickHouse commands:
  ping           Test the connection (read-only)
  init           Create missing database objects (writes schema)
  sync           Upload missing events and verify them (writes data)
  verify         Compare a snapshot with cloud data (read-only)

Other commands:
  help           Show help for a command
  version        Print the CLI version

Shared options (before or after the command):
  --snapshot PATH   Snapshot directory or cleaned.sqlite file, where required
  --config FILE     Private ClickHouse connection JSON, where required
  --json            Emit newline-delimited JSON for scripts
  -h, --help        Show help
  --version         Print the CLI version

Cloud quick start:
  Copy connection.example.json, fill in your service details, and keep it private.
  agent-archive ping --config ./connection.local.json
  agent-archive init --config ./connection.local.json
  agent-archive plan --snapshot ./codex-backup --all
  agent-archive sync --snapshot ./codex-backup --config ./connection.local.json --all

Run 'agent-archive help <command>' for required options and examples.`)
		return e
	}
	c, ok := spec(name)
	if !ok {
		return usage("unknown command", "Run 'agent-archive help' to see available commands.")
	}
	if _, e := fmt.Fprintf(w, "%s\n\nUsage:\n  agent-archive %s\n", c.summary, c.usage); e != nil {
		return e
	}
	if c.details != "" {
		if _, e := fmt.Fprintf(w, "\n%s\n", c.details); e != nil {
			return e
		}
	}
	if name != "version" {
		if _, e := fmt.Fprintln(w, "\nOptions:"); e != nil {
			return e
		}
		if c.options != "" {
			if _, e := fmt.Fprintln(w, c.options); e != nil {
				return e
			}
		}
		if needsSnapshot(name) {
			if _, e := fmt.Fprintln(w, "  --snapshot PATH     Required. Snapshot directory or cleaned.sqlite file"); e != nil {
				return e
			}
		}
		if needsCloud(name) {
			if _, e := fmt.Fprintln(w, "  --config FILE       Required. Private ClickHouse connection JSON"); e != nil {
				return e
			}
		}
		if needsSelection(name) {
			if _, e := fmt.Fprintln(w, "  --all               Select every session in the snapshot\n  --session ID        Select a session; repeat for multiple sessions\n                      Required: either --all or --session, not both"); e != nil {
				return e
			}
		}
		if _, e := fmt.Fprintln(w, "  --json              Emit newline-delimited JSON for scripts\n  -h, --help          Show this help"); e != nil {
			return e
		}
	}
	if _, e := fmt.Fprintf(w, "\nExamples:\n%s\n", c.examples); e != nil {
		return e
	}
	if needsCloud(name) {
		_, e := fmt.Fprintln(w, "\nCredentials: copy connection.example.json to a private file (mode 600).\nAGENT_ARCHIVE_CLICKHOUSE_PASSWORD overrides the config password.\nPass a config filename, never a password, on the command line.")
		return e
	}
	return nil
}
func needsSnapshot(c string) bool {
	return c == "status" || c == "plan" || c == "verify-local" || c == "sync" || c == "verify"
}
func needsCloud(c string) bool     { return c == "ping" || c == "init" || c == "sync" || c == "verify" }
func needsSelection(c string) bool { return c == "plan" || c == "sync" || c == "verify" }
