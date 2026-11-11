# Contract

The requirements source of truth for gh-workflow-exporter. Consumers are
programs: Prometheus scrapes, alert rules and probes. Every clause below is a
stable API; changing one is a breaking change and needs a `semver:major` label
and an update here first. [`README.md`](../README.md) is the user-facing
explanation of the same behaviour; when the two disagree, this file wins and
the README is fixed.

Test names are in `internal/<pkg>/*_test.go` unless a file is given.

## §1 Metrics

### §1.1 Names and labels

Defined in `internal/collector/metrics.go`. Names, label names and label order
are fixed.

| Metric | Labels | Kind |
|---|---|---|
| `github_workflow_last_run_conclusion` | `repo`, `workflow`, `branch`, `event`, `conclusion` | gauge, value `1` |
| `github_workflow_last_run_timestamp_seconds` | `repo`, `workflow`, `branch` | gauge |
| `github_workflow_last_run_url_info` | `repo`, `workflow`, `run_url` | gauge, value `1` |
| `github_workflow_last_success_timestamp_seconds` | `repo`, `workflow`, `branch` | gauge |
| `github_workflow_expected_interval_seconds` | `repo`, `workflow` | gauge |
| `github_workflow_grace_factor` | `repo`, `workflow` | gauge |
| `github_workflow_state` | `repo`, `workflow`, `state` | gauge, value `1` |
| `gh_exporter_last_poll_success_timestamp_seconds` | — | gauge |
| `gh_exporter_rate_limit_remaining` | — | gauge |
| `gh_exporter_poll_errors_total` | `kind` | counter |
| `gh_exporter_repo_poll_errors_total` | `repo`, `kind` | counter |
| `gh_exporter_watched_workflows` | — | gauge |

Enforced end to end by `TestPollEndToEnd`.

### §1.2 Label values

- `repo` is the GitHub full name (`owner/name`); `workflow` is the workflow
  file basename (`build.yml`).
- `branch` on `last_run_*` is the tracked branch (§2.1). On `last_success` it
  is the default branch for scheduled workflows, the tracked branch for
  include-list workflows.
- `kind` is exactly one of `403`, `404`, `5xx`, `parse`, `network`, `auth`
  (`github.Kinds` in `internal/github/errors.go`). 429 and other 4xx refusals
  fold into `403`; a 401 that survives the token-refresh retry is `auth`. All
  six are pre-initialised to `0`. `TestErrorKinds`, `TestPollErrors`.
- `gh_exporter_repo_poll_errors_total` uses the same kinds, minus the global
  ones (§5.1). A repo's series is created at its first error and deleted once
  the repo is no longer visible to the App. `TestRepoFailuresAndPollSuccess`.

### §1.3 Series lifecycle

1. Exactly one `last_run_conclusion`, `last_run_url_info` and `state` series
   per workflow. A label change replaces the old series in the same locked
   write. `TestSeriesReplacedAndDeleted`, `TestConcurrentGatherDuringPoll`.
2. Every series of a workflow is deleted when it stops being watched: removed,
   newly excluded, no longer scheduled, repo archived or no longer visible.
   `TestSeriesReplacedAndDeleted`.
3. A failed request never deletes or replaces series; only a successful
   listing showing the thing is gone does.
   `TestBranchRepointKeepsSeriesUntilListingSucceeds`, `TestPollErrors`,
   `TestPollDeadlineCountsAsNetwork`.
4. `expected_interval_seconds` and `grace_factor` exist only for scheduled
   workflows whose crons can be evaluated. `TestUnparsableCron`.

## §2 Run selection

Implemented in `internal/collector/runs.go`.

