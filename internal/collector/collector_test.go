package collector

import (
	"bytes"
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

// fakeToken is the installation token the fake mints; logs must never
// contain it.
const fakeToken = "ghs_FAKEinstallationTOKEN0123456789"

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
	delay     map[string]time.Duration     // URL path -> response delay

	requests     []*url.URL
	notModified  int
	fileFetches  int
	runPageFetch map[string]int // "<workflow id>/<per_page>" -> pages fetched
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{
		t:            t,
		workflows:    map[string][]workflow{},
		files:        map[string]map[string]string{},
		runs:         map[int64][]run{},
		fail:         map[string]int{},
		delay:        map[string]time.Duration{},
		runPageFetch: map[string]int{},
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
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, fakeToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return
	}
	if d, ok := f.delay[r.URL.Path]; ok {
		f.mu.Unlock()
		select {
		case <-time.After(d):
		case <-r.Context().Done():
		}
		f.mu.Lock()
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
		f.runPageFetch[fmt.Sprintf("%d/%d", id, perPage)]++
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
	logs *syncBuffer
	gh   *fakeGitHub
	c    *Collector
	reg  *prometheus.Registry
	api  *github.Client
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
	logs := &syncBuffer{}
	c = New(api, cfg, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), reg)
	return &harness{logs: logs, gh: gh, c: c, reg: reg, api: api}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pollStats returns the requests/not_modified of every "poll complete" log
// line so far, in order.
func (h *harness) pollStats(t *testing.T) [][2]int {
	t.Helper()
	var out [][2]int
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var rec struct {
			Msg         string `json:"msg"`
			Requests    int    `json:"requests"`
			NotModified int    `json:"not_modified"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec.Msg == "poll complete" {
			out = append(out, [2]int{rec.Requests, rec.NotModified})
		}
	}
	return out
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
	// Cold poll: token mint + repos + workflows + workflow dir + 3 files
	// (build, ci, deploy) + runs of build + runs of deploy.
	if got := h.pollStats(t); len(got) != 1 || got[0] != [2]int{9, 0} {
		t.Fatalf("cold poll stats = %v, want [[9 0]]", got)
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
	if got := h.pollStats(t); len(got) != 2 || got[1] != [2]int{5, 5} {
		t.Errorf("warm poll stats = %v, want second [5 5]", got)
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

const lastSuccessKey = `github_workflow_last_success_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=build.yml}`

// pagingFixture: one scheduled workflow whose runs are all scheduled
// failures except a success at index successAt (-1: none), newest first.
func pagingFixture(t *testing.T, now time.Time, total, successAt int) *fakeGitHub {
	gh := newFakeGitHub(t)
	gh.repos = []repository{{FullName: nightly, DefaultBranch: "main"}}
	gh.workflows[nightly] = []workflow{{ID: 1, Path: ".github/workflows/build.yml", State: "active",
		CreatedAt: pagingCreated(now).Format(time.RFC3339)}}
	gh.files[nightly] = map[string]string{".github/workflows/build.yml": dailyYAML}
	gh.runs[1] = pagingRuns(now, 0, total, successAt)
	return gh
}

// pagingCreated is the fixture workflow's created_at, older than every run.
func pagingCreated(now time.Time) time.Time { return now.Add(-1000 * time.Hour) }

// pagingRuns builds runs with ids/ages offset by start, one hour apart.
func pagingRuns(now time.Time, start, n, successAt int) []run {
	var out []run
	for i := start; i < start+n; i++ {
		c := "failure"
		if i == successAt {
			c = "success"
		}
		out = append(out, ghRun(nightly, int64(100000-i), now.Add(-time.Duration(i+1)*time.Hour), "schedule", c))
	}
	return out
}

func TestRunPaging(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	const (
		absent  = -2 // no last_success series
		created = -1 // history exhausted: the workflow's created_at
	)
	tests := []struct {
		name        string
		total       int
		successAt   int // -1: no success at all
		wantShallow int // per_page=10 pages fetched
		wantDeep    int // per_page=100 pages fetched
		want        int // index of the success expected, or absent/created
	}{
		{name: "success on page 1", total: 40, successAt: 3, wantShallow: 1, want: 3},
		{name: "success on page 3", total: 40, successAt: 22, wantShallow: 3, want: 22},
		{name: "success on last shallow page", total: 80, successAt: 49, wantShallow: 5, want: 49},
		{name: "history exhausted on page 1", total: 8, successAt: -1, wantShallow: 1, want: created},
		{name: "history exhausted before cap", total: 25, successAt: -1, wantShallow: 3, want: created},
		{name: "cap hit, deep search finds success", total: 80, successAt: 55, wantShallow: 5, wantDeep: 1, want: 55},
		{name: "cap hit, deep search finds success on page 3", total: 400, successAt: 250, wantShallow: 5, wantDeep: 3, want: 250},
		{name: "cap hit, deep history exhausted", total: 120, successAt: -1, wantShallow: 5, wantDeep: 2, want: created},
		{name: "cap hit, deep search capped", total: 600, successAt: 550, wantShallow: 5, wantDeep: 5, want: absent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := pagingFixture(t, now, tt.total, tt.successAt)
			h := newHarness(t, gh, "")
			if err := h.c.Poll(context.Background()); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if got := gh.runPageFetch["1/10"]; got != tt.wantShallow {
				t.Errorf("shallow pages = %d, want %d", got, tt.wantShallow)
			}
			if got := gh.runPageFetch["1/100"]; got != tt.wantDeep {
				t.Errorf("deep pages = %d, want %d", got, tt.wantDeep)
			}
			series := h.series(t)
			got, present := series[lastSuccessKey]
			switch tt.want {
			case absent:
				if present {
					t.Errorf("last_success = %v, want no series", got)
				}
			case created:
				if want := float64(pagingCreated(now).Unix()); !present || got != want {
					t.Errorf("last_success = %v (present %v), want created_at %v", got, present, want)
				}
			default:
				want := float64(now.Add(-time.Duration(tt.want+1) * time.Hour).Unix())
				if !present || got != want {
					t.Errorf("last_success = %v (present %v), want %v", got, present, want)
				}
			}
			// A capped search is not an error.
			if series[`gh_exporter_poll_errors_total{kind=parse}`]+series[`gh_exporter_poll_errors_total{kind=404}`] != 0 {
				t.Errorf("errors counted: %v", series)
			}
			assertNoServerSideFilters(t, gh.requests)
		})
	}
}

func TestRunPagingKeepsKnownSuccess(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := pagingFixture(t, now, 80, 3)
	h := newHarness(t, gh, "")
	ctx := context.Background()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	known := h.series(t)[lastSuccessKey]
	if known == 0 {
		t.Fatal("first poll found no success")
	}
	// 60 newer non-success runs bury the success past the shallow cap.
	gh.mu.Lock()
	newer := make([]run, 0, 60)
	for i := range 60 {
		newer = append(newer, ghRun(nightly, int64(200000-i), now.Add(time.Duration(60-i)*time.Minute), "push", "success"))
	}
	gh.runs[1] = append(newer, gh.runs[1]...)
	gh.runPageFetch = map[string]int{}
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.series(t)[lastSuccessKey]; got != known {
		t.Errorf("last_success = %v, want kept %v", got, known)
	}
	// The shallow walk is capped before reaching back to the known success,
	// so one deep page (100 runs) is walked, which reaches it and stops.
	if gh.runPageFetch["1/10"] != 5 || gh.runPageFetch["1/100"] != 1 {
		t.Errorf("pages = %v, want 5 shallow and 1 deep", gh.runPageFetch)
	}

	// The known success's run disappears (deleted) and the newer runs are
	// gone too: paging stops at the first page that reaches back past the
	// known success instead of walking the whole cap.
	gh.mu.Lock()
	var kept []run
	for _, r := range gh.runs[1][60:] {
		if r.Conclusion != "success" {
			kept = append(kept, r)
		}
	}
	gh.runs[1] = kept // index 0..2 newer than the known success, then older
	gh.runPageFetch = map[string]int{}
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if gh.runPageFetch["1/10"] != 1 || gh.runPageFetch["1/100"] != 0 {
		t.Errorf("pages = %v, want 1 shallow (stopped at the known success)", gh.runPageFetch)
	}
	if got := h.series(t)[lastSuccessKey]; got != known {
		t.Errorf("last_success = %v, want kept %v", got, known)
	}
}

func TestDeepSearchCappedThenResolved(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := pagingFixture(t, now, 600, -1)
	h := newHarness(t, gh, "")
	ctx := context.Background()
	for range 2 {
		if err := h.c.Poll(ctx); err != nil {
			t.Fatalf("capped search must not fail the poll: %v", err)
		}
		if _, ok := h.series(t)[lastSuccessKey]; ok {
			t.Fatal("last_success exported while unknown")
		}
	}
	if !h.c.Ready() {
		t.Fatal("not ready")
	}
	// A new scheduled success resolves it.
	gh.mu.Lock()
	gh.runs[1] = append([]run{ghRun(nightly, 300000, now, "schedule", "success")}, gh.runs[1]...)
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.series(t)[lastSuccessKey]; got != float64(now.Unix()) {
		t.Errorf("last_success = %v, want %v", got, now.Unix())
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

	// A branch override on a scheduled workflow re-points last_run_* at the
	// override branch, but the last scheduled success is still searched on
	// the default branch: schedules never fire anywhere else.
	gh.mu.Lock()
	release := ghRun(nightly, 108, now.Add(-30*time.Minute), "push", "success")
	release.HeadBranch = "release"
	gh.runs[1] = append([]run{release}, gh.runs[1]...)
	gh.mu.Unlock()
	h.c.cfg = mustConfig(t, standardConfig+"overrides:\n  - repo: octo-org/nightly\n    workflow: build.yml\n    branch: release\n    grace_factor: 3\n")
	got = poll()
	build = workflowSeries(got, nightly, "build.yml")
	for k := range build {
		if strings.HasPrefix(k, "github_workflow_last_run_") && strings.Contains(k, "branch=main") {
			t.Errorf("old-branch run series survived: %s", k)
		}
		if strings.HasPrefix(k, "github_workflow_last_success_") && strings.Contains(k, "branch=release") {
			t.Errorf("last success tracked on the override branch: %s", k)
		}
	}
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_run_conclusion{branch=release,conclusion=success,event=push,repo=octo-org/nightly,workflow=build.yml}`: 1,
		`github_workflow_last_success_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=build.yml}`:                       float64(now.Add(-26 * time.Hour).Unix()),
		`github_workflow_grace_factor{repo=octo-org/nightly,workflow=build.yml}`:                                                     3,
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
	gh.workflows[nightly] = []workflow{
		{ID: 1, Path: ".github/workflows/bad.yml", State: "active"},
		{ID: 2, Path: ".github/workflows/broken.yml", State: "active"},
		{ID: 3, Path: ".github/workflows/sunday.yml", State: "active"},
	}
	gh.files[nightly] = map[string]string{
		".github/workflows/bad.yml":    badYAML,
		".github/workflows/broken.yml": "on: [\n",
		".github/workflows/sunday.yml": "on:\n  schedule:\n    - cron: '0 4 * * 7'\n",
	}
	gh.runs[1] = []run{ghRun(nightly, 11, time.Now().Add(-time.Hour), "schedule", "failure")}
	h := newHarness(t, gh, "")
	for range 2 {
		if err := h.c.Poll(context.Background()); err != nil {
			t.Fatalf("a broken workflow file must not fail the poll: %v", err)
		}
	}
	got := h.series(t)
	// Counted once per blob SHA, not once per poll. The bad cron is still
	// watched (state, last run) without interval series; invalid YAML is not;
	// day-of-week 7 is Sunday.
	assertSeries(t, got, map[string]float64{
		`gh_exporter_poll_errors_total{kind=parse}`:                                  2,
		`gh_exporter_watched_workflows{}`:                                            2,
		`github_workflow_state{repo=octo-org/nightly,state=active,workflow=bad.yml}`: 1,
		`github_workflow_last_run_conclusion{branch=main,conclusion=failure,event=schedule,repo=octo-org/nightly,workflow=bad.yml}`: 1,
		`github_workflow_expected_interval_seconds{repo=octo-org/nightly,workflow=sunday.yml}`:                                      7 * 86400,
	})
	assertAbsent(t, got,
		`github_workflow_expected_interval_seconds{repo=octo-org/nightly,workflow=bad.yml}`,
		`github_workflow_grace_factor{repo=octo-org/nightly,workflow=bad.yml}`,
		`github_workflow_state{repo=octo-org/nightly,state=active,workflow=broken.yml}`,
	)
	if !h.c.Ready() {
		t.Fatal("not ready")
	}
}

func TestConcurrentGatherDuringPoll(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	h := newHarness(t, gh, standardConfig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			mfs, err := h.reg.Gather()
			if err != nil {
				t.Errorf("Gather: %v", err)
				return
			}
			// Never two conclusion (or url) series for one workflow.
			for _, mf := range mfs {
				name := mf.GetName()
				if name != "github_workflow_last_run_conclusion" && name != "github_workflow_last_run_url_info" {
					continue
				}
				seen := map[string]bool{}
				for _, m := range mf.GetMetric() {
					var repo, wf string
					for _, l := range m.GetLabel() {
						switch l.GetName() {
						case "repo":
							repo = l.GetValue()
						case "workflow":
							wf = l.GetValue()
						}
					}
					if seen[repo+"/"+wf] {
						t.Errorf("two %s series for %s/%s", name, repo, wf)
					}
					seen[repo+"/"+wf] = true
				}
			}
		}
	}()
	conclusions := []string{"failure", "success", "cancelled", "timed_out"}
	for i := range 40 {
		gh.mu.Lock()
		gh.runs[1] = append([]run{ghRun(nightly, int64(1000+i), now.Add(time.Duration(i)*time.Minute), "schedule", conclusions[i%4])}, gh.runs[1]...)
		gh.mu.Unlock()
		if err := h.c.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	<-done
}

func TestRunStopsOnShutdown(t *testing.T) {
	gh := standardFixture(t, time.Now())
	h := newHarness(t, gh, standardConfig)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		h.c.Run(ctx)
		close(stopped)
	}()
	deadline := time.After(5 * time.Second)
	for !h.c.Ready() {
		select {
		case <-deadline:
			t.Fatal("first poll never completed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func TestPollDeadlineCountsAsNetwork(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	gh.repos = append(gh.repos, repository{FullName: "octo-org/other", DefaultBranch: "main"})
	gh.workflows["octo-org/other"] = []workflow{{ID: 50, Path: ".github/workflows/sync.yml", State: "active"}}
	gh.files["octo-org/other"] = map[string]string{".github/workflows/sync.yml": dailyYAML}
	gh.runs[50] = []run{ghRun("octo-org/other", 501, now.Add(-time.Hour), "schedule", "success")}
	h := newHarness(t, gh, standardConfig)
	if err := h.c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := h.series(t)

	// The first workflow's runs hang past the poll deadline. Everything
	// after it (deploy.yml, the whole second repo) is skipped and keeps its
	// previous series, and the overrun is counted and logged exactly once.
	gh.mu.Lock()
	gh.delay["/repos/octo-org/nightly/actions/workflows/1/runs"] = 2 * time.Second
	sent := len(gh.requests)
	gh.mu.Unlock()
	h.c.pollTimeout = 100 * time.Millisecond
	if err := h.c.Poll(context.Background()); err == nil {
		t.Fatal("poll that overran its deadline succeeded")
	}
	got := h.series(t)
	if n := got[`gh_exporter_poll_errors_total{kind=network}`]; n != 1 {
		t.Errorf("network errors = %v, want exactly 1", n)
	}
	if n := strings.Count(h.logs.String(), "poll deadline exceeded"); n != 1 {
		t.Errorf("deadline logged %d times, want 1:\n%s", n, h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "deadline exceeded") {
		t.Errorf("deadline error not logged:\n%s", h.logs.String())
	}
	gh.mu.Lock()
	for _, u := range gh.requests[sent:] {
		if strings.Contains(u.Path, "octo-org/other") || strings.HasSuffix(u.Path, "/workflows/3/runs") {
			t.Errorf("request after the deadline: %s", u)
		}
	}
	gh.mu.Unlock()
	if !mapsEqual(before, got, "gh_exporter_") {
		t.Errorf("series changed on an overrun poll")
	}

	// Shutdown (the parent context ending) is not counted.
	gh.mu.Lock()
	delete(gh.delay, "/repos/octo-org/nightly/actions/workflows/1/runs")
	gh.mu.Unlock()
	beforeShutdown := got[`gh_exporter_poll_errors_total{kind=network}`]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.c.pollTimeout = 0
	if err := h.c.Poll(ctx); err == nil {
		t.Fatal("cancelled poll succeeded")
	}
	if after := h.series(t)[`gh_exporter_poll_errors_total{kind=network}`]; after != beforeShutdown {
		t.Errorf("shutdown counted as an error: %v -> %v", beforeShutdown, after)
	}
}

func TestMissingContentsPermissionWarnsOnce(t *testing.T) {
	gh := standardFixture(t, time.Now())
	h := newHarness(t, gh, standardConfig)
	ctx := context.Background()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	before := h.series(t)

	// The directory starts to 404 while active workflows still point into
	// it (the App lost Contents: read): an error, not an empty directory.
	// The poll fails, the workflows keep their series, and the warning is
	// logged once however often it recurs.
	gh.mu.Lock()
	saved := gh.files[nightly]
	delete(gh.files, nightly)
	gh.mu.Unlock()
	for range 2 {
		if err := h.c.Poll(ctx); err == nil {
			t.Fatal("poll succeeded with the workflow directory hidden")
		}
		if got := h.series(t); !mapsEqual(before, got, "gh_exporter_") {
			t.Fatalf("series changed while the directory was hidden")
		}
	}
	if n := strings.Count(h.logs.String(), "Contents: read"); n != 1 {
		t.Errorf("permission warning logged %d times, want 1", n)
	}
	if got := h.series(t)[`gh_exporter_poll_errors_total{kind=404}`]; got != 2 {
		t.Errorf("404 errors = %v, want 2", got)
	}

	// Readable again, then hidden again: the warning fires anew.
	gh.mu.Lock()
	gh.files[nightly] = saved
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	delete(gh.files, nightly)
	gh.mu.Unlock()
	_ = h.c.Poll(ctx)
	if n := strings.Count(h.logs.String(), "Contents: read"); n != 2 {
		t.Errorf("permission warning logged %d times after a recovery, want 2", n)
	}
}

// TestMissingDirWithOnlyInactiveWorkflows: when no active workflow points
// into the directory, a 404 is a genuinely empty directory, not an error.
func TestMissingDirWithOnlyInactiveWorkflows(t *testing.T) {
	gh := standardFixture(t, time.Now())
	for i := range gh.workflows[nightly] {
		gh.workflows[nightly][i].State = "disabled_manually"
	}
	delete(gh.files, nightly)
	h := newHarness(t, gh, "")
	if err := h.c.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got := h.series(t)[`gh_exporter_watched_workflows{}`]; got != 0 {
		t.Errorf("watched = %v, want 0", got)
	}
}

func TestBuriedNewerSuccessFoundByDeepWalk(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := pagingFixture(t, now, 30, 3)
	h := newHarness(t, gh, "")
	ctx := context.Background()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	// Between polls: a new scheduled success, then more than 50
	// non-qualifying runs on top of it.
	gh.mu.Lock()
	fresh := ghRun(nightly, 300000, now.Add(time.Hour), "schedule", "success")
	var top []run
	for i := range 70 {
		top = append(top, ghRun(nightly, int64(400000-i), now.Add(3*time.Hour-time.Duration(i)*time.Minute), "push", "success"))
	}
	gh.runs[1] = append(append(top, fresh), gh.runs[1]...)
	gh.runPageFetch = map[string]int{}
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.series(t)[lastSuccessKey]; got != float64(fresh.CreatedAt.Unix()) {
		t.Errorf("last_success = %v, want the buried newer success %v", got, fresh.CreatedAt.Unix())
	}
	if gh.runPageFetch["1/10"] != 5 || gh.runPageFetch["1/100"] != 1 {
		t.Errorf("pages = %v, want 5 shallow and 1 deep", gh.runPageFetch)
	}
}

// TestBranchRepointKeepsSeriesUntilListingSucceeds: the old branch's series
// are only replaced once the new branch's runs have been listed.
func TestBranchRepointKeepsSeriesUntilListingSucceeds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	h := newHarness(t, gh, standardConfig)
	ctx := context.Background()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	oldKey := `github_workflow_last_run_conclusion{branch=main,conclusion=cancelled,event=schedule,repo=octo-org/nightly,workflow=build.yml}`

	gh.mu.Lock()
	gh.fail["/repos/octo-org/nightly/actions/workflows/1/runs"] = http.StatusBadGateway
	gh.mu.Unlock()
	h.c.cfg = mustConfig(t, standardConfig+"overrides:\n  - repo: octo-org/nightly\n    workflow: build.yml\n    branch: release\n")
	if err := h.c.Poll(ctx); err == nil {
		t.Fatal("want error")
	}
	if _, ok := h.series(t)[oldKey]; !ok {
		t.Fatal("old-branch series deleted before the new listing succeeded")
	}

	gh.mu.Lock()
	delete(gh.fail, "/repos/octo-org/nightly/actions/workflows/1/runs")
	rel := ghRun(nightly, 109, now.Add(-time.Minute), "push", "failure")
	rel.HeadBranch = "release"
	gh.runs[1] = append([]run{rel}, gh.runs[1]...)
	gh.mu.Unlock()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	got := h.series(t)
	if _, ok := got[oldKey]; ok {
		t.Error("old-branch series survived a successful re-point")
	}
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_run_conclusion{branch=release,conclusion=failure,event=push,repo=octo-org/nightly,workflow=build.yml}`: 1,
	})
}

// TestOverrideBranchRunsBeyondFirstPage: a default-branch scheduled success
// on page 1 must not stop paging before the override branch's latest run.
func TestOverrideBranchRunsBeyondFirstPage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := pagingFixture(t, now, 30, 0) // scheduled success is the newest run
	rel := ghRun(nightly, 1, now.Add(-15*time.Hour-30*time.Minute), "push", "failure")
	rel.HeadBranch = "release"
	gh.runs[1] = append(gh.runs[1][:15], append([]run{rel}, gh.runs[1][15:]...)...) // index 15: page 2
	h := newHarness(t, gh, "overrides:\n  - repo: octo-org/nightly\n    workflow: build.yml\n    branch: release\n")
	if err := h.c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := h.series(t)
	assertSeries(t, got, map[string]float64{
		`github_workflow_last_run_conclusion{branch=release,conclusion=failure,event=push,repo=octo-org/nightly,workflow=build.yml}`: 1,
		lastSuccessKey: float64(now.Add(-time.Hour).Unix()),
	})
	if gh.runPageFetch["1/10"] != 2 {
		t.Errorf("shallow pages = %d, want 2", gh.runPageFetch["1/10"])
	}
}

func TestLogsNeverContainSecrets(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	gh := standardFixture(t, now)
	h := newHarness(t, gh, standardConfig)
	ctx := context.Background()
	if err := h.c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	// Exercise the error paths too: every request rejected.
	gh.mu.Lock()
	gh.fail["/installation/repositories"] = http.StatusUnauthorized
	gh.fail["/repos/octo-org/nightly/actions/workflows"] = http.StatusInternalServerError
	gh.mu.Unlock()
	_ = h.c.Poll(ctx)
	logs := h.logs.String()
	if logs == "" {
		t.Fatal("no logs captured")
	}
	for _, secret := range []string{fakeToken, "PRIVATE KEY", string(testKeyPEM[40:80])} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain secret material %q", secret)
		}
	}
}

// TestLastSuccessOnExhaustedHistory: what last_success reports when the run
// history holds no qualifying success.
func TestLastSuccessOnExhaustedHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	const deployKey = `github_workflow_last_success_timestamp_seconds{branch=main,repo=octo-org/nightly,workflow=deploy.yml}`
	createdAt := pagingCreated(now)
	tests := []struct {
		name string
		// setup builds the fixture; prime, if set, runs a first poll and then
		// mutates the fixture before the poll under test.
		setup  func(t *testing.T) (*fakeGitHub, string)
		prime  func(gh *fakeGitHub)
		key    string
		want   float64
		absent bool
	}{
		{
			name:  "scheduled, no success ever: created_at",
			setup: func(t *testing.T) (*fakeGitHub, string) { return pagingFixture(t, now, 25, -1), "" },
			key:   lastSuccessKey,
			want:  float64(createdAt.Unix()),
		},
		{
			name:  "scheduled, known earlier success wins over created_at",
			setup: func(t *testing.T) (*fakeGitHub, string) { return pagingFixture(t, now, 25, 20), "" },
			prime: func(gh *fakeGitHub) {
				// The success's run is deleted: the history is exhausted
				// without one, but the known success stands.
				gh.runs[1] = pagingRuns(now, 0, 20, -1)
			},
			key:  lastSuccessKey,
			want: float64(now.Add(-21 * time.Hour).Unix()),
		},
		{
			name:   "deep search capped, nothing known: absent",
			setup:  func(t *testing.T) (*fakeGitHub, string) { return pagingFixture(t, now, 600, -1), "" },
			key:    lastSuccessKey,
			absent: true,
		},
		{
			name: "include-list, no success ever: 0",
			setup: func(t *testing.T) (*fakeGitHub, string) {
				gh := newFakeGitHub(t)
				gh.repos = []repository{{FullName: nightly, DefaultBranch: "main"}}
				gh.workflows[nightly] = []workflow{{ID: 3, Path: ".github/workflows/deploy.yml", State: "active",
					CreatedAt: createdAt.Format(time.RFC3339)}}
				gh.files[nightly] = map[string]string{".github/workflows/deploy.yml": deployYAML}
				gh.runs[3] = []run{ghRun(nightly, 301, now.Add(-time.Hour), "push", "failure")}
				return gh, standardConfig
			},
			key:  deployKey,
			want: 0,
		},
		{
			name: "scheduled, created_at missing: 0",
			setup: func(t *testing.T) (*fakeGitHub, string) {
				gh := pagingFixture(t, now, 25, -1)
				gh.workflows[nightly][0].CreatedAt = ""
				return gh, ""
			},
			key:  lastSuccessKey,
			want: 0,
		},
		{
			name: "scheduled, created_at unparsable: 0",
			setup: func(t *testing.T) (*fakeGitHub, string) {
				gh := pagingFixture(t, now, 25, -1)
				gh.workflows[nightly][0].CreatedAt = "last tuesday"
				return gh, ""
			},
			key:  lastSuccessKey,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh, cfg := tt.setup(t)
			h := newHarness(t, gh, cfg)
			ctx := context.Background()
			if tt.prime != nil {
				if err := h.c.Poll(ctx); err != nil {
					t.Fatal(err)
				}
				gh.mu.Lock()
				tt.prime(gh)
				gh.mu.Unlock()
			}
			if err := h.c.Poll(ctx); err != nil {
				t.Fatal(err)
			}
			got, present := h.series(t)[tt.key]
			switch {
			case tt.absent && present:
				t.Errorf("last_success = %v, want no series", got)
			case !tt.absent && (!present || got != tt.want):
				t.Errorf("last_success = %v (present %v), want %v", got, present, tt.want)
			}
		})
	}
}
