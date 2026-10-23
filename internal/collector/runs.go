package collector

import (
	"strings"
	"time"
)

// maxRunPages caps how far back each runs listing (shallow per_page=10, then
// deep per_page=100) is paged when no qualifying success has been found.
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
	repo string
	// runBranch selects the latest run; successBranch the last success.
	runBranch, successBranch string
	scheduled                bool

	latest  *run
	success *run
}

// qualifies filters to completed runs in the repo itself. Pull-request runs
// (including fork PRs whose head branch shares a tracked branch's name) are
// ignored.
func (s *selector) qualifies(r *run) bool {
	if r.Status != "completed" {
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
	if r.Conclusion != "success" || r.HeadBranch != s.successBranch {
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
		// Copies, so the selector does not pin the whole decoded page.
		if r.HeadBranch == s.runBranch && (s.latest == nil || newer(r, s.latest)) {
			cp := *r
			s.latest = &cp
		}
		if s.countsAsSuccess(r) && (s.success == nil || newer(r, s.success)) {
			cp := *r
			s.success = &cp
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

// covers reports whether paging further back past page is pointless: the
// page already reaches back to the known last success, and the latest run is
// either in hand or known to be newer than anything older pages hold.
func (o *observation) covers(s *selector, page []run) bool {
	if o.lastSuccess.IsZero() || len(page) == 0 {
		return false
	}
	oldest := page[0].CreatedAt
	for _, r := range page[1:] {
		if r.CreatedAt.Before(oldest) {
			oldest = r.CreatedAt
		}
	}
	if oldest.After(o.lastSuccess) {
		return false
	}
	return s.latest != nil || (o.lastRun != nil && !oldest.After(o.lastRun.CreatedAt))
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
