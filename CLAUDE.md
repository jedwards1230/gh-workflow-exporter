# gh-workflow-exporter

@CONTRIBUTING.md

A Prometheus exporter that authenticates as a GitHub App, discovers every
scheduled GitHub Actions workflow in the installation, and exports run
conclusion, last success, expected cron interval and workflow state. It polls;
there are no webhooks.

- [`docs/CONTRACT.md`](docs/CONTRACT.md): metrics, run selection, `last_success`
  rules, config and endpoints, each clause with its enforcing test. Read it
  before changing anything a scrape, alert or probe can see.
- [`docs/TESTING.md`](docs/TESTING.md): what CI runs, what each suite covers,
  fixture conventions. Read it before adding or changing tests.
- `README.md` is the user-facing doc; keep it in step with CONTRACT.

## Gotchas

- **`event=` / `branch=` on the runs listing look like the obvious filter.**
  GitHub answers them from a lagging index that returns stale or empty results
  with a `200`. Filter client-side; a test fails on any server-side filter.
- **A `200` isn't the cheap response, a `304` is.** Conditional `If-None-Match`
  requests that return `304` are not charged against the rate limit, which is
  why every GET is ETag-revalidated. A new request path that skips the ETag
  cache still returns correct data, just at full rate-limit cost.
- **Schedules only fire on the default branch.** A `branch` override moves the
  `last_run_*` series, never a scheduled workflow's `last_success`.
- **`cancelled` is neither failure nor success.** It becomes the latest run but
  leaves `last_success` alone; a cancel streak is meant to trip the Stale alert.
  A manual or push success likewise says nothing about the schedule.
- **State is in memory.** After a restart nothing is known, so the first poll
  may need the deep (500-run) search, and `last_success` can be *absent* until
  a success is found. Absent is deliberate; never fill it with `0`. Run one
  replica.
- **YAML 1.1 turns `on:` into `true:`.** Schedule parsing accepts the `true`
  key only when no literal `on` exists; keep that preference.
- **A `.github/workflows` 404 is ambiguous.** The Actions API can list a
  workflow whose file isn't on the default branch (first registered from a
  PR). The exporter tells the cases apart by listing the repo root: root
  readable means the dir is really absent, root hidden too means the App
  lacks Contents: read (a repo error that keeps series).
- **`0` in the config means "use the default"**, not zero:
  `default_grace_factor: 0` silently becomes `1.5`. An override's
  `grace_factor: 0` is an error instead.
- **Workflow-file problems don't fail the poll.** They are counted as `parse`
  once per file version, and never fail the poll (see invariant 6).

## Invariants (violations are bugs)

1. **Strictly sequential requests.** The client holds its lock across each
   request. Never fan out.
2. **Metric names, labels and semantics are a stable API** consumed by alert
   rules outside this repo. Changes go through `docs/CONTRACT.md` first and
   need `semver:major`.
3. **One series per workflow** for `last_run_conclusion`, `last_run_url_info`
   and `state`, swapped under `metrics.mu` so a scrape never sees two.
4. **Only positive evidence deletes series.** A failed request keeps the last
   values; a successful listing showing the thing gone removes every series.
5. **Never emit a value for "didn't look far enough".** `last_success` follows
   CONTRACT §3; observations never move backwards.
6. **Only global failures block poll success.** One repo's errors go to
   `gh_exporter_repo_poll_errors_total` and never hold back
   `gh_exporter_last_poll_success_timestamp_seconds` or `/readyz`. Discovery,
   auth, token mint, rate limiting, the poll deadline, or every polled repo
   failing do (CONTRACT §5).
7. **Public repo, no secrets in output.** Never log the private key or
   installation token; examples use neutral names (`octo-org/...`) and no
   environment-specific hosts or names.

## Layout

- `internal/cronx` computes the *longest* gap of the merged fire sequence of
  all cron entries, not the average or the first cron's period.
- `internal/collector/runs.go` holds selection and the merge; `collector.go`
  holds discovery, the poll loop and when series are dropped (`dropUnseen`).

## What lives where

Anything needed to work in this repo (gotchas, invariants, contract) belongs
here or in `docs/`, reviewed with the code. Agent-local memory is for
machine-specific facts only and is never the sole copy of a rule.
