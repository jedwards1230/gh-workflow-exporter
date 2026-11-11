# Testing

How to prove a change. Commands live in [`CONTRIBUTING.md`](../CONTRIBUTING.md);
this file says what they cover and what must stay true.

## What CI runs

Every PR and push to `main` runs `.github/workflows/ci.yml`. The job names are
required checks, so do not rename them.

| Job | Runs | Local equivalent |
|---|---|---|
| `test` | `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...` | `make test` + `go vet ./...` |
| `lint` | golangci-lint (pinned version), `gofmt -l`, `go mod tidy` diff | `make lint`, `gofmt -l .`, `go mod tidy && git diff --exit-code go.mod go.sum` |
| `docker` | multi-arch (`amd64`, `arm64`) image build, no push | `make docker` |

There is no pre-commit config. Use the pinned golangci-lint command in
CONTRIBUTING when a local run disagrees with CI.

## Suites

| Package | File | Covers |
|---|---|---|
| `internal/collector` | `collector_test.go` | Full polls against `fakeGitHub`: discovery, paging, series lifecycle, error accounting, readiness, deadlines, concurrency with Gather, secret-free logs |
| `internal/collector` | `unit_test.go` | Schedule parsing (`TestParseSchedules*`), run selection (`TestSelector`), the no-going-backwards merge (`TestObservationNoGoingBackwards`) |
| `internal/github` | `client_test.go` | Key parsing, JWT, token refresh and 401 retry, ETag cache, pagination and off-host links, error kinds, body size caps |
| `internal/cronx` | `cronx_test.go` | Interval table (`TestInterval`), day matching vs robfig in a non-UTC zone, benchmark for `* * * * *` |
| `internal/config` | `config_test.go` | Parsing, defaults, validation errors, credential sources |
| root | `main_test.go` | Flag/env precedence, missing-config handling, endpoint status codes |
| `internal/version` | `version_test.go` | Build stamping |

Which test enforces which clause of the contract is listed in
[`CONTRACT.md`](CONTRACT.md).

## Fixtures

- No test touches the real GitHub API. `internal/collector` uses `fakeGitHub`
  (an `http.Handler` behind `httptest`), which serves content-derived ETags,
  honours `If-None-Match`, paginates, and records every request URL, 304
  count, file fetch and runs page fetch. `internal/github` uses a smaller
  `fakeAPI` plus `fakeClock` for token expiry.
- Build a poll with `newHarness(t, gh, cfgYAML)`, then read results with
  `h.series(t)`, `assertSeries`, `assertAbsent` and `h.pollStats(t)`.
- Start from `standardFixture` (multi-repo, excluded and archived cases) or
  `pagingFixture` (one scheduled workflow, a success at a chosen index). Runs
  are built with `ghRun`; workflow files are the YAML constants
  (`dailyYAML`, `badYAML`, …).
- Times are relative to a truncated `time.Now()`; no test depends on the wall
  clock beyond that.
- Names are neutral (`octo-org/...`), and the fake's token is
  `fakeToken`, which `TestLogsNeverContainSecrets` greps the logs for.

## Must stay true

- Any test that lists runs goes through `assertNoServerSideFilters` or keeps
  it covered: a server-side `event`/`branch` filter must fail the suite.
- Tests run under `-race`. A change touching `metrics.mu` or the poll loop
  must keep `TestConcurrentGatherDuringPoll` meaningful, not just green.
- A behaviour change updates the citing clause in `CONTRACT.md` and adds or
  changes the named test in the same PR.

## Avoid

- Hitting `api.github.com` or reading real credentials in tests.
- Sleeping to wait for a poll; call `h.c.Poll(ctx)` directly.
- Fixtures that hold the already-selected result (for example a lone success
  run) when the test is about selection; seed the mixed run history instead.
