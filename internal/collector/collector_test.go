package collector

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jedwards1230/gh-workflow-exporter/internal/config"
	"github.com/jedwards1230/gh-workflow-exporter/internal/github"
)

// fakeGitHub is an in-memory GitHub REST API covering the endpoints the
// exporter uses. Every GET answers with a content-derived ETag and honours
// If-None-Match, like the real API.
type fakeGitHub struct {
	t  *testing.T
	mu sync.Mutex

	repos     []repository
	workflows map[string][]workflow        // repo -> workflows
	files     map[string]map[string]string // repo -> path -> content
	runs      map[int64][]run              // workflow id -> runs, newest first
	fail      map[string]int               // URL path -> forced status

	requests     []*url.URL
	notModified  int
	fileFetches  int
	runPageFetch map[int64]int // workflow id -> pages fetched
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{
		t:            t,
		workflows:    map[string][]workflow{},
		files:        map[string]map[string]string{},
		runs:         map[int64][]run{},
		fail:         map[string]int{},
		runPageFetch: map[int64]int{},
	}
}

func blobSHA(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:20])
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return
	}
	f.requests = append(f.requests, r.URL)
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(5000-len(f.requests)))
	if code, ok := f.fail[r.URL.Path]; ok {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"message":"forced"}`))
		return
	}
	body, next, status := f.route(r)
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return
	}
	if next != "" {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, next))
	}
	b, err := json.Marshal(body)
	if err != nil {
		f.t.Errorf("marshal: %v", err)
	}
	sum := sha256.Sum256(append(b, next...))
	etag := `W/"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		f.notModified++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(b)
}

func (f *fakeGitHub) route(r *http.Request) (any, string, int) {
	p := r.URL.Path
	if p == "/installation/repositories" {
		return map[string]any{"total_count": len(f.repos), "repositories": f.repos}, "", http.StatusOK
	}
	rest, ok := strings.CutPrefix(p, "/repos/")
	if !ok {
		return nil, "", http.StatusNotFound
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 {
		return nil, "", http.StatusNotFound
	}
	repo, tail := parts[0]+"/"+parts[1], parts[2]
	switch {
	case tail == "actions/workflows":
		return map[string]any{"total_count": len(f.workflows[repo]), "workflows": f.workflows[repo]}, "", http.StatusOK
	case tail == "contents/.github/workflows":
		files, ok := f.files[repo]
		if !ok {
			return nil, "", http.StatusNotFound
		}
		var entries []map[string]string
		for path, content := range files {
			entries = append(entries, map[string]string{"path": path, "sha": blobSHA(content), "type": "file"})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i]["path"] < entries[j]["path"] })
		return entries, "", http.StatusOK
	case strings.HasPrefix(tail, "contents/"):
		content, ok := f.files[repo][strings.TrimPrefix(tail, "contents/")]
		if !ok {
			return nil, "", http.StatusNotFound
		}
		f.fileFetches++
		return map[string]string{"sha": blobSHA(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))}, "", http.StatusOK
	case strings.HasPrefix(tail, "actions/workflows/") && strings.HasSuffix(tail, "/runs"):
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(tail, "actions/workflows/"), "/runs"), 10, 64)
		if err != nil {
			return nil, "", http.StatusNotFound
		}
		q := r.URL.Query()
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		if perPage == 0 {
			perPage = 30
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if page == 0 {
			page = 1
		}
		f.runPageFetch[id]++
		all := f.runs[id]
		lo, hi := min((page-1)*perPage, len(all)), min(page*perPage, len(all))
		next := ""
		if hi < len(all) {
			next = fmt.Sprintf("%s?per_page=%d&page=%d", p, perPage, page+1)
		}
		return map[string]any{"total_count": len(all), "workflow_runs": all[lo:hi]}, next, http.StatusOK
	}
	return nil, "", http.StatusNotFound
}

// run builders for the fake.
func ghRun(repo string, id int64, created time.Time, event, conclusion string) run {
	r := run{ID: id, Status: "completed", Conclusion: conclusion, Event: event, HeadBranch: "main",
		CreatedAt: created, HTMLURL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, id)}
	r.HeadRepository = &struct {
		FullName string `json:"full_name"`
	}{repo}
	return r
}

