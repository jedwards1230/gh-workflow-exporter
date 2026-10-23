// Package collector polls GitHub for the workflows the exporter watches and
// turns what it finds into Prometheus metrics.
package collector

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jedwards1230/gh-workflow-exporter/internal/config"
	"github.com/jedwards1230/gh-workflow-exporter/internal/cronx"
	"github.com/jedwards1230/gh-workflow-exporter/internal/github"
)

// workflowDir is the only directory GitHub loads workflow files from.
const workflowDir = ".github/workflows"

// API is the GitHub surface the collector needs; *github.Client implements it.
type API interface {
	Get(ctx context.Context, pathOrURL string) (*github.Response, error)
	BeginPoll()
	EndPoll()
	TakeStats() github.Stats
}

// Collector owns all polling state. Poll and Run must not be called
// concurrently; Ready may be called from any goroutine.
type Collector struct {
	api API
	cfg *config.Config
	log *slog.Logger
	m   *metrics
	now func() time.Time

	ready atomic.Bool

	// parsed caches each workflow file's schedule by blob SHA, so a file is
	// fetched and parsed only when its content changes.
	parsed map[string]*schedule
	// files remembers, per repo, the blob SHA of each workflow file path as
	// last listed. It is the fallback when a listing fails.
	files map[string]map[string]string
	// watched is keyed by config.Key(repo, workflow).
	watched map[string]*workflowState
	// warned records include entries already reported as not found.
	warned map[string]bool
}

type schedule struct {
	crons    []string
	interval time.Duration // 0: no usable schedule
}

// workflowState is one watched workflow and the label values last emitted
// for it.
type workflowState struct {
	repo, workflow, branch string
	obs                    observation
	// observed is set once a runs listing has succeeded; until then nothing
	// is emitted, so a failed first fetch cannot publish a bogus 0 success.
	observed bool
	// successUnknown: no last success observed and the run history was too
	// long to search to the end, so last_success is not exported.
	successUnknown bool
	warnedUnknown  bool

	conclusion []string
	runURL     []string
	state      []string
}

// New returns a Collector that registers its metrics on reg.
func New(api API, cfg *config.Config, log *slog.Logger, reg prometheus.Registerer) *Collector {
	return &Collector{
		api:     api,
		cfg:     cfg,
		log:     log,
		m:       newMetrics(reg),
		now:     time.Now,
		parsed:  map[string]*schedule{},
		files:   map[string]map[string]string{},
		watched: map[string]*workflowState{},
		warned:  map[string]bool{},
	}
}

// SetRateLimitRemaining records x-ratelimit-remaining; wire it to
// github.Options.OnRateLimit.
func (c *Collector) SetRateLimitRemaining(v float64) { c.m.rateLimitRemaining.Set(v) }

// Ready reports whether a poll has completed without errors.
func (c *Collector) Ready() bool { return c.ready.Load() }

// Run polls immediately and then every poll interval until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	interval := c.cfg.Interval()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, interval)
		_ = c.Poll(pctx) // outcome is logged and exported by Poll
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll carries one poll's bookkeeping.
type poll struct {
	ctx      context.Context
	failures int
	seen     map[string]bool
	matched  map[string]bool // include keys found this poll
	shas     map[string]bool // parsed-cache entries still in use
}

// fail counts and logs an error that makes this poll unsuccessful. Errors
// caused by our own context ending (shutdown, poll timeout) are not GitHub's
// fault and are not counted.
func (c *Collector) fail(p *poll, err error, msg string, attrs ...any) {
	p.failures++
	if p.ctx.Err() != nil {
		return
	}
	c.m.pollErrors.WithLabelValues(github.KindOf(err)).Inc()
	c.log.Warn(msg, append(attrs, "kind", github.KindOf(err), "err", err)...)
}

