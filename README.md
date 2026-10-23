# gh-workflow-exporter

A Prometheus exporter for GitHub Actions workflow health. It authenticates as a
GitHub App, discovers every scheduled workflow across the App's installation,
and exports the latest run conclusion, the last successful scheduled run, the
expected cron interval and the workflow state. Failed, stale and disabled
workflows can then be alerted on with a handful of plain PromQL rules, and a
new scheduled workflow is covered the moment it lands, with no per-workflow
configuration.

It polls (no webhooks), so it needs no public endpoint and can notice a
scheduled run that never happened.

## How it works

Every `poll_interval` (default 5m), one request at a time:

1. `GET /installation/repositories`, skipping archived repos.
2. Per repo, `GET /repos/{owner}/{repo}/actions/workflows`.
3. Per repo, list `.github/workflows` on the default branch. A workflow file is
   fetched and its `on.schedule[].cron` entries parsed only when its blob SHA
   changes; the result is cached by SHA.
4. For every watched workflow, `GET .../actions/workflows/{id}/runs?per_page=10`,
   paging back (up to 5 pages) only when the first page holds no qualifying
   success. If those 50 runs hold none and no success is known yet, a deep
   search pages `?per_page=100` (up to 5 pages, 500 runs).

Every GET is revalidated with `If-None-Match`, so a warm poll is almost
entirely `304 Not Modified` responses, which GitHub does not count against the
rate limit. The runs listing is never filtered with `event=` or `branch=`:
GitHub answers those from a lagging index that can return stale or empty
results with a `200`. Filtering happens client-side.

**Which workflows are watched:** every workflow with at least one
`schedule` cron, plus the `include` list, minus the `exclude` list. Workflows
are identified by repo full name and file basename (`octo-org/nightly`,
`build.yml`).

**Run selection** (per workflow, on the tracked branch, default the repo's
default branch):

- Only completed runs whose `head_branch` is the tracked branch count.
  Pull-request runs (`pull_request*` events, or a head repository other than
  the repo itself) are ignored.
- *Latest run*: the newest such run, any event.
- *Last success*: for scheduled workflows, the newest `success` with
  `event=schedule` (a manual re-run says nothing about the schedule). For
  include-list workflows, the newest `success` of any event.
- If the history runs out without a qualifying success, the last success is
  `0` (never succeeded). If the search hits its cap first, a previously seen
  success is kept; with none known (for example right after a restart, since
  state is in memory), the deep search runs. If even that is capped, the last
  success is *unknown*: no `last_success` series is exported for the workflow
  (a warning is logged once, and no error is counted), rather than a `0` that
  would read as "never succeeded".
- Observations never go backwards. A "newest" run older than one already seen
  is ignored, and the last success only moves forward. A streak of cancelled
  runs therefore leaves the last success at the older success, and the Stale
  alert fires.

## GitHub App setup

Create a GitHub App (it needs no webhook) with these **repository**
permissions, all read-only:

| Permission | Why |
|---|---|
| Actions: Read | List workflows and workflow runs |
| Contents: Read | Read workflow files to find their cron schedules |
| Metadata: Read | List the installation's repositories (granted by default) |

