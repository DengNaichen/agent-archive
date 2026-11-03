# Agent entry point

This repository is a standalone Go CLI for Codex / Claude Code data collection,
verification and ClickHouse synchronization. Current scope has no daemon or UI.

- Read [docs/architecture.md](docs/architecture.md) before changing package
  responsibilities, source interpretation, event identity or snapshot format.
- Read [docs/development.md](docs/development.md) before changing validation,
  dependencies, credentials, network behavior or distribution.
- Run `make check` after changes. Tests use synthetic local data and test servers.
  Live verification and uploads are separate, explicit operations.
- Keep real histories, credentials and private validation artifacts outside Git.
  Unknown source fields belong in preserved raw records, with provenance retained.