1. **Tracked branch**: the repo's default branch, or an override's `branch`.
2. **Qualifying run**: `status=completed`, not a `pull_request*` event, head
   repository equal to the repo. `TestSelector` ("pull request runs ignored",
   "fork run on same-named branch ignored", "other branch and in-progress
   ignored").
3. **Latest run**: newest qualifying run on the tracked branch, any event.
4. **Counting success**: scheduled workflows count only `success` with
   `event=schedule` on the default branch, whatever the override branch.
   Include-list workflows count `success` of any event on the tracked branch.
   `TestSelector` ("manual success does not count for scheduled workflow",
   "include-list counts any-event success"), `TestOverrideBranchRunsBeyondFirstPage`.
5. **Cancelled is not a failure and not a success.** A cancelled run is the
   latest run (`conclusion="cancelled"`) but leaves `last_success` where it was.
   `TestSelector` ("cancel streak keeps older success").
6. **No going backwards**: a latest run older than the one already observed is
   ignored (the same run is re-accepted so a re-run's conclusion lands), and
   `last_success` only moves forward. `TestObservationNoGoingBackwards`.
7. **Server-side filters are forbidden.** Runs listings never carry `event`,
   `branch`, `status` or `created` query parameters; filtering is client-side.
   `assertNoServerSideFilters`, called from `TestPollEndToEnd` and `TestRunPaging`.
8. **Paging**: shallow `per_page=10`, at most 5 pages, only while no counting
   success is found and the known last success is not reached. Then, if still
   unresolved, a deep `per_page=100` walk, at most 5 pages.
   `TestRunPaging`, `TestRunPagingKeepsKnownSuccess`,
   `TestBuriedNewerSuccessFoundByDeepWalk`, `TestDeepSearchCappedThenResolved`.

## §3 `last_success` value

Enforced by `TestLastSuccessOnExhaustedHistory` and `TestDeepSearchCappedThenResolved`.

| Situation | Value |
|---|---|
| Counting success found, or one seen earlier | its run's `created_at` (a known success always wins) |
| History exhausted, none found, scheduled workflow | the workflow's `created_at` |
| History exhausted, none found, include-list workflow | `0` |
| `created_at` missing or malformed | `0` |
| Deep search capped, nothing known | **no series** (warning logged once, no error counted) |

Never emit a value meaning "did not look far enough".

## §4 Expected interval

`internal/cronx`. The longest gap between consecutive fires of the merged
sequence of all the workflow's cron entries, evaluated in UTC over a 9-year
window, floored at 300 s. Standard 5-field crons only (no `@descriptors`);
day-of-week `7` is Sunday; DOM/DOW follow the cron OR/AND rule. Any invalid
entry, or a cron that never fires, makes the whole workflow un-evaluable (§1.3.4).
`TestInterval`, `TestDayMatchingAgreesWithRobfig`.

Schedules come from `on.schedule[].cron` in the default-branch workflow file;
YAML anchors, aliases and `<<` merges are resolved, and a YAML 1.1 `true` key
stands in for `on` only when no literal `on` exists. A file that is not valid
YAML is unscheduled. `TestParseSchedules`, `TestParseSchedulesPrefersOnKey`.

## §5 Poll success and readiness

1. A poll is unsuccessful, so `gh_exporter_last_poll_success_timestamp_seconds`
   and `/readyz` do not advance, only on a **global** failure or when **every**
   polled repo failed. Global: the installation's repository listing failing,
   any `auth` or token-mint error, or rate limiting anywhere in the poll (a
   429, or a 403 with `x-ratelimit-remaining: 0` or `Retry-After`). The rest of
   the poll is still collected. `TestRepoFailuresAndPollSuccess`,
   `TestPollErrors`, `TestReadinessNeedsCleanPoll`, `TestRateLimited`.
2. An error inside one repo is that repo's: counted in both
   `gh_exporter_repo_poll_errors_total{repo,kind}` and
   `gh_exporter_poll_errors_total{kind}`, its workflows keep their previous
   series, and the poll still succeeds. Global errors are counted only in
   `gh_exporter_poll_errors_total`. `TestRepoFailuresAndPollSuccess`.
3. A poll that overruns `poll_interval` is cut off, counted as `network`, and is
   a global failure. `TestPollDeadlineCountsAsNetwork`.
4. A broken workflow file is the repository's problem: counted once per file
   version as `parse`, logged, and the poll still succeeds. `TestUnparsableCron`.
5. A `.github/workflows` 404 on the default branch while the Actions API lists
   workflows there is resolved by listing the repo root. Root readable: the
   directory is absent on that branch, nothing is scheduled, no error.
   Root hidden too: missing **Contents: read**, a repo error (`404`), series
   kept, warning once per repo, whether the workflows are active or disabled.
   `TestWorkflowDirRemovedFromDefaultBranch`,
   `TestMissingContentsPermissionWarnsOnce`,
   `TestMissingDirWithOnlyInactiveWorkflows`,
   `TestHiddenDirWithOnlyInactiveWorkflows`.

## §6 Endpoints

`main.go` `newMux`; `TestEndpoints` in `main_test.go`.

| Path | Contract |
|---|---|
| `/metrics` | Prometheus exposition, `200` |
| `/healthz` | `200` while serving |
| `/readyz` | `503` until a poll succeeds (§5.1), then `200` |

## §7 Configuration

### §7.1 Flags and environment

Flags > environment > defaults. Names and defaults as in the README's "Flags
and environment" table. `TestParseOptionsPrecedence` in `main_test.go`.

- A missing config file at the default path means all defaults; a missing file
  at a path the operator set is fatal; an invalid file is fatal at either path.
  `TestLoadConfigMissing`.
- Credentials: `GHWE_APP_ID`, `GHWE_INSTALLATION_ID`, `GHWE_PRIVATE_KEY` each
  win over the matching file (`app_id`, `installation_id`, `private_key`) in
  the secret dir. PKCS#1 and PKCS#8 keys are accepted. `TestLoadCredentials`,
  `TestParsePrivateKey`.

### §7.2 Config file

Shape and comments: [`config.example.yaml`](../config.example.yaml), parsed by
`TestParse`.

- Unknown keys are rejected. `TestParseInvalid` ("unknown key").
- `poll_interval` ≥ `1m`, default `5m`. `default_grace_factor` > 0, default
  `1.5`. An omitted or zero value takes the default. `TestParseDefaults`,
  `TestParseInvalid`.
- `include` / `exclude` / `overrides` entries need `repo` in `owner/name` form
  and `workflow` as a file basename. Override `grace_factor` must be > 0;
  duplicate overrides are rejected. `TestParseInvalid`.
- Matching on `repo` and `workflow` is case-insensitive. `TestParse`
  ("included case-insensitive").
- `exclude` beats both `include` and having a schedule (checked first in
  `internal/collector/collector.go`).

## §8 Security

1. The App private key and installation token never appear in logs, labels or
   on disk. Logs are tested (`TestLogsNeverContainSecrets`); labels and disk
   are review-only.
2. Pagination `next` links that leave the configured API host are rejected.
   `TestOffHostNextRejected`.
3. Response bodies are size-capped. `TestOversizedBody`, `TestOversizedMintBody`.