const (
	dailyYAML  = "name: build\non:\n  schedule:\n    - cron: '0 3 * * *'\n  workflow_dispatch:\n"
	weeklyYAML = "on:\n  schedule:\n    - cron: '0 6 * * 1'\n"
	pushYAML   = "on: [push]\n"
	deployYAML = "on:\n  workflow_dispatch:\n  push:\n    branches: [main]\n"
	badYAML    = "on:\n  schedule:\n    - cron: '61 * * * *'\n"
)

var testKeyPEM = func() []byte {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}()

type harness struct {
	gh  *fakeGitHub
	c   *Collector
	reg *prometheus.Registry
	api *github.Client
}

func newHarness(t *testing.T, gh *fakeGitHub, cfgYAML string) *harness {
	t.Helper()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	cfg, err := config.Parse(strings.NewReader(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	var c *Collector
	api, err := github.New(1, 2, testKeyPEM, github.Options{
		BaseURL:     srv.URL,
		OnRateLimit: func(v float64) { c.SetRateLimitRemaining(v) },
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	c = New(api, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	return &harness{gh: gh, c: c, reg: reg, api: api}
}

// series returns every sample as "name{k=v,...}" -> value, labels sorted.
func (h *harness) series(t *testing.T) map[string]float64 {
	t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var ls []string
			for _, l := range m.GetLabel() {
				ls = append(ls, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(ls)
			v := m.GetGauge().GetValue() + m.GetCounter().GetValue()
			out[mf.GetName()+"{"+strings.Join(ls, ",")+"}"] = v
		}
	}
	return out
}

// workflowSeries filters series to those naming repo and workflow.
func workflowSeries(all map[string]float64, repo, wf string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range all {
		if strings.Contains(k, "repo="+repo+",") || strings.Contains(k, "repo="+repo+"}") {
			if strings.Contains(k, "workflow="+wf) {
				out[k] = v
			}
		}
	}
	return out
}

func assertSeries(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing series %s", k)
			continue
		}
		if gv != v {
			t.Errorf("%s = %v, want %v", k, gv, v)
		}
	}
}

func assertAbsent(t *testing.T, got map[string]float64, prefixes ...string) {
	t.Helper()
	for k := range got {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				t.Errorf("unexpected series %s", k)
			}
		}
	}
}

const nightly = "octo-org/nightly"

// standardFixture: one active repo with a scheduled, a push-only, an included
// and an excluded workflow, plus an archived repo that must be ignored.
func standardFixture(t *testing.T, now time.Time) *fakeGitHub {
	gh := newFakeGitHub(t)
	gh.repos = []repository{
		{FullName: nightly, DefaultBranch: "main"},
		{FullName: "octo-org/old", DefaultBranch: "main", Archived: true},
	}
	gh.workflows[nightly] = []workflow{
		{ID: 1, Path: ".github/workflows/build.yml", State: "active"},
		{ID: 2, Path: ".github/workflows/ci.yml", State: "active"},
		{ID: 3, Path: ".github/workflows/deploy.yml", State: "active"},
		{ID: 4, Path: ".github/workflows/codeql.yml", State: "active"},
		{ID: 5, Path: "dynamic/github-code-scanning/codeql", State: "active"},
	}
	gh.workflows["octo-org/old"] = []workflow{{ID: 9, Path: ".github/workflows/build.yml", State: "active"}}
	gh.files[nightly] = map[string]string{
		".github/workflows/build.yml":  dailyYAML,
		".github/workflows/ci.yml":     pushYAML,
		".github/workflows/deploy.yml": deployYAML,
		".github/workflows/codeql.yml": weeklyYAML,
	}
	gh.files["octo-org/old"] = map[string]string{".github/workflows/build.yml": dailyYAML}
	h := func(n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }
	fork := ghRun("someone/nightly", 106, h(1), "pull_request", "failure")
	gh.runs[1] = []run{
		fork,
		ghRun(nightly, 105, h(2), "schedule", "cancelled"),
		ghRun(nightly, 104, h(3), "workflow_dispatch", "success"),
		ghRun(nightly, 103, h(26), "schedule", "success"),
	}
	gh.runs[3] = []run{
		ghRun(nightly, 302, h(5), "push", "failure"),
		ghRun(nightly, 301, h(48), "workflow_dispatch", "success"),
	}
	return gh
}

const standardConfig = `
include:
  - repo: octo-org/nightly
    workflow: deploy.yml
exclude:
  - repo: octo-org/nightly
    workflow: codeql.yml
`

func TestPollEndToEnd(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	h := newHarness(t, gh, standardConfig)
	ctx := context.Background()
	ts := func(n int) float64 { return float64(now.Add(-time.Duration(n) * time.Hour).Unix()) }

	if h.c.Ready() {
		t.Fatal("ready before first poll")
	}
	if err := h.c.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !h.c.Ready() {
		t.Fatal("not ready after successful poll")
	}
	first := h.api.TakeStats() // already taken by Poll; ensure reset
	if first.Requests != 0 {
		t.Fatalf("stats not reset by Poll: %+v", first)
	}
	got := h.series(t)
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_run_conclusion{branch=main,conclusion=cancelled,event=schedule,repo=octo-org/nightly,workflow=build.yml}`:            1,
		`github_workflow_last_run_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=build.yml}`:                                         ts(2),
		`github_workflow_last_run_url_info{repo=octo-org/nightly,run_url=https://github.com/octo-org/nightly/actions/runs/105,workflow=build.yml}`: 1,
		// The newer workflow_dispatch success does not count for a schedule.
		`github_workflow_last_success_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=build.yml}`: ts(26),
		`github_workflow_expected_interval_seconds{repo=octo-org/nightly,workflow=build.yml}`:                  86400,
		`github_workflow_grace_factor{repo=octo-org/nightly,workflow=build.yml}`:                               1.5,
		`github_workflow_state{repo=octo-org/nightly,state=active,workflow=build.yml}`:                         1,
		// Include-list: failure tracking, any-event success, no schedule series.
		`github_workflow_last_run_conclusion{branch=main,conclusion=failure,event=push,repo=octo-org/nightly,workflow=deploy.yml}`: 1,
		`github_workflow_last_success_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=deploy.yml}`:                    ts(48),
		`gh_exporter_watched_workflows{}`:         2,
		`gh_exporter_poll_errors_total{kind=5xx}`: 0,
	})
	assertAbsent(t, got,
		`github_workflow_expected_interval_seconds{repo=octo-org/nightly,workflow=deploy.yml}`,
		`github_workflow_grace_factor{repo=octo-org/nightly,workflow=deploy.yml}`,
		`github_workflow_state{repo=octo-org/nightly,state=active,workflow=ci.yml}`,
		`github_workflow_state{repo=octo-org/nightly,state=active,workflow=codeql.yml}`,
		`github_workflow_state{repo=octo-org/old`,
	)
	if got[`gh_exporter_last_poll_success_timestamp_seconds{}`] < float64(now.Unix()-5) {
		t.Errorf("last poll success not set: %v", got[`gh_exporter_last_poll_success_timestamp_seconds{}`])
	}
	if got[`gh_exporter_rate_limit_remaining{}`] <= 0 {
		t.Errorf("rate limit remaining not recorded")
	}

	// Every workflow file was fetched exactly once (build, ci, deploy; the
	// excluded codeql.yml and the archived repo are never read).
	if gh.fileFetches != 3 {
		t.Errorf("file fetches = %d, want 3", gh.fileFetches)
	}

	// A warm second poll revalidates everything with ETags: every request is
	// a 304 and no file is re-fetched or re-parsed.
	firstPollRequests, first304 := len(gh.requests), gh.notModified
	if err := h.c.Poll(ctx); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if gh.fileFetches != 3 {
		t.Errorf("file re-fetched on warm poll: %d", gh.fileFetches)
	}
	warm := gh.requests[firstPollRequests:]
	// repos + workflows + dir + runs(build) + runs(deploy)
	if len(warm) != 5 || gh.notModified-first304 != 5 {
		t.Errorf("warm poll made %d requests (%d 304s), want 5 all 304: %v", len(warm), gh.notModified-first304, warm)
	}
	if !mapsEqual(got, h.series(t), "gh_exporter_last_poll_success", "gh_exporter_rate_limit") {
		t.Errorf("warm poll changed series")
	}

	// A changed file (new blob SHA) is re-fetched and re-parsed.
	gh.mu.Lock()
	gh.files[nightly][".github/workflows/build.yml"] = weeklyYAML
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if gh.fileFetches != 4 {
		t.Errorf("file fetches after change = %d, want 4", gh.fileFetches)
	}
	assertSeries(t, h.series(t), map[string]float64{
		`github_workflow_expected_interval_seconds{repo=octo-org/nightly,workflow=build.yml}`: 7 * 86400,
	})

	assertNoServerSideFilters(t, gh.requests)
}

