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

## Collect private snapshots

```sh
./bin/agent-archive collect --source codex --output /path/to/new-codex-snapshot
./bin/agent-archive collect --source claude-code --output /path/to/new-claude-snapshot
```

Defaults: current `CODEX_HOME` or `~/.codex`, and `~/.claude`. Override with
`--home /path/to/source-home`. Each output must be a new directory outside the
source home. Collection preserves raw source snapshots, complete event JSON,
source positions and session/child relationships, then verifies them. Check the
final `verified:true`, `summary.json` and `verification.json`. Live-source changes
and malformed records are reported; original sources stay untouched.

Codex reads `sessions` / `archived_sessions` JSONL and JSONL.ZST. Claude reads
`projects/**/*.jsonl`, including child agents, and separately backs up the history
index. Settings/authentication files and telemetry are excluded. Embedded images,
thinking and tool input/output remain; external files referenced by a path/URL are
not fetched. Existing truncation or deleted history cannot be recovered.

## Inspect and verify locally

Global flags come **before** the command. There are no machine-specific default
snapshot/config paths:

```sh
./bin/agent-archive --local-db /path/to/snapshot/cleaned.sqlite status
./bin/agent-archive --local-db /path/to/snapshot/cleaned.sqlite plan --all
./bin/agent-archive --local-db /path/to/snapshot/cleaned.sqlite verify-local
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
./bin/agent-archive --config /private/connection.local.json ping
./bin/agent-archive --config /private/connection.local.json init
./bin/agent-archive --local-db /path/to/cleaned.sqlite --config /private/connection.local.json sync --session SESSION_ID
./bin/agent-archive --local-db /path/to/cleaned.sqlite --config /private/connection.local.json verify --session SESSION_ID
```

`ping` and `verify` are read-only. `init` creates missing tables/views without
altering/deleting existing ones. **`sync` performs real uploads** to the configured
ClickHouse service. Use `--all` for the whole snapshot or repeat `--session`.
Defaults: 500 events / 4 MiB uncompressed per gzip batch, 2 seconds after each
batch. Configure `--batch-rows`, `--batch-bytes`, `--delay`. Events above the byte
limit cause refusal, never truncation.

After a network failure/interruption, rerun the same sync. It compares server IDs
and hashes, sends missing events and verifies actual stored content hashes. It
refuses conflicting/extra remote events; no overwrite or cloud deletion is
attempted. Run one uploader at a time. The first version synchronizes point-in-time
snapshots, not a live watched directory.

Full histories and private configs should stay outside the source repository.
Logs contain counts/progress only; source JSON is intentionally sensitive.

[Architecture and data contract](docs/architecture.md) ·
[Development, validation and distribution](docs/development.md)
