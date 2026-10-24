# gh-workflow-exporter

@CONTRIBUTING.md

A Prometheus exporter that authenticates as a GitHub App, discovers every
scheduled GitHub Actions workflow in the installation, and exports run
conclusion, last success, expected cron interval and workflow state.
`README.md` is the user-facing contract (config, flags, metrics, alerts);
metric names and labels are consumed by alert rules elsewhere, so treat them
as a stable API.

## Architecture

| Path | Role |
|---|---|
| `main.go` | Flags/env (flags > env > defaults), credential + config loading, HTTP server (`/metrics`, `/healthz`, `/readyz`), signal handling |
| `internal/config` | YAML config (include/exclude/overrides, strict keys, validation) and App credentials from env or the secret dir |
| `internal/github` | Sequential REST client: App JWT → installation token (cached, re-minted <10 min before expiry, once on 401), ETag cache with per-poll eviction, Link pagination, error kinds, request stats |
| `internal/cronx` | Expected interval = LONGEST gap in the merged fire sequence of all cron entries, walked day by day over 9 years (robfig/cron parsing, 5-field, UTC, DOW 7 = Sunday, floor 5m) |
| `internal/collector` | The poll: discovery, workflow-file SHA cache + `on.schedule` parsing, run selection (client-side filtering; shallow ≤5×10 runs, then a deep ≤5×100 search only when no success is known), no-going-backwards merge, metric emission and series deletion |
| `internal/version` | Build stamping via `-ldflags` |

## Invariants

- Requests are strictly sequential (the client holds its lock across each
  request). Never fan out.
- Never pass `event=` or `branch=` to the runs listing; GitHub serves those
  from a lagging index. A test asserts this.
- Exactly one `github_workflow_last_run_conclusion`, `_last_run_url_info` and
  `_state` series per workflow; a workflow that stops being watched loses
  every series. A failed request never deletes series: only positive evidence
  (listing succeeded and the thing is gone) does.
- `last_success` semantics: a real success when found (or previously seen;
  it always wins). When the run history was exhausted without one: the
  workflow's `created_at` for scheduled workflows, `0` for include-list ones
  (or when `created_at` is missing). *No series* when the search was capped
  with nothing known. Never emit a value for "didn't look far enough".
- A scheduled workflow's last success is always `event=schedule` on the
  default branch; a `branch` override moves only `last_run_*`.
- Per-workflow series are written under `metrics.mu`; Gather reads under its
  read lock, so a label swap is atomic to scrapes.
- A poll with any API error does not advance
  `gh_exporter_last_poll_success_timestamp_seconds` or readiness.
- Never log the private key or the installation token.
- Public repo: examples use neutral names (`octo-org/...`).
