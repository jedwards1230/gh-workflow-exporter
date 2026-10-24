package cronx

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

func TestInterval(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		name    string
		exprs   []string
		want    time.Duration
		wantErr bool
	}{
		{name: "hourly", exprs: []string{"0 * * * *"}, want: time.Hour},
		{name: "daily", exprs: []string{"30 4 * * *"}, want: day},
		{name: "every 15 minutes", exprs: []string{"*/15 * * * *"}, want: 15 * time.Minute},
		{name: "every minute clamps to 5m", exprs: []string{"* * * * *"}, want: MinInterval},
		{name: "every 2 minutes clamps to 5m", exprs: []string{"*/2 * * * *"}, want: MinInterval},
		{name: "uneven hours take the long gap", exprs: []string{"0 6,8 * * *"}, want: 22 * time.Hour},
		{name: "weekly by day of week", exprs: []string{"0 6 * * 1"}, want: 7 * day},
		{name: "weekly by day name", exprs: []string{"0 6 * * MON"}, want: 7 * day},
		{name: "weekdays span the weekend", exprs: []string{"0 9 * * 1-5"}, want: 72 * time.Hour},
		{name: "1st and 15th", exprs: []string{"0 0 1,15 * *"}, want: 17 * day},
		{name: "monthly", exprs: []string{"0 0 1 * *"}, want: 31 * day},
		{name: "extra whitespace", exprs: []string{"  0   3  * *   * "}, want: day},
		{
			name:  "merged sequence shorter than either cron alone",
			exprs: []string{"0 0 * * 1", "0 0 * * 4"}, // Mon and Thu: gaps 3d and 4d
			want:  4 * day,
		},
		{name: "duplicate crons collapse", exprs: []string{"0 0 * * *", "0 0 * * *"}, want: day},
		{name: "leap day", exprs: []string{"0 0 29 2 *"}, want: (4*365 + 1) * day},
		// DOW 7 is Sunday.
		{name: "dow 7", exprs: []string{"0 0 * * 7"}, want: 7 * day},
		{name: "dow range ending in 7", exprs: []string{"0 0 * * 5-7"}, want: 5 * day}, // Fri, Sat, Sun
		{name: "dow list with 7", exprs: []string{"0 0 * * 5,7"}, want: 5 * day},       // Sun -> Fri
		{name: "dow 1-7 is every day", exprs: []string{"0 0 * * 1-7"}, want: day},
		// DOM and DOW both restricted: a day matches if EITHER matches, so the
		// 1st of the month adds fires between Mondays.
		{name: "dom or dow", exprs: []string{"0 0 1 * 1"}, want: 7 * day},
		// robfig treats a stepped wildcard as restricted, so `*/10` DOM with a
		// pinned DOW is also OR: days 1,11,21,31 plus every Monday.
		{name: "stepped dom with pinned dow is OR", exprs: []string{"0 0 */10 * 1"}, want: 7 * day},
		// With DOM a plain `*`, DOW alone decides (AND with an all-days DOM).
		{name: "star dom with dow is AND", exprs: []string{"0 0 * * 3"}, want: 7 * day},
		{name: "invalid field count", exprs: []string{"0 0 * *"}, wantErr: true},
		{name: "invalid value", exprs: []string{"61 * * * *"}, wantErr: true},
		{name: "dow 8", exprs: []string{"0 0 * * 8"}, wantErr: true},
		{name: "descriptor rejected", exprs: []string{"@daily"}, wantErr: true},
		{name: "one invalid among valid", exprs: []string{"0 0 * * *", "bogus"}, wantErr: true},
		{name: "blank", exprs: []string{"   "}, wantErr: true},
		{name: "none", exprs: nil, wantErr: true},
		{name: "never fires", exprs: []string{"0 0 30 2 *"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Interval(tt.exprs)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Interval(%q) = %v, want error", tt.exprs, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Interval(%q): %v", tt.exprs, err)
			}
			if got != tt.want {
				t.Fatalf("Interval(%q) = %v, want %v", tt.exprs, got, tt.want)
			}
		})
	}
}

// TestDayMatchingAgreesWithRobfig cross-checks the day-at-a-time walk against
// robfig's own Next() for a few shapes over the first year.
//
// The late-evening crons land on another UTC day if a schedule is evaluated
// in any zone but UTC, which the EST-zoned input below would expose.
func TestDayMatchingAgreesWithRobfig(t *testing.T) {
	for _, expr := range []string{"0 0 1 * 1", "0 0 */10 * 1", "0 9 * * 1-5", "15 3 1,15 * *", "0 0 * 2 *", "30 23 * * 1", "45 23 28 * *"} {
		s, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if s.Location != time.UTC {
			t.Fatalf("%s: location %v, want UTC", expr, s.Location)
		}
		var sched cron.Schedule = s
		// Feed robfig a non-UTC time: a schedule left at time.Local would be
		// evaluated in the zone of the time passed in.
		est := time.FixedZone("EST", -5*3600)
		next := sched.Next(epoch.Add(-time.Second).In(est))
		for d := range 366 {
			day := epoch.AddDate(0, 0, d)
			want := !next.IsZero() && next.Before(day.Add(24*time.Hour)) && !next.Before(day)
			if got := dayMatches(s, day); got != want {
				t.Fatalf("%s on %s: dayMatches=%v, robfig fires=%v", expr, day.Format("2006-01-02"), got, want)
			}
			for !next.IsZero() && next.Before(day.Add(24*time.Hour)) {
				next = sched.Next(next)
			}
		}
	}
}

func BenchmarkIntervalEveryMinute(b *testing.B) {
	for b.Loop() {
		if _, err := Interval([]string{"* * * * *"}); err != nil {
			b.Fatal(err)
		}
	}
}