func mapsEqual(a, b map[string]float64, ignorePrefixes ...string) bool {
	keep := func(k string) bool {
		for _, p := range ignorePrefixes {
			if strings.HasPrefix(k, p) {
				return false
			}
		}
		return true
	}
	n := 0
	for k, v := range a {
		if !keep(k) {
			continue
		}
		n++
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	for k := range b {
		if keep(k) {
			n--
		}
	}
	return n == 0
}

func assertNoServerSideFilters(t *testing.T, reqs []*url.URL) {
	t.Helper()
	if len(reqs) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, u := range reqs {
		q := u.Query()
		for _, banned := range []string{"event", "branch", "status", "created"} {
			if q.Has(banned) {
				t.Errorf("request %s carries server-side filter %q", u, banned)
			}
		}
	}
}

func TestRunPaging(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name        string
		successAt   int // index of the only scheduled success; -1 none
		total       int
		wantPages   int
		wantSuccess bool
	}{
		{name: "success on page 1", successAt: 3, total: 40, wantPages: 1, wantSuccess: true},
		{name: "success on page 3", successAt: 22, total: 40, wantPages: 3, wantSuccess: true},
		{name: "success on page 5", successAt: 49, total: 80, wantPages: 5, wantSuccess: true},
		{name: "success beyond cap", successAt: 55, total: 80, wantPages: 5, wantSuccess: false},
		{name: "history shorter than cap", successAt: -1, total: 25, wantPages: 3, wantSuccess: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.repos = []repository{{FullName: nightly, DefaultBranch: "main"}}
			gh.workflows[nightly] = []workflow{{ID: 1, Path: ".github/workflows/build.yml", State: "active"}}
			gh.files[nightly] = map[string]string{".github/workflows/build.yml": dailyYAML}
			for i := range tt.total {
				c := "failure"
				if i == tt.successAt {
					c = "success"
				}
				gh.runs[1] = append(gh.runs[1], ghRun(nightly, int64(1000-i), now.Add(-time.Duration(i+1)*time.Hour), "schedule", c))
			}
			h := newHarness(t, gh, "")
			if err := h.c.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if gh.runPageFetch[1] != tt.wantPages {
				t.Errorf("pages fetched = %d, want %d", gh.runPageFetch[1], tt.wantPages)
			}
			got := h.series(t)[`github_workflow_last_success_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=build.yml}`]
			want := 0.0
			if tt.wantSuccess {
				want = float64(now.Add(-time.Duration(tt.successAt+1) * time.Hour).Unix())
			}
			if got != want {
				t.Errorf("last success = %v, want %v", got, want)
			}
			assertNoServerSideFilters(t, gh.requests)
		})
	}
}

