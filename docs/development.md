# Development and validation

`go.mod` / `go.sum` own Go and dependency versions. SQLite uses modernc's CGo-free
driver; Zstandard uses klauspost/compress. Both ship in the executable. Deliberate
module changes should run `go mod tidy` and review the resulting dependency diff.

Run `make check`: format check, `go vet`, synthetic tests and a CGO-disabled build.
Run `make fmt` to apply formatting. Unit/integration tests use temporary directories
and local TLS test servers; they never read real agent homes or contact ClickHouse.
They exercise the same Collect/Store/Synchronize interfaces used by the CLI.

Source changes need tests covering raw content, source locations, duplicate and
conflicting versions, child streams and malformed inputs. Upload changes need
fault tests for ambiguous commits, idempotent reruns, corrupt/extra remote data,
read-only verification and refusal before insert. Error behavior must preserve
cancellation, expose actionable safe validation errors and hide raw bodies/secrets.
Avoid per-event queries against legacy provenance: it lacks an event-ID index.

Live checks are opt-in CLI operations using a private `--config`. `ping` and
`verify` are read-only. `init` creates missing schema objects. `sync` uploads selected
data. A test suite must never start a live upload implicitly. For parity checks,
keep real Python snapshots and regenerated Go snapshots outside this repository;
compare logical IDs, every upload field and provenance references. Private example
scripts/receipts are not release fixtures.

Build locally with `make build`. Example cross-build, without executing the target:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/agent-archive-linux-amd64 ./cmd/agent-archive
```

A successful cross-build is not evidence that the target ran correctly. This
repository publishes source only; binary releases and package-manager distribution
are separate future work. A binary release requires choosing a license,
target-machine smoke tests, artifact checksums and platform signing where required.
No Release, Homebrew tap, signing identity or publishing credential is bundled.
The module path is `github.com/DengNaichen/agent-archive`.

Architecture documentation references InfHarness's Go profile and shared
foundation: actual package owners, repository-owned validation commands, private
credentials, preserved existing choices and separation of tests from live side
effects. The project does not claim a Harness bootstrap or score.

Incremental/task changes must cover unchanged-file skipping, append/rewrite/revert,
compressed streams, deleted sources, interrupted imports, retained-archive
corruption, changed source-home rejection, and exclusive lock release. Cloud
fault tests cover classified retries after ambiguous commits, bounded retry
budgets, cancellation during backoff, persisted resume and fresh independent
verification against corrupted server content. Job tests run two sources through
failure/restart and multiple cycles using the real collector/store/synchronizer
and a synthetic transport; all real network checks remain explicit and private.
