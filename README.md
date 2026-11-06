# Agent Archive

Standalone Go CLI to collect, verify and synchronize Codex / Claude Code history.
It preserves original records and source evidence. No Python installation, external
`zstd`, background service or UI is required.

## Build

Use the Go version declared by `go.mod`:

```sh
make check
./bin/agent-archive --help
# Optional local installation:
make install
```

## Help and output

Run `agent-archive help` or `agent-archive help sync`. `-h` / `--help` also work on
every command. With no arguments, the CLI shows help and exits successfully.

The default interface is human-readable: results go to stdout and progress/errors
to stderr. Add `--json` for scripts; successful results and progress then use
newline-delimited JSON on stdout, while errors are JSON on stderr. This replaces
the initial JSON-only default; scripts should now explicitly pass `--json`.

```sh
./bin/agent-archive status --snapshot ./codex-backup --json
```

Exit codes: 0 success/help, 1 operation failure, 2 invalid usage, 130 interrupted.
Help commands never read snapshots or connect to ClickHouse.

## Collect private snapshots

```sh
./bin/agent-archive collect --source codex --output /path/to/new-codex-snapshot
./bin/agent-archive collect --source claude-code --output /path/to/new-claude-snapshot
```

Defaults: current `CODEX_HOME` or `~/.codex`, and `~/.claude`. Override with
`--home /path/to/source-home`. Each output must be a new directory outside the
source home. Collection preserves raw source snapshots, complete event JSON,
source positions and session/child relationships, then verifies them. Check the
final verification message, `summary.json` and `verification.json`. Live-source changes
and malformed records are reported; original sources stay untouched.

Codex reads `sessions` / `archived_sessions` JSONL and JSONL.ZST. Claude reads
`projects/**/*.jsonl`, including child agents, and separately backs up the history
index. Settings/authentication files and telemetry are excluded. Embedded images,
thinking and tool input/output remain; external files referenced by a path/URL are
not fetched. Existing truncation or deleted history cannot be recovered.

## Daily incremental collection and resumable jobs

```sh
./bin/agent-archive run --state /private/agent-archive-state --config /private/connection.local.json \
  --batch-rows 1000 --batch-bytes 33554432 --delay 0.5
./bin/agent-archive status --state /private/agent-archive-state
```

`run` performs one six-stage cycle: collect Codex, collect Claude Code, sync each,
then independently verify each. Schedule this command with your existing scheduler;
it is not a daemon. Source-home overrides are `--codex-home` and `--claude-home`.
Keep the state directory outside both source homes and outside Git. It holds the
reusable snapshots, `job.json`, and per-cycle cloud checkpoints. Credentials stay
in the supplied config, not in task state.

Rerun the same command after interruption. A pending cycle finishes against its
captured data before the next invocation collects newer data. A completed cycle's
next invocation starts a new collection and fresh independent cloud verification.
SQLite OS locks prevent concurrent runs and release on process death. `status`
reads local persisted state without connecting to the cloud. It checks lock
ownership and reports an abandoned running task as interrupted. The stored JSON
alone records only the last persisted phase.

For a single source, incremental collection also works independently:

```sh
./bin/agent-archive collect --source codex --output /private/codex-store --incremental
```

This creates a snapshot initially, then fingerprints source files on every run.
Unchanged files are not copied or parsed. Changed/new files are captured and
reparsed whole (including compressed logs), preserving parser context. This is
file-level incremental collection, **not byte-offset tailing**. Verification still
checks the full local evidence. Deleted/truncated/rewritten source files never
remove earlier evidence. Original versions are retained, so disk usage grows with
changed files; there is no automatic retention/deletion policy.

## Inspect and verify locally

Use `--snapshot` with a snapshot directory or a `cleaned.sqlite` file. Shared
options work before or after the command; old `--local-db` / `--db` aliases remain
accepted. There are no machine-specific default snapshot/config paths:

```sh
./bin/agent-archive status --snapshot /path/to/snapshot
./bin/agent-archive plan --snapshot /path/to/snapshot --all
./bin/agent-archive verify-local --snapshot /path/to/snapshot
```

`status`, `plan`, `collect` and `verify-local` never connect to the cloud. Plan bytes
measure event JSON only, excluding provenance, upload fields and compression.
The reader supports both the earlier Python snapshots and new versioned Go ones.
Claude inferred turns/timestamps retain explicit limitations; they are not native
Codex turn semantics. See [architecture](docs/architecture.md).

## ClickHouse

Copy `connection.example.json` to a private location, set host/credentials, and
restrict it to mode 600. You may keep `password` empty and set
`AGENT_ARCHIVE_CLICKHOUSE_PASSWORD` in the environment. No credentials are bundled
in this project, passed as CLI arguments, or printed in errors.

```sh
./bin/agent-archive ping --config /private/connection.local.json
./bin/agent-archive init --config /private/connection.local.json
./bin/agent-archive sync --snapshot /path/to/snapshot --config /private/connection.local.json --session SESSION_ID
./bin/agent-archive verify --snapshot /path/to/snapshot --config /private/connection.local.json --session SESSION_ID
```

`ping` and `verify` are read-only. `init` creates missing tables/views without
altering/deleting existing ones. **`sync` performs real uploads** to the configured
ClickHouse service. Use `--all` for the whole snapshot or repeat `--session`.
Defaults: 500 events / 4 MiB uncompressed per gzip batch, 2 seconds after each
batch. Configure `--batch-rows`, `--batch-bytes`, `--delay`. Events above the byte
limit cause refusal, never truncation.

Temporary transport errors and HTTP 408/429/5xx retry at session level (default:
5 retries, 2 seconds; configure `--retries`, `--retry-delay`). Every retry first
checks the server, including after a lost INSERT response. Authentication errors,
content conflicts, local corruption and oversized records are not retried.

Add `--state /private/sync-task` to `sync` or `verify` to persist verified session
checkpoints and inspect them with `status --state`. Checkpoints are bound to the
snapshot database digest, destination (excluding password), selection and mode.
Changed evidence or destination resets them. Only sessions whose complete actual
content hashes passed verification can be skipped on resume. A completed direct
command with the same state reuses its checkpoints; use a **new state directory**
for a fresh verification pass. `run` does that automatically each cycle. Without
`--state`, each invocation checks all selected sessions again.

After a network failure/interruption, rerun the same sync. It compares server IDs
and hashes, sends missing events and verifies actual stored content hashes. It
refuses conflicting/extra remote events; no overwrite or cloud deletion is
attempted. Run one uploader at a time. Each cycle synchronizes captured evidence; it does not watch a live directory.

Full histories and private configs should stay outside the source repository.
Logs contain counts/progress only; source JSON is intentionally sensitive.

[Architecture and data contract](docs/architecture.md) ·
[Development, validation and distribution](docs/development.md)
