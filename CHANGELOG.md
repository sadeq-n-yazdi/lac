# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `internal/auth`: agent tokens stored as keyed hashes, and capability checks that deny by default.
- `internal/service/registry`, `internal/service/messaging`, `internal/service/leasing`: the agent roster,
  durable messaging, and the fair, capacity-bounded resource queue.
- `internal/api`: the JSON-RPC method surface, with every call but registration behind a token.
- `pkg/lacclient`: the public Go client for the daemon.
- The `lac` command-line client, including `lac run --resource <name> -- <command>`.
- `internal/core`: the domain vocabulary and the storage contracts, with no I/O.
- `internal/config`: XDG path resolution and layered configuration with strict validation.
- `internal/store/sqlite`: a pure-Go SQLite store with embedded, versioned migrations.
- `internal/transport/unixsock`: a private Unix socket listener with kernel peer verification.
- `internal/transport/jsonrpc`: the JSON-RPC 2.0 server, with concurrent calls and graceful shutdown.
- `internal/daemon` and a working `lacd` serving `daemon.info` and `daemon.ping`.
- Project foundation: README, MIT license, changelog, TODO roadmap, contributing guide, code of conduct and security
  policy.
- GitHub issue and pull request templates, CI workflow and Dependabot configuration.
- Go module `code.sadeq.uk/lac`, Makefile, linter and pre-commit configuration.

[Unreleased]: https://github.com/sadeq-n-yazdi/lac/commits/main
