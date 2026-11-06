# Architecture and data contract

## Purpose and module ownership

The CLI turns current on-disk coding-agent logs into private, verifiable snapshots
and optionally synchronizes selected session streams to the user's ClickHouse.
It preserves evidence rather than computing work-habit metrics.

`cmd/agent-archive` owns process signals, exit status and the single error-reporting
boundary. `internal/app` owns command arguments, selection and presentation. Its help and
output modules keep the human interface separate from command execution; `--json`
preserves a machine-readable NDJSON stream. Shared flags work on either side of
the command, and snapshot directories hide the internal database filename.
Neither interprets transcript fields or implements SQL/HTTP protocols.

`internal/history` is a deep module with `Collect`, `Open`, `Store.Walk` and
`Store.Verify` as its principal interface. It owns inventory, frozen source copies,
source-specific interpretation, SQLite persistence, identity and provenance.
`codex.go` and `claude.go` keep format decisions local. Store implementation and
source readers are concrete private code; there is no generic plugin framework.
The legacy adapter reads existing Python snapshots without changing them.

`internal/cloud` owns HTTPS authentication, additive schema initialization,
resumable batches, conflict detection and server-computed hash verification.
Its `Transport` interface is the actual test seam: production HTTP and a
fault-injecting test transport. No general repository, service or dependency
injection framework is needed.

`internal/job` owns a resumable six-stage collect/sync/verify cycle. It composes
history and cloud through their existing interfaces; it does not parse logs or
implement transport. `internal/runstate` owns atomic private JSON records and
SQLite OS locks shared by collectors and cloud tasks. Locks are released on
process death; no stale PID file must be manually deleted.

`internal/fault` marks messages explicitly safe to show. Internal wrapped parser,
filesystem and SQL errors stay private. Progress contains metadata/counts only.

## Vocabulary

- Source file: a copied JSONL or compressed JSONL byte stream.
- Source occurrence: one file/line position, including repeated event content.
- Event: one logical source record, with the entire JSON document preserved.
- Session stream: a main thread or a distinct child-agent transcript.
- Turn: native Codex turn identity, or Claude parent-chain inference. These are
  different semantics and should not be silently compared as equal measures.
- Snapshot: the database, exact source copies and verification reports together.

## Collection and failure semantics

`collect` inventories regular files, excluding symlinks. It requires a new output
directory outside the source home, including resolved symlink ancestors. Source
files are never modified. Files are copied up to the size observed before copy;
changes/truncation are flagged. This is a per-file capture, not an atomic backup
of a running multi-file application.

Output directories are 0700; database, source copies and reports are 0600.
Codex copies retain original compression. Claude copies are gzip-compressed;
its prompt history index is backed up separately and is not counted as events.
Each transcript file is imported in one SQLite transaction. Failure retains a
partial output for investigation, with no successful verification report.
Recollection normally uses a new directory; `--incremental` resumes a reusable Go store. A completion marker blocks cloud operations
on interrupted Go collections; `status` remains available for diagnosis, while
`verify-local` reports the incomplete marker as a failure.

Malformed/unsupported records stay in source bytes and produce `issues`. Unknown
fields inside supported JSON envelopes are preserved in raw records. Records over
64 MiB or decompression errors stop collection explicitly. No authentication
settings, telemetry, file-history backup bytes or external attachment downloads
are included. Embedded images/thinking/tool content remain in raw JSON.

## Incremental evidence stores and jobs

`collect --incremental` uses a new snapshot initially. Later invocations bind it
to the same resolved source home and source type. Content fingerprints, rather
than mtimes, decide whether files changed. Unchanged files are read for hashing
but not copied/parsed. Changed/new files are captured in unique directories and
replayed whole, so Codex context and Claude parent-chain inference remain local
to their adapters; no byte-offset cursor can lose parser context or partial lines.

The optional v1 `incremental_version=1` extension adds `archive_versions`, keyed
by original path and content hash. `source_files` tracks the newest capture;
all earlier original bytes remain verified through `archive_versions`. Event
identity stays unchanged and events are cumulative: truncation, rewriting,
rotation or deletion at the source never deletes prior events. Every distinct
file version retains its provenance. Reverting to an already imported version
does not duplicate source occurrences. Cloud uploads still only insert missing
events; extra historical source locations for an already uploaded event are not
backfilled into that row. Cloud verification covers event IDs and raw content.