// Poll runs one discovery-and-collect pass. It returns an error if any
// request failed; partial results are still exported, but the poll does not
// count as successful (gh_exporter_last_poll_success_timestamp_seconds and
// readiness are untouched).
func (c *Collector) Poll(ctx context.Context) error {
	start := c.now()
	c.api.BeginPoll()
	defer c.api.EndPoll()
	p := &poll{ctx: ctx, seen: map[string]bool{}, matched: map[string]bool{}, shas: map[string]bool{}}

	repos, err := c.listRepos(ctx)
	if err != nil {
		c.fail(p, err, "listing installation repositories")
	} else {
		visible := map[string]bool{}
		for _, r := range repos {
			if r.Archived {
				continue
			}
			visible[strings.ToLower(r.FullName)] = true
			c.pollRepo(p, r)
		}
		// Forget repos that are gone, archived, or no longer granted.
		for k := range c.files {
			if !visible[k] {
				delete(c.files, k)
			}
		}
		c.dropUnseen(p)
		if p.failures == 0 {
			c.warnUnmatchedIncludes(p)
		}
	}
	// A failed repo listing keeps every series as-is: nothing was proven
	// gone. Shas of carried-over repos stay cached via c.files.
	for _, files := range c.files {
		for _, sha := range files {
			p.shas[sha] = true
		}
	}
	for sha := range c.parsed {
		if !p.shas[sha] {
			delete(c.parsed, sha)
		}
	}
	c.m.watched.Set(float64(len(c.watched)))

	stats := c.api.TakeStats()
	c.log.Info("poll complete",
		"requests", stats.Requests,
		"not_modified", stats.NotModified,
		"errors", p.failures,
		"watched", len(c.watched),
		"duration", c.now().Sub(start).Round(time.Millisecond).String())
	if p.failures > 0 {
		return fmt.Errorf("poll finished with %d errors", p.failures)
	}
	c.m.lastPollSuccess.Set(float64(c.now().UnixNano()) / 1e9)
	c.ready.Store(true)
	return nil
}

type repository struct {
	FullName      string `json:"full_name"`
	Archived      bool   `json:"archived"`
	DefaultBranch string `json:"default_branch"`
}

type workflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"`
}

func (c *Collector) listRepos(ctx context.Context) ([]repository, error) {
	var all []repository
	next := "/installation/repositories?per_page=100"
	for next != "" {
		res, err := c.api.Get(ctx, next)
		if err != nil {
			return nil, err
		}
		var page struct {
			Repositories []repository `json:"repositories"`
		}
		if err := github.Decode(res.Body, &page, "decoding installation repositories"); err != nil {
			return nil, err
		}
		all = append(all, page.Repositories...)
		next = res.Next
	}
	return all, nil
}

func repoPath(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// carryOver keeps every watched workflow of repo as-is for this poll.
func (c *Collector) carryOver(p *poll, repo string) {
	for k, st := range c.watched {
		if strings.EqualFold(st.repo, repo) {
			p.seen[k] = true
		}
	}
}

func (c *Collector) pollRepo(p *poll, r repository) {
	ctx := p.ctx
	workflows, err := c.listWorkflows(ctx, r.FullName)
	if err != nil {
		c.fail(p, err, "listing workflows", "repo", r.FullName)
		c.carryOver(p, r.FullName)
		return
	}
	files := c.workflowFiles(p, r, workflows)

	for _, wf := range workflows {
		name := path.Base(wf.Path)
		key := config.Key(r.FullName, name)
		if c.cfg.Excluded(r.FullName, name) {
			continue
		}
		included := c.cfg.Included(r.FullName, name)
		if included {
			p.matched[key] = true
		}
		sched, ok := c.scheduleFor(p, r, wf, files)
		if !ok {
			// The file could not be read; keep what we had.
			if _, known := c.watched[key]; known {
				p.seen[key] = true
			}
			continue
		}
		scheduled := sched != nil && sched.interval > 0
		if !scheduled && !included {
			continue
		}
		branch := c.cfg.Branch(r.FullName, name, r.DefaultBranch)
		st := c.watched[key]
		if st == nil || st.repo != r.FullName || st.workflow != name || st.branch != branch {
			// New, renamed, or re-pointed at another branch: earlier
			// observations no longer describe this series.
			if st != nil {
				c.m.deleteWorkflow(st.repo, st.workflow)
			}
			st = &workflowState{repo: r.FullName, workflow: name, branch: branch}
			c.watched[key] = st
		}
		p.seen[key] = true

		sel := &selector{repo: r.FullName, branch: branch, scheduled: scheduled}
		if err := c.collectRuns(ctx, st, wf.ID, sel); err != nil {
			c.fail(p, err, "listing workflow runs", "repo", r.FullName, "workflow", name)
		} else {
			st.obs.merge(sel)
			st.observed = true
		}
		if st.observed {
			c.emit(st, wf.State, scheduled, sched)
		}
	}
}

func (c *Collector) listWorkflows(ctx context.Context, repo string) ([]workflow, error) {
	var all []workflow
	next := repoPath(repo) + "/actions/workflows?per_page=100"
	for next != "" {
		res, err := c.api.Get(ctx, next)
		if err != nil {
			return nil, err
		}
		var page struct {
			Workflows []workflow `json:"workflows"`
		}
		if err := github.Decode(res.Body, &page, "decoding workflows of "+repo); err != nil {
			return nil, err
		}
		all = append(all, page.Workflows...)
		next = res.Next
	}
	return all, nil
}

// workflowFiles returns path → blob SHA for the repo's workflow directory on
// its default branch (schedules only ever fire from the default branch). The
// listing is ETag-revalidated, so an unchanged directory costs a 304. On
// failure it falls back to the last known listing.
func (c *Collector) workflowFiles(p *poll, r repository, workflows []workflow) map[string]string {
	key := strings.ToLower(r.FullName)
	need := false
	for _, wf := range workflows {
		if strings.HasPrefix(wf.Path, workflowDir+"/") {
			need = true
			break
		}
	}
	if !need {
		delete(c.files, key)
		return nil
	}
	u := repoPath(r.FullName) + "/contents/" + workflowDir + "?ref=" + url.QueryEscape(r.DefaultBranch)
	res, err := c.api.Get(p.ctx, u)
	if err != nil {
		if github.IsNotFound(err) {
			// No workflow directory on the default branch (e.g. workflows
			// that only exist on other branches): nothing is scheduled.
			c.files[key] = map[string]string{}
			return c.files[key]
		}
		c.fail(p, err, "listing workflow files", "repo", r.FullName)
		return c.files[key]
	}
	var entries []struct {
		Path string `json:"path"`
		SHA  string `json:"sha"`
		Type string `json:"type"`
	}
	if err := github.Decode(res.Body, &entries, "decoding workflow directory of "+r.FullName); err != nil {
		c.fail(p, err, "listing workflow files", "repo", r.FullName)
		return c.files[key]
	}
	files := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			files[e.Path] = e.SHA
		}
	}
	c.files[key] = files
	return files
}

