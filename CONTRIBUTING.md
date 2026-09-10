# Contributing to LAC

Thanks for your interest. LAC is a small project with a deliberately narrow scope, so the most valuable contribution is
usually a focused change with a test.

## Ground rules

- **Keep the scope narrow.** LAC coordinates local agents. It is not a job scheduler, a CI system or a chat platform.
- **Dependencies point inward.** `internal/core` knows nothing about storage or transports; `internal/service` holds the
  business rules; transports and the SQLite store depend on those, never the other way around.
- **No cgo.** The SQLite driver is pure Go so that `go install` works everywhere without a toolchain.
- **Security is a feature.** Anything that widens the attack surface — a network listener, an exec path, a new
  credential — needs a note in `SECURITY.md` and a reviewer's explicit sign-off.

## Getting set up

Requires Go 1.25 or newer.

```sh
git clone https://github.com/sadeq-n-yazdi/lac.git
cd lac
make deps      # install the developer tooling
make build
make test
```

Install the git hooks once:

```sh
pre-commit install
```

## Before you open a pull request

```sh
make fmt lint test
```

All of these must pass:

- `gofmt` and `goimports` produce no diff
- `go vet` and `golangci-lint` are clean (`gosec` runs as part of `golangci-lint`)
- `go test -race ./...` passes
- `make vuln` (`govulncheck ./...`) reports nothing new

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `refactor:`, `test:`,
`chore:`, `build:`, `ci:`. Reference the issue in the body or footer, for example `Refs #12` or `Closes #12`.

Write the subject in the imperative mood and keep it under 72 characters.

## Code style

- Line length is soft-capped at 120 characters.
- Use complete words for identifiers. `header`, not `hdr`; `maximum` is fine as `max` only where the standard library
  does the same. Single letters are acceptable only as loop indices in short loops.
- Return errors, do not log-and-continue. Wrap with `fmt.Errorf("doing thing: %w", err)` so the cause survives.
- Exported identifiers get doc comments that start with the identifier name.
- Table-driven tests, named subtests, and no sleeps in tests — synchronise on channels or contexts.

## Reporting bugs and requesting features

Use the issue templates. A bug report without a reproduction is very hard to act on; please include the LAC version,
your OS, and the relevant log lines with any tokens redacted.

## Security issues

Do not open a public issue. Follow [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contribution is licensed under the MIT License, the same as the rest of the project.
