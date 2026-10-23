package collector

import (
	"slices"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jedwards1230/gh-workflow-exporter/internal/github"
)

type metrics struct {
	// mu guards the per-workflow vecs as a unit: writers take it while
	// swapping label sets, and Gather takes the read side (see locked).
	mu sync.RWMutex

	lastRunConclusion *prometheus.GaugeVec
	lastRunTimestamp  *prometheus.GaugeVec
	lastRunURL        *prometheus.GaugeVec
	lastSuccess       *prometheus.GaugeVec
	expectedInterval  *prometheus.GaugeVec
	graceFactor       *prometheus.GaugeVec
	state             *prometheus.GaugeVec

	lastPollSuccess    prometheus.Gauge
	rateLimitRemaining prometheus.Gauge
	pollErrors         *prometheus.CounterVec
	watched            prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	gv := func(name, help string, labels ...string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	}
	m := &metrics{
		lastRunConclusion: gv("github_workflow_last_run_conclusion",
			"1 for the conclusion of the latest completed run on the tracked branch (any event).",
			"repo", "workflow", "branch", "event", "conclusion"),
		lastRunTimestamp: gv("github_workflow_last_run_timestamp_seconds",
			"Creation time of the latest completed run on the tracked branch.",
			"repo", "workflow", "branch"),
		lastRunURL: gv("github_workflow_last_run_url_info",
			"Always 1; carries the URL of the latest completed run.",
			"repo", "workflow", "run_url"),
		lastSuccess: gv("github_workflow_last_success_timestamp_seconds",
			"Creation time of the last successful run that counts (scheduled workflows: event=schedule on the default branch); 0 if the run history holds none; absent if the search was capped.",
			"repo", "workflow", "branch"),
		expectedInterval: gv("github_workflow_expected_interval_seconds",
			"Longest gap in seconds between consecutive scheduled fires across all of the workflow's cron entries.",
			"repo", "workflow"),
		graceFactor: gv("github_workflow_grace_factor",
			"Effective grace factor applied to the expected interval.",
			"repo", "workflow"),
		state: gv("github_workflow_state",
			"1 for the workflow's current state (active, disabled_inactivity, disabled_manually, ...).",
			"repo", "workflow", "state"),
		lastPollSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gh_exporter_last_poll_success_timestamp_seconds",
			Help: "Unix time at the end of the last poll that completed without errors.",
		}),
		rateLimitRemaining: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gh_exporter_rate_limit_remaining",
			Help: "GitHub API requests remaining in the installation's rate-limit window (x-ratelimit-remaining).",
		}),
		pollErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gh_exporter_poll_errors_total",
			Help: "Errors met while polling GitHub, by kind.",
		}, []string{"kind"}),
		watched: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gh_exporter_watched_workflows",
			Help: "Number of workflows currently watched.",
		}),
	}
	for _, v := range m.perWorkflow() {
		reg.MustRegister(locked{mu: &m.mu, c: v})
	}
	reg.MustRegister(m.lastPollSuccess, m.rateLimitRemaining, m.pollErrors, m.watched)
	for _, k := range github.Kinds {
		m.pollErrors.WithLabelValues(k)
	}
	return m
}

// perWorkflow lists every vec keyed by (repo, workflow).
func (m *metrics) perWorkflow() []*prometheus.GaugeVec {
	return []*prometheus.GaugeVec{m.lastRunConclusion, m.lastRunTimestamp, m.lastRunURL,
		m.lastSuccess, m.expectedInterval, m.graceFactor, m.state}
}

// locked collects a collector under the read side of mu.
type locked struct {
	mu *sync.RWMutex
	c  prometheus.Collector
}

func (l locked) Describe(ch chan<- *prometheus.Desc) { l.c.Describe(ch) }

func (l locked) Collect(ch chan<- prometheus.Metric) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	l.c.Collect(ch)
}

// deleteWorkflow removes every series of one workflow. The caller holds mu.
func (m *metrics) deleteWorkflow(repo, workflow string) {
	match := prometheus.Labels{"repo": repo, "workflow": workflow}
	for _, v := range m.perWorkflow() {
		v.DeletePartialMatch(match)
	}
}

// swap (caller holds mu) sets the series for labels to 1 and deletes the previously emitted
// label set, so exactly one series exists. It returns labels for the caller
// to remember.
func swap(v *prometheus.GaugeVec, prev, labels []string) []string {
	v.WithLabelValues(labels...).Set(1)
	if prev != nil && !slices.Equal(prev, labels) {
		v.DeleteLabelValues(prev...)
	}
	return labels
}
