# Repository Guidelines

## Project Structure & Module Organization

This repository contains the `ship` Go CLI. The entry point is `cmd/ship/main.go`; most implementation code lives under `internal/`, organized by domain such as `cli`, `config`, `provider`, `deployment`, `docker`, `ingress`, `transport`, and `state`. Provider implementations are in `internal/provider/<name>/`. Tests are colocated with code as `*_test.go`. Shared documentation is in `docs/`, logo and README art assets are in `assets/logos/`, reusable agent skill material is in `skills/ship/`, and acceptance fixtures live in `testdata/sample-app/`.

## Build, Test, and Development Commands

- `go test ./...` runs the default CI-safe test suite.
- `go build ./cmd/ship` builds the CLI binary.
- `go install ./cmd/ship` installs the local CLI into your Go bin path.
- `SHIP_LOCAL_REGISTRY_INTEGRATION=1 go test ./internal/docker -run TestLocalRegistryIntegrationBuildPushResolvePull -count=1` runs the optional Docker registry integration test.
- `go test ./internal/ingress -run TestGeneratedCaddyfileValidatesWithCaddyBinary -count=1` validates generated Caddy config when `caddy` is installed.

Live Hetzner gates are skipped by default. See `docs/development.md` before running any command with `SHIP_LIVE_HETZNER` or `SHIP_LIVE_HETZNER_DESTRUCTIVE`.

## Coding Style & Naming Conventions

Use standard Go formatting: run `gofmt` on changed Go files and keep imports ordered by `goimports` or the Go toolchain. Package names should be short, lowercase, and domain-oriented. Exported identifiers need clear Go doc comments when they are part of a package contract. Prefer table-driven tests for command, parser, provider, and planner behavior.

## Testing Guidelines

Add or update colocated `*_test.go` files for behavior changes. Keep default tests hermetic: do not require Docker, cloud credentials, SSH hosts, or registry access unless guarded by an explicit environment variable. Use `testdata/sample-app/` for deploy workflow fixtures instead of duplicating sample applications.

## Commit & Pull Request Guidelines

Recent history uses concise imperative subjects, sometimes with Conventional Commit scopes, for example `feat(cli): ...`, `docs(brand): ...`, or `Fix ...`. Keep commits focused and describe user-visible behavior when applicable.

Pull requests should include a clear summary, linked issue when relevant, test commands run, and screenshots or terminal excerpts for CLI output changes. Call out any live infrastructure, credential, or destructive test coverage explicitly.

## Security & Configuration Tips

Default to `--dry-run` for mutating infrastructure workflows while developing. Do not commit provider credentials, registry tokens, private keys, generated certificates, or real `ship.yml` files containing secrets.