// scheduleFor returns the workflow's parsed schedule (nil when it has none)
// and false when it could not be determined this poll.
func (c *Collector) scheduleFor(p *poll, r repository, wf workflow, files map[string]string) (*schedule, bool) {
	if files == nil {
		if !strings.HasPrefix(wf.Path, workflowDir+"/") {
			return nil, true // dynamic workflow (e.g. CodeQL default setup): no file
		}
		return nil, false
	}
	sha, ok := files[wf.Path]
	if !ok {
		return nil, true // not on the default branch, so it cannot be scheduled
	}
	p.shas[sha] = true
	if s, ok := c.parsed[sha]; ok {
		return s, true
	}

	u := repoPath(r.FullName) + "/contents/" + escapePath(wf.Path) + "?ref=" + url.QueryEscape(r.DefaultBranch)
	res, err := c.api.Get(p.ctx, u)
	if err != nil {
		c.fail(p, err, "fetching workflow file", "repo", r.FullName, "path", wf.Path)
		return nil, false
	}
	var file struct {
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := github.Decode(res.Body, &file, "decoding workflow file "+wf.Path); err != nil {
		c.fail(p, err, "fetching workflow file", "repo", r.FullName, "path", wf.Path)
		return nil, false
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil || file.Encoding != "base64" {
		c.fail(p, &github.Error{Kind: github.KindParse, Op: "decoding " + wf.Path, Err: fmt.Errorf("unexpected encoding %q", file.Encoding)},
			"fetching workflow file", "repo", r.FullName, "path", wf.Path)
		return nil, false
	}

	s := &schedule{}
	crons, err := parseSchedules(content)
	if err == nil && len(crons) > 0 {
		s.crons = crons
		s.interval, err = cronx.Interval(crons)
	}
	if err != nil {
		// A broken workflow file is the repo's problem, not the poller's: it
		// is counted (once per blob SHA) and logged, but does not fail the
		// poll. GitHub refuses to schedule such a file too, so it is treated
		// as unscheduled.
		c.m.pollErrors.WithLabelValues(github.KindParse).Inc()
		c.log.Warn("workflow schedule unusable; treating as unscheduled",
			"repo", r.FullName, "path", wf.Path, "sha", sha, "err", err)
		s.interval = 0
	}
	c.parsed[sha] = s
	return s, true
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// collectRuns feeds the selector from the runs listing and settles whether
// the workflow's last success is known.
//
// The shallow listing (per_page=10, at most maxRunPages pages) is enough
// almost always. When it hits its cap without a qualifying success:
//   - a previously observed success is kept (no going backwards) and nothing
//     more is fetched;
//   - otherwise a deep listing (per_page=100, at most maxRunPages pages) is
//     searched. A workflow that also runs on push, PRs or Dependabot can
//     bury its last scheduled success under more than 50 other runs, and
//     state is in memory, so this happens after every restart.
//
// If even the deep listing is capped, the last success is unknown: no series
// is emitted for it (an "absent" is honest; a 0 would page as "never
// succeeded"). Only a listing that ran out of history means "none".
func (c *Collector) collectRuns(ctx context.Context, st *workflowState, workflowID int64, sel *selector) error {
	base := fmt.Sprintf("%s/actions/workflows/%d/runs", repoPath(st.repo), workflowID)
	capped, err := c.fetchRuns(ctx, base+"?per_page=10", st.repo, sel)
	if err != nil {
		return err
	}
	if capped && st.obs.lastSuccess.IsZero() {
		if capped, err = c.fetchRuns(ctx, base+"?per_page=100", st.repo, sel); err != nil {
			return err
		}
	}
	st.successUnknown = capped && sel.success == nil && st.obs.lastSuccess.IsZero()
	switch {
	case st.successUnknown && !st.warnedUnknown:
		st.warnedUnknown = true
		c.log.Warn("last success not found within the deep search cap; not exporting last_success",
			"repo", st.repo, "workflow", st.workflow, "branch", st.branch, "max_runs", maxRunPages*100)
	case !st.successUnknown:
		st.warnedUnknown = false
	}
	return nil
}

// fetchRuns feeds the selector from first and its next pages, newest first,
// stopping at a qualifying success or after maxRunPages pages. It reports
// capped=true when it stopped at the page cap with more history left. The
// listing is never filtered server-side with event= or branch=: GitHub
// answers those from a lagging index that can return stale or empty results
// with a 200.
func (c *Collector) fetchRuns(ctx context.Context, first, repo string, sel *selector) (capped bool, err error) {
	next := first
	for page := 1; page <= maxRunPages && next != ""; page++ {
		res, err := c.api.Get(ctx, next)
		if err != nil {
			return false, err
		}
		var body struct {
			Runs []run `json:"workflow_runs"`
		}
		if err := github.Decode(res.Body, &body, "decoding runs of "+repo); err != nil {
			return false, err
		}
		sel.add(body.Runs)
		if sel.done() {
			return false, nil
		}
		next = res.Next
	}
	return next != "", nil
}

// emit writes a workflow's series, replacing any label set that changed.
func (c *Collector) emit(st *workflowState, state string, scheduled bool, sched *schedule) {
	m := c.m
	if r := st.obs.lastRun; r != nil {
		conclusion := r.Conclusion
		if conclusion == "" {
			conclusion = "unknown"
		}
		st.conclusion = swap(m.lastRunConclusion, st.conclusion,
			[]string{st.repo, st.workflow, st.branch, r.Event, conclusion})
		m.lastRunTimestamp.WithLabelValues(st.repo, st.workflow, st.branch).Set(float64(r.CreatedAt.Unix()))
		st.runURL = swap(m.lastRunURL, st.runURL, []string{st.repo, st.workflow, r.HTMLURL})
	}
	switch {
	case !st.obs.lastSuccess.IsZero():
		m.lastSuccess.WithLabelValues(st.repo, st.workflow, st.branch).Set(float64(st.obs.lastSuccess.Unix()))
	case st.successUnknown:
		m.lastSuccess.DeleteLabelValues(st.repo, st.workflow, st.branch)
	default:
		// History exhausted without a qualifying success: never succeeded.
		m.lastSuccess.WithLabelValues(st.repo, st.workflow, st.branch).Set(0)
	}

	if scheduled {
		m.expectedInterval.WithLabelValues(st.repo, st.workflow).Set(sched.interval.Seconds())
		m.graceFactor.WithLabelValues(st.repo, st.workflow).Set(c.cfg.GraceFactor(st.repo, st.workflow))
	} else {
		m.expectedInterval.DeleteLabelValues(st.repo, st.workflow)
		m.graceFactor.DeleteLabelValues(st.repo, st.workflow)
	}
	if state == "" {
		state = "unknown"
	}
	st.state = swap(m.state, st.state, []string{st.repo, st.workflow, state})
}

// dropUnseen deletes every series of workflows not seen this poll: removed
// upstream, newly excluded, no longer scheduled, or in a repo that vanished.
func (c *Collector) dropUnseen(p *poll) {
	for k, st := range c.watched {
		if p.seen[k] {
			continue
		}
		c.m.deleteWorkflow(st.repo, st.workflow)
		delete(c.watched, k)
		c.log.Info("workflow no longer watched", "repo", st.repo, "workflow", st.workflow)
	}
}

func (c *Collector) warnUnmatchedIncludes(p *poll) {
	for _, ref := range c.cfg.Include {
		k := ref.Key()
		if p.matched[k] || c.warned[k] {
			continue
		}
		c.warned[k] = true
		c.log.Warn("include entry matches no visible workflow", "repo", ref.Repo, "workflow", ref.Workflow)
	}
}
