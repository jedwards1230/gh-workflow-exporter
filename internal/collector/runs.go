package collector

import (
	"strings"
	"time"
)

// maxRunPages caps how far back the runs listing is paged when the first
// page holds no qualifying success.
const maxRunPages = 5

// run is the subset of a workflow run the exporter reads.
type run struct {
	ID             int64     `json:"id"`
	Status         string    `json:"status"`
	Conclusion     string    `json:"conclusion"`
	Event          string    `json:"event"`
	HeadBranch     string    `json:"head_branch"`
	CreatedAt      time.Time `json:"created_at"`
	HTMLURL        string    `json:"html_url"`
	HeadRepository *struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

// newer reports whether a is more recent than b.
func newer(a, b *run) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

// selector picks, from a stream of runs, the latest completed run on the
// tracked branch and the latest success that counts for the workflow.
type selector struct {
	repo      string
	branch    string
	scheduled bool

	latest  *run
	success *run
}

// qualifies filters to completed runs of the tracked branch in the repo
// itself. Pull-request runs (including fork PRs whose head branch shares the
// tracked branch's name) are ignored.
func (s *selector) qualifies(r *run) bool {
	if r.Status != "completed" || r.HeadBranch != s.branch {
		return false
	}
	if strings.HasPrefix(r.Event, "pull_request") {
		return false
	}
	return r.HeadRepository != nil && strings.EqualFold(r.HeadRepository.FullName, s.repo)
}

// countsAsSuccess: scheduled workflows only count scheduled successes (a
// manual re-run proves nothing about the schedule); include-list workflows
// count any successful event.
func (s *selector) countsAsSuccess(r *run) bool {
	if r.Conclusion != "success" {
		return false
	}
	return !s.scheduled || r.Event == "schedule"
}

func (s *selector) add(runs []run) {
	for i := range runs {
		r := &runs[i]
		if !s.qualifies(r) {
			continue
		}
		if s.latest == nil || newer(r, s.latest) {
			s.latest = r
		}
		if s.countsAsSuccess(r) && (s.success == nil || newer(r, s.success)) {
			s.success = r
		}
	}
}

// done reports whether paging can stop: a qualifying success also implies a
// latest completed run.
func (s *selector) done() bool { return s.success != nil }

// observation is what the exporter remembers per workflow between polls.
type observation struct {
	lastRun     *run
	lastSuccess time.Time
}

// merge folds a fresh selection into the observation without ever going
// backwards: a "latest" run older than the one already seen is ignored (the
// same run is accepted again so a re-run's new conclusion lands), and the
// last success only moves forward.
func (o *observation) merge(s *selector) {
	if s.latest != nil && (o.lastRun == nil || !newer(o.lastRun, s.latest)) {
		o.lastRun = s.latest
	}
	if s.success != nil && s.success.CreatedAt.After(o.lastSuccess) {
		o.lastSuccess = s.success.CreatedAt
	}
}
