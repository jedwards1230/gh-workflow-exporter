package collector

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// TestParseSchedulesPrefersOnKey: with both a literal `on` and a YAML 1.1
// `true` key, `on` wins every time regardless of map iteration order.
func TestParseSchedulesPrefersOnKey(t *testing.T) {
	doc := []byte("true:\n  schedule:\n    - cron: '1 1 * * *'\non:\n  schedule:\n    - cron: '2 2 * * *'\n")
	for range 50 {
		got, err := parseSchedules(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, []string{"2 2 * * *"}) {
			t.Fatalf("got %q, want the `on` key's cron", got)
		}
	}
}

func TestParseSchedules(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    []string
		wantErr bool
	}{
		{name: "map with schedule", yaml: "on:\n  schedule:\n    - cron: '0 3 * * *'\n    - cron: \"30 */6 * * *\"\n  workflow_dispatch:\n", want: []string{"0 3 * * *", "30 */6 * * *"}},
		{name: "string on", yaml: "on: push\n"},
		{name: "list on", yaml: "on: [push, workflow_dispatch]\n"},
		{name: "map without schedule", yaml: "on:\n  push:\n    branches: [main]\n"},
		{name: "yaml 1.1 true key", yaml: "true:\n  schedule:\n    - cron: '0 0 * * 0'\n", want: []string{"0 0 * * 0"}},
		{name: "quoted on key", yaml: "\"on\":\n  schedule:\n    - cron: '15 * * * *'\n", want: []string{"15 * * * *"}},
		{name: "empty schedule", yaml: "on:\n  schedule:\n"},
		{name: "empty file", yaml: ""},
		{name: "no on", yaml: "name: x\njobs: {}\n"},
		{
			name: "anchor and alias",
			yaml: "x-crons: &nightly\n  - cron: '0 3 * * *'\non:\n  schedule: *nightly\n",
			want: []string{"0 3 * * *"},
		},
		{
			name: "merge key",
			yaml: "x-on: &base\n  schedule:\n    - cron: '0 6 * * 1'\non:\n  <<: *base\n  push: {}\n",
			want: []string{"0 6 * * 1"},
		},
		{
			name: "aliased cron scalar",
			yaml: "x: &c '15 2 * * *'\non:\n  schedule:\n    - cron: *c\n",
			want: []string{"15 2 * * *"},
		},
		{name: "schedule not a list", yaml: "on:\n  schedule: '0 0 * * *'\n", wantErr: true},
		{name: "entry without cron", yaml: "on:\n  schedule:\n    - foo: bar\n", wantErr: true},
		{name: "invalid yaml", yaml: "on: [\n", wantErr: true},
		{name: "not a mapping", yaml: "- a\n- b\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSchedules([]byte(tt.yaml))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// mk builds a completed run on main in octo-org/nightly, hoursAgo before t0.
func mk(id int64, hoursAgo int, event, conclusion string) run {
	r := run{
		ID: id, Status: "completed", Conclusion: conclusion, Event: event, HeadBranch: "main",
		CreatedAt: t0.Add(-time.Duration(hoursAgo) * time.Hour),
		HTMLURL:   fmt.Sprintf("https://github.com/octo-org/nightly/actions/runs/%d", id),
	}
	r.HeadRepository = &struct {
		FullName string `json:"full_name"`
	}{"octo-org/nightly"}
	return r
}

func with(r run, f func(*run)) run { f(&r); return r }

func TestSelector(t *testing.T) {
	fork := func(r *run) { r.HeadRepository.FullName = "someone/nightly" }
	tests := []struct {
		name        string
		scheduled   bool
		runs        []run
		wantLatest  int64 // 0 = none
		wantSuccess int64 // 0 = none
	}{
		{
			name:      "cancel streak keeps older success",
			scheduled: true,
			runs: []run{
				mk(5, 1, "schedule", "cancelled"), mk(4, 25, "schedule", "cancelled"),
				mk(3, 49, "schedule", "cancelled"), mk(2, 73, "schedule", "success"),
			},
			wantLatest: 5, wantSuccess: 2,
		},
		{
			name:      "pull request runs ignored",
			scheduled: true,
			runs: []run{
				mk(9, 1, "pull_request", "failure"), mk(8, 2, "pull_request_target", "success"),
				mk(7, 3, "schedule", "success"),
			},
			wantLatest: 7, wantSuccess: 7,
		},
		{
			name:       "fork run on same-named branch ignored",
			scheduled:  true,
			runs:       []run{with(mk(9, 1, "push", "failure"), fork), mk(7, 3, "schedule", "success")},
			wantLatest: 7, wantSuccess: 7,
		},
		{
			name:      "other branch and in-progress ignored",
			scheduled: true,
			runs: []run{
				with(mk(9, 1, "schedule", ""), func(r *run) { r.Status = "in_progress" }),
				with(mk(8, 2, "schedule", "failure"), func(r *run) { r.HeadBranch = "dev" }),
				mk(7, 3, "schedule", "failure"),
			},
			wantLatest: 7,
		},
		{
			name:       "manual success does not count for scheduled workflow",
			scheduled:  true,
			runs:       []run{mk(9, 1, "workflow_dispatch", "success"), mk(8, 30, "schedule", "failure")},
			wantLatest: 9,
		},
		{
			name:       "include-list counts any-event success",
			scheduled:  false,
			runs:       []run{mk(9, 1, "push", "failure"), mk(8, 30, "workflow_dispatch", "success")},
			wantLatest: 9, wantSuccess: 8,
		},
		{
			name:       "order independent",
			scheduled:  true,
			runs:       []run{mk(1, 50, "schedule", "success"), mk(3, 2, "schedule", "failure"), mk(2, 26, "schedule", "success")},
			wantLatest: 3, wantSuccess: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &selector{repo: "Octo-Org/Nightly", runBranch: "main", successBranch: "main", scheduled: tt.scheduled}
			s.add(tt.runs)
			if id := idOf(s.latest); id != tt.wantLatest {
				t.Errorf("latest = %d, want %d", id, tt.wantLatest)
			}
			if id := idOf(s.success); id != tt.wantSuccess {
				t.Errorf("success = %d, want %d", id, tt.wantSuccess)
			}
			if s.done() != (tt.wantSuccess != 0) {
				t.Errorf("done = %v", s.done())
			}
		})
	}
}

func idOf(r *run) int64 {
	if r == nil {
		return 0
	}
	return r.ID
}

func TestObservationNoGoingBackwards(t *testing.T) {
	newest := mk(10, 1, "schedule", "failure")
	older := mk(9, 25, "schedule", "success")
	rerun := mk(10, 1, "schedule", "success") // same run, re-run to success

	var o observation
	o.merge(&selector{latest: &newest, success: nil})
	if o.lastRun.ID != 10 || !o.lastSuccess.IsZero() {
		t.Fatalf("after first merge: %+v", o)
	}
	// A lagging listing whose newest run is older must not regress lastRun,
	// though its success is still newer than what we had.
	o.merge(&selector{latest: &older, success: &older})
	if o.lastRun.ID != 10 || o.lastRun.Conclusion != "failure" {
		t.Fatalf("lastRun went backwards: %+v", o.lastRun)
	}
	if !o.lastSuccess.Equal(older.CreatedAt) {
		t.Fatalf("lastSuccess = %v", o.lastSuccess)
	}
	// A re-run of the same run updates the conclusion.
	o.merge(&selector{latest: &rerun, success: &rerun})
	if o.lastRun.Conclusion != "success" || !o.lastSuccess.Equal(rerun.CreatedAt) {
		t.Fatalf("re-run not applied: %+v", o)
	}
	// An older success never moves lastSuccess back; an empty selection
	// keeps everything.
	o.merge(&selector{success: &older})
	o.merge(&selector{})
	if !o.lastSuccess.Equal(rerun.CreatedAt) || o.lastRun.ID != 10 {
		t.Fatalf("observation regressed: %+v", o)
	}
}