Install it on the account or organisation and choose the repositories to
watch. Note the App ID and the installation ID, and generate a private key
(PKCS#1 as downloaded from GitHub, or PKCS#8).

## Running

```bash
docker run -p 9877:9877 \
  -v /path/to/secret-dir:/etc/gh-app:ro \
  -v /path/to/config.yaml:/etc/gh-workflow-exporter/config.yaml:ro \
  ghcr.io/jedwards1230/gh-workflow-exporter:0.1.0
```

The secret dir holds three files: `app_id`, `installation_id` and
`private_key`. In Kubernetes, mount a Secret with those keys at
`/etc/gh-app`. Alternatively pass the values as environment variables, which
keeps the key off disk for local runs:

```bash
GHWE_APP_ID=123456 GHWE_INSTALLATION_ID=7890123 \
GHWE_PRIVATE_KEY="$(cat app.private-key.pem)" \
gh-workflow-exporter --config config.yaml
```

The image is distroless (`gcr.io/distroless/static-debian12:nonroot`, uid
65532), for `linux/amd64` and `linux/arm64`, tagged `vX.Y.Z` and `X.Y.Z`.
Run a single replica: the exporter keeps its observations in memory.

### Endpoints

| Path | Meaning |
|---|---|
| `/metrics` | Prometheus metrics |
| `/healthz` | Liveness: `200` while the process is serving |
| `/readyz` | `200` once a poll has completed without errors, `503` before |

### Flags and environment

Flags win over environment variables, which win over defaults.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--config` | `GHWE_CONFIG` | `/etc/gh-workflow-exporter/config.yaml` | Config file. A missing file at the default path means "all defaults"; a path you set must exist. |
| `--secret-dir` | `GHWE_SECRET_DIR` | `/etc/gh-app` | Directory with `app_id`, `installation_id`, `private_key` |
| — | `GHWE_APP_ID`, `GHWE_INSTALLATION_ID`, `GHWE_PRIVATE_KEY` | — | Credentials as values (PEM content for the key). Each one, when set, wins over its file. |
| `--listen` | `GHWE_LISTEN` | `:9877` | HTTP listen address |
| `--log-level` | `GHWE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stderr. |
| `--github-api-url` | `GHWE_GITHUB_API_URL` | `https://api.github.com` | REST API base URL (for GitHub Enterprise Server, `https://HOST/api/v3`) |
| `--version` | — | — | Print the version and exit |

### Config file

Every key is optional. Unknown keys are rejected. See
[`config.example.yaml`](config.example.yaml).

| Key | Default | Meaning |
|---|---|---|
| `poll_interval` | `5m` | Time between polls (Go duration) |
| `default_grace_factor` | `1.5` | Grace factor for every scheduled workflow without an override |
| `include[]` | — | `{repo, workflow}` workflows without a schedule to watch anyway: failure tracking and last success of any event, with no interval or grace series |
| `exclude[]` | — | `{repo, workflow}` workflows to ignore |
| `overrides[]` | — | `{repo, workflow, branch, grace_factor}`. `branch` replaces the repo's default branch for run selection, and `grace_factor` replaces `default_grace_factor` |

`repo` is `owner/name`. `workflow` is the file basename, for example
`build.yml`. Matching is case-insensitive.

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `github_workflow_last_run_conclusion` | `repo`, `workflow`, `branch`, `event`, `conclusion` | `1` for the latest completed run on the tracked branch (any event). Exactly one series per workflow: the old one is deleted when a label changes. |
| `github_workflow_last_run_timestamp_seconds` | `repo`, `workflow`, `branch` | Creation time of that run |
| `github_workflow_last_run_url_info` | `repo`, `workflow`, `run_url` | `1`; the URL of that run, for alert links |
| `github_workflow_last_success_timestamp_seconds` | `repo`, `workflow`, `branch` | Creation time of the last success that counts (scheduled workflows: `event=schedule` only). `0` when the whole run history was searched without one, so a never-succeeded workflow is stale. **Absent** when the history is longer than the search cap (550 runs) and no success was found in it. |
| `github_workflow_expected_interval_seconds` | `repo`, `workflow` | Shortest gap between consecutive fires across all of the workflow's crons, in UTC. Scheduled workflows only. |
| `github_workflow_grace_factor` | `repo`, `workflow` | Effective grace factor. Scheduled workflows only. |
| `github_workflow_state` | `repo`, `workflow`, `state` | `1` for the current state: `active`, `disabled_inactivity`, `disabled_manually`, … |
| `gh_exporter_last_poll_success_timestamp_seconds` | — | End of the last poll that completed without errors |
| `gh_exporter_rate_limit_remaining` | — | Last `x-ratelimit-remaining` seen (`0` until the first API response) |
| `gh_exporter_poll_errors_total` | `kind` | Errors by kind: `403` (also 429 and other 4xx refusals), `404`, `5xx`, `parse`, `network`, `auth` |
| `gh_exporter_watched_workflows` | — | Number of watched workflows |

All series of a workflow are deleted when it stops being watched: removed
upstream, newly excluded, no longer scheduled, or its repo archived or no
longer visible to the App. A failed request never deletes series. The
workflow keeps its last values until a later poll shows it is really gone.

**Errors and poll success.** One repo or workflow failing does not abort the
poll: the rest is still collected and exported. But any API, network, auth or
response-decoding error marks the poll unsuccessful, so
`gh_exporter_last_poll_success_timestamp_seconds` and readiness do not
advance. A workflow file with unparsable YAML or an invalid cron is different:
it is the repository's problem, not the exporter's. It is logged and counted
once per file version as `kind="parse"`, the workflow is treated as
unscheduled (GitHub will not schedule it either), and the poll still counts as
successful.

## Example alerts

```yaml
# Latest completed run failed. Joins the run URL in for the notification.
- alert: GitHubWorkflowFailed
  expr: |
    (github_workflow_last_run_conclusion{conclusion=~"failure|timed_out|startup_failure"} == 1)
      * on(repo, workflow) group_left(run_url) github_workflow_last_run_url_info
  for: 1h

# No counting success within grace × interval (+1h for GitHub's schedule jitter).
- alert: GitHubWorkflowStale
  expr: |
    time() - github_workflow_last_success_timestamp_seconds
      > on(repo, workflow)
    (github_workflow_grace_factor * github_workflow_expected_interval_seconds + 3600)
  for: 1h

# GitHub disables scheduled workflows after 60 days of repo inactivity.
- alert: GitHubWorkflowDisabled
  expr: github_workflow_state{state=~"disabled.*"} == 1
  for: 1h

# The exporter itself is down or stuck.
- alert: GitHubWorkflowExporterDown
  expr: |
    up{job="gh-workflow-exporter"} == 0
      or absent(up{job="gh-workflow-exporter"})
      or time() - gh_exporter_last_poll_success_timestamp_seconds > 1800
  for: 15m
```

The Stale rule needs no per-workflow windows: a daily cron gets
`1.5 × 24h + 1h`, a weekly one `1.5 × 7d + 1h`, a `0 0 1,15 * *` cron
`1.5 × 14d + 1h`.

## API budget

GitHub App installations get at least 5,000 requests per hour. With ETags a
warm poll is roughly one `304` per repo listing plus one per watched workflow,
and 304s are not charged. A cold start fetches each workflow file once. Each
poll logs `requests` and `not_modified` counts.

## License

See [LICENSE](LICENSE).
