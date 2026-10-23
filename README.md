# gh-workflow-exporter

A Prometheus exporter for GitHub Actions workflow health. It authenticates as a
GitHub App, discovers every scheduled workflow across the App's installation,
and exports the latest run conclusion, last success time, expected cron
interval and workflow state, so failed, stale and disabled workflows can be
alerted on with plain PromQL.

Work in progress.