func TestSeriesReplacedAndDeleted(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	h := newHarness(t, gh, standardConfig)
	ctx := context.Background()
	poll := func() map[string]float64 {
		t.Helper()
		if err := h.c.Poll(ctx); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		return h.series(t)
	}
	count := func(all map[string]float64, prefix string) int {
		n := 0
		for k := range all {
			if strings.HasPrefix(k, prefix) {
				n++
			}
		}
		return n
	}
	poll()

	// A new run with a different conclusion and event replaces the
	// conclusion, URL and state series rather than adding to them.
	gh.mu.Lock()
	gh.runs[1] = append([]run{ghRun(nightly, 107, now, "workflow_dispatch", "failure")}, gh.runs[1]...)
	gh.workflows[nightly][0].State = "disabled_manually"
	gh.mu.Unlock()
	got := poll()
	build := workflowSeries(got, nightly, "build.yml")
	if n := count(build, "github_workflow_last_run_conclusion{"); n != 1 {
		t.Errorf("conclusion series = %d, want 1: %v", n, build)
	}
	if n := count(build, "github_workflow_last_run_url_info{"); n != 1 {
		t.Errorf("url series = %d, want 1", n)
	}
	if n := count(build, "github_workflow_state{"); n != 1 {
		t.Errorf("state series = %d, want 1", n)
	}
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_run_conclusion{branch=main,conclusion=failure,event=workflow_dispatch,repo=octo-org/nightly,workflow=build.yml}`: 1,
		`github_workflow_state{repo=octo-org/nightly,state=disabled_manually,workflow=build.yml}`:                                              1,
	})

	// No going backwards: a lagging listing that drops the newest run keeps
	// the previous observation.
	gh.mu.Lock()
	gh.runs[1] = gh.runs[1][1:]
	gh.mu.Unlock()
	got = poll()
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_run_conclusion{branch=main,conclusion=failure,event=workflow_dispatch,repo=octo-org/nightly,workflow=build.yml}`: 1,
		`github_workflow_last_run_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=build.yml}`:                                     float64(now.Unix()),
	})

	// A branch override re-points the workflow: old-branch series go away.
	h.c.cfg = mustConfig(t, standardConfig+"overrides:\n  - repo: octo-org/nightly\n    workflow: build.yml\n    branch: release\n    grace_factor: 3\n")
	got = poll()
	build = workflowSeries(got, nightly, "build.yml")
	for k := range build {
		if strings.Contains(k, "branch=main") {
			t.Errorf("old-branch series survived: %s", k)
		}
	}
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_success_timestamp_seconds{branch=release,repo=octo-org/nightly,workflow=build.yml}`: 0,
		`github_workflow_grace_factor{repo=octo-org/nightly,workflow=build.yml}`:                                  3,
	})

	// Newly excluded: every series of the workflow is deleted.
	h.c.cfg = mustConfig(t, standardConfig+"  - repo: octo-org/nightly\n    workflow: build.yml\n")
	got = poll()
	if s := workflowSeries(got, nightly, "build.yml"); len(s) != 0 {
		t.Errorf("excluded workflow still exported: %v", s)
	}

	// Removed upstream: same.
	h.c.cfg = mustConfig(t, standardConfig)
	poll()
	gh.mu.Lock()
	gh.workflows[nightly] = gh.workflows[nightly][1:] // drop build.yml
	gh.mu.Unlock()
	got = poll()
	if s := workflowSeries(got, nightly, "build.yml"); len(s) != 0 {
		t.Errorf("removed workflow still exported: %v", s)
	}
	if got[`gh_exporter_watched_workflows{}`] != 1 {
		t.Errorf("watched = %v, want 1", got[`gh_exporter_watched_workflows{}`])
	}

	// Repo no longer visible: everything goes.
	gh.mu.Lock()
	gh.repos = nil
	gh.mu.Unlock()
	got = poll()
	if s := workflowSeries(got, nightly, "deploy.yml"); len(s) != 0 {
		t.Errorf("workflow of vanished repo still exported: %v", s)
	}
	if got[`gh_exporter_watched_workflows{}`] != 0 {
		t.Errorf("watched = %v, want 0", got[`gh_exporter_watched_workflows{}`])
	}
}

func mustConfig(t *testing.T, doc string) *config.Config {
	t.Helper()
	c, err := config.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPollErrors(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	gh.repos = append(gh.repos, repository{FullName: "octo-org/other", DefaultBranch: "main"})
	gh.workflows["octo-org/other"] = []workflow{{ID: 50, Path: ".github/workflows/sync.yml", State: "active"}}
	gh.files["octo-org/other"] = map[string]string{".github/workflows/sync.yml": dailyYAML}
	gh.runs[50] = []run{ghRun("octo-org/other", 501, now.Add(-time.Hour), "schedule", "success")}
	h := newHarness(t, gh, standardConfig)
	ctx := context.Background()

	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	before := h.series(t)
	okAt := before[`gh_exporter_last_poll_success_timestamp_seconds{}`]

	// One repo's workflow listing starts failing: the other repo is still
	// polled, the failing repo's series are kept, and the poll is not a
	// success.
	gh.mu.Lock()
	gh.fail["/repos/octo-org/nightly/actions/workflows"] = http.StatusBadGateway
	gh.runs[50] = append([]run{ghRun("octo-org/other", 502, now, "schedule", "failure")}, gh.runs[50]...)
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err == nil {
		t.Fatal("Poll succeeded despite a failing repo")
	}
	got := h.series(t)
	assertSeries(t, got, map[string]float64{
		`gh_exporter_poll_errors_total{kind=5xx}`:           1,
		`gh_exporter_last_poll_success_timestamp_seconds{}`: okAt,
		`github_workflow_last_run_conclusion{branch=main,conclusion=failure,event=schedule,repo=octo-org/other,workflow=sync.yml}`:      1,
		`github_workflow_last_run_conclusion{branch=main,conclusion=cancelled,event=schedule,repo=octo-org/nightly,workflow=build.yml}`: 1,
	})
	if got[`gh_exporter_watched_workflows{}`] != 3 {
		t.Errorf("watched = %v, want 3", got[`gh_exporter_watched_workflows{}`])
	}

	// A runs-listing 404 and a 403 are counted by kind.
	gh.mu.Lock()
	delete(gh.fail, "/repos/octo-org/nightly/actions/workflows")
	gh.fail["/repos/octo-org/nightly/actions/workflows/1/runs"] = http.StatusNotFound
	gh.fail["/repos/octo-org/other/actions/workflows/50/runs"] = http.StatusForbidden
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err == nil {
		t.Fatal("Poll succeeded despite failing runs")
	}
	assertSeries(t, h.series(t), map[string]float64{
		`gh_exporter_poll_errors_total{kind=404}`: 1,
		`gh_exporter_poll_errors_total{kind=403}`: 1,
		// Kept from before the failure.
		`github_workflow_last_run_conclusion{branch=main,conclusion=cancelled,event=schedule,repo=octo-org/nightly,workflow=build.yml}`: 1,
	})

	// The installation listing failing aborts the poll but deletes nothing.
	gh.mu.Lock()
	gh.fail = map[string]int{"/installation/repositories": http.StatusInternalServerError}
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err == nil {
		t.Fatal("Poll succeeded despite failing discovery")
	}
	got = h.series(t)
	if got[`gh_exporter_watched_workflows{}`] != 3 {
		t.Errorf("watched = %v after discovery failure, want 3", got[`gh_exporter_watched_workflows{}`])
	}
}

func TestReadinessNeedsCleanPoll(t *testing.T) {
	gh := standardFixture(t, time.Now())
	gh.fail["/installation/repositories"] = http.StatusUnauthorized
	h := newHarness(t, gh, standardConfig)
	if err := h.c.Poll(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if h.c.Ready() {
		t.Fatal("ready after failed poll")
	}
	assertSeries(t, h.series(t), map[string]float64{`gh_exporter_poll_errors_total{kind=auth}`: 1})
}

func TestUnparsableCron(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.repos = []repository{{FullName: nightly, DefaultBranch: "main"}}
	gh.workflows[nightly] = []workflow{{ID: 1, Path: ".github/workflows/bad.yml", State: "active"}}
	gh.files[nightly] = map[string]string{".github/workflows/bad.yml": badYAML}
	h := newHarness(t, gh, "")
	for range 2 {
		if err := h.c.Poll(context.Background()); err != nil {
			t.Fatalf("a broken workflow file must not fail the poll: %v", err)
		}
	}
	got := h.series(t)
	// Counted once per blob SHA, not once per poll.
	assertSeries(t, got, map[string]float64{
		`gh_exporter_poll_errors_total{kind=parse}`: 1,
		`gh_exporter_watched_workflows{}`:           0,
	})
	if !h.c.Ready() {
		t.Fatal("not ready")
	}
}
