package cronx

import (
	"testing"
	"time"
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
		{name: "every minute", exprs: []string{"* * * * *"}, want: time.Minute},
		{name: "weekly by day of week", exprs: []string{"0 6 * * 1"}, want: 7 * day},
		{name: "weekly by day name", exprs: []string{"0 6 * * MON"}, want: 7 * day},
		{name: "weekdays", exprs: []string{"0 9 * * 1-5"}, want: day},
		{name: "1st and 15th", exprs: []string{"0 0 1,15 * *"}, want: 14 * day},
		{name: "monthly", exprs: []string{"0 0 1 * *"}, want: 28 * day},
		{name: "extra whitespace", exprs: []string{"  0   3  * *   * "}, want: day},
		{name: "min gap spans two crons", exprs: []string{"0 2 * * *", "0 3 * * *"}, want: time.Hour},
		{name: "second cron narrows weekly", exprs: []string{"0 0 * * 0", "0 12 * * 0"}, want: 12 * time.Hour},
		{name: "duplicate crons collapse", exprs: []string{"0 0 * * *", "0 0 * * *"}, want: day},
		{name: "leap day", exprs: []string{"0 0 29 2 *"}, want: (4*365 + 1) * day},
		{name: "invalid field count", exprs: []string{"0 0 * *"}, wantErr: true},
		{name: "invalid value", exprs: []string{"61 * * * *"}, wantErr: true},
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