Each file import and manifest update commits together. The completion marker is
cleared before updates, and restored only after full evidence verification.
Cancellation/failure leaves a resumable incomplete store. Resuming skips committed
unchanged files, imports remaining changes, then verifies all retained versions
and events. Full local verification is intentionally retained. Archives from an
aborted capture may remain unreferenced; there is no automatic garbage collection.
The database, archives and task JSON must remain private and outside source homes.
The snapshot lock serializes collectors and synchronizers over the same store.

`run --state DIR` persists a cycle, next stage, safe error and result counts in
`job.json`. Source homes and destination bind the job. Interrupted cycles resume
without recollecting newer data halfway through. A completed cycle's next run
starts incremental collection and a new independent verification scope. `status --state` probes the existing local task lock and reports abandoned
running state as interrupted. Raw JSON alone records the last persisted phase.

## Identity and compatibility

Codex identity follows the Python prototype: canonical SHA256(payload), then
canonical SHA256([session_id, event_type, original_timestamp, payload_hash]).
Canonical JSON sorts object keys, preserves arbitrary-size integer tokens and
matches Python numeric/string serialization. Stored document order follows source
key order and Python-compatible compact serialization, so tested existing raw
content hashes stay compatible. Exact original bytes remain in source snapshots.
Storage-only ordinal differences can share an event, while every occurrence
retains its original source position.

Claude identity is SHA256("claude_code" + NUL + stream_id + NUL + SHA256(raw JSON)).
The original JSON text is retained without its line ending. Identical records in
a stream deduplicate; provenance keeps all occurrences. Distinct versions with
the same native UUID remain distinct events. Child stream IDs are
`PARENT_SESSION_ID:agent-NAME`, with original parent IDs in provenance/raw JSON.

Claude turns walk `parentUuid` to a non-meta user message without tool-result
blocks. Cycles/unresolved links leave the turn empty. Missing timestamps are
marked in provenance as file-first inference or an unknown 1970 sentinel. Those
times are storage placeholders, not evidence of user activity. Dedicated Claude
cloud session views use original record timestamps only.

## Snapshot format v1

`metadata` holds `source`, `schema_version=1` and collection completion. Future
versions must be rejected until a deliberate reader/migration is added.
`sessions` and `turns` hold interpreted relationships; `events` holds all upload
fields plus full `record_json` and SHA256. `provenance` stores every location and
source hash; `source_files` registers original-byte hashes, sizes, compression
and snapshot paths. `issues` reports source limitations. `cloud_events` assembles
upload fields and source locations; `event_documents` exposes full documents.

The Go reader also supports the original Python Codex and Claude SQLite layouts.
Legacy provenance is loaded once per selected session because the Codex prototype
has no event-ID provenance index. Legacy files stay read-only.

## Cloud synchronization

Users choose `--all` or repeatable `--session`. Source is read from the snapshot;
IDs never silently cross Codex/Claude namespaces. The fixed destination is
`agent_history.raw_events`, with dedicated per-source parsing views. `init` embeds
additive SQL; source interpretation does not depend on remote views.

Before inserting a session, check schema, remote IDs and actual content hashes,
local hashes/provenance and batch size. Conflicting/extra remote IDs cause refusal.
Each gzip batch is bounded by row count and uncompressed bytes; oversized events
are refused whole. After upload, verify all selected event IDs, stored hashes and
server-computed SHA256(record_json), not just counts or acknowledged inserts.

A timeout after server commit is ambiguous. Retry the whole session only for
classified temporary transport / 408 / 429 / 5xx errors, with a bounded cancellable
delay, and read actual remote hashes again before sending missing IDs. Other
failures return immediately. There are no blind INSERT retries.

Optional `--state` checkpoints record only fully verified sessions, bound to the
whole snapshot database SHA256, destination identity (host/port/database/user,
never password), ordered selection and upload/verify mode. Atomic JSON replace
occurs under an OS lock. Unknown/corrupt state is refused; changed scope resets
checkpoints. Resume trusts verified sessions from that same immutable scope, so
use fresh state for an independent recheck. `run` uses a new verification scope
each cycle. Process crashes after commit but before checkpoint cause a safe
server recheck on resume. ReplacingMergeTree and
FINAL provide logical deduplication, not uniqueness constraints. Run one uploader
at a time. Errors may leave earlier batches/sessions uploaded; nothing is deleted.

## Extending the tool

A format change belongs in its collector plus synthetic regression tests.
A new source requires identity/provenance rules and an explicit compatibility
contract before it is offered by the CLI. A new destination earns an interface
only when there are two real adapters. Existing snapshot identities/hashes must
remain stable or receive an explicit migration; never silently re-key cloud data.
