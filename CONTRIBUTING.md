# Contributing to gh-workflow-exporter

gh-workflow-exporter is a small Prometheus exporter for GitHub Actions
workflow health, written in Go.

## Prerequisites

- [Go](https://go.dev/) (version from `go.mod`)
- [golangci-lint](https://golangci-lint.run/) v2
- Docker with buildx (only for `make docker`)

## Build, test & lint

```bash
make build    # ./gh-workflow-exporter, version-stamped via -ldflags
make test     # go test -race -count=1 ./...
make lint     # golangci-lint run ./...
make docker   # local image build
go vet ./...
gofmt -l -w .
```

CI pins golangci-lint (see `.github/workflows/ci.yml`). A newer local binary
can report findings the pinned one does not; to match CI exactly:

```bash
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run ./...
```

Tests never touch the real GitHub API: `internal/collector` drives full polls
against an in-memory fake (`httptest`) that honours ETags and pagination.

## Running locally

Credentials can come from the environment, so a private key never has to be
written to disk:

```bash
GHWE_APP_ID=123456 GHWE_INSTALLATION_ID=7890123 \
GHWE_PRIVATE_KEY="$(your-secret-manager read app-key)" \
go run . --config config.example.yaml --log-level debug
curl -s localhost:9877/metrics | grep -E '^(github_|gh_exporter_)'
```

Each poll logs a `poll complete` line with `requests` and `not_modified`
counts; the second poll should be almost entirely 304s.

## Documentation

Keep documentation current as part of the change: update `README.md` (config,
flags, metrics) in the same PR as the code.

## Branching & commits

- Branch off `main`; never commit directly to `main`.
- Use [Conventional Commits](https://www.conventionalcommits.org/) prefixes
  (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, …).
- Sign your commits where possible (`git commit -S`).
- Keep each PR focused; delete dead code rather than commenting it out.

## Pull requests

- Open the PR against `main`. CI (`test`, `lint`, `docker`) must be green and
  every review thread resolved before merge.
- **A PR carries at most one `semver:*` label**; see [Releases](#releases).

## Releases

Merges to `main` are released automatically. A PR may declare what it does to
the version with one label:

| Label | Effect on merge |
|---|---|
| `semver:major` | Cut `vX.0.0` |
| `semver:minor` | Cut `v0.Y.0` |
| `semver:patch` | Cut `v0.0.Z` |
| `semver:none` | No release |
| *(no label)* | Decided by the diff: no release if every changed file is dev-only, otherwise patch |

Each release publishes a multi-arch (`linux/amd64`, `linux/arm64`) image to
`ghcr.io/jedwards1230/gh-workflow-exporter`, tagged `vX.Y.Z` and `X.Y.Z`.
