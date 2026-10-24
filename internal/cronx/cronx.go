// Package cronx derives the expected run interval of a GitHub Actions
// workflow from its `on.schedule[].cron` entries.
//
// GitHub schedules use the standard 5-field cron syntax evaluated in UTC.
// The expected interval is the LONGEST gap between consecutive fires of the
// merged fire sequence of ALL a workflow's cron entries. That is the longest
// a healthy workflow can legitimately go without a run, so a staleness alert
// built on it never fires on a quiet stretch the schedule itself contains
// (for example a weekdays-only cron over a weekend).
package cronx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// parser accepts exactly the 5-field form GitHub supports (minute, hour,
// day-of-month, month, day-of-week). Descriptors such as @daily are rejected
// because GitHub rejects them too.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

const (
	// window bounds the simulation. Nine years covers every calendar cycle a
	// 5-field cron can express within its gaps, including leap days
	// (`0 0 29 2 *` fires every four years).
	window = 9 * 366 * 24 * time.Hour
	// MinInterval is GitHub's shortest supported schedule interval; more
	// frequent crons are throttled to it, so the result is clamped up to it.
	MinInterval = 5 * time.Minute
	// starBit marks a field written as `*` (or `?`) in robfig's SpecSchedule.
	starBit       = 1 << 63
	minutesPerDay = 24 * 60
)

// epoch is a fixed simulation start so the result is deterministic.
var epoch = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// Parse parses a single GitHub cron expression. Runs of whitespace between
// fields are tolerated, and day-of-week 7 is accepted as Sunday (as GitHub
// and most crons do; robfig/cron only knows 0-6).
func Parse(expr string) (*cron.SpecSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) == 0 {
		return nil, errors.New("empty cron expression")
	}
	if len(fields) == 5 {
		dow, err := normalizeDOW(fields[4])
		if err != nil {
			return nil, fmt.Errorf("parsing cron %q: %w", expr, err)
		}
		fields[4] = dow
	}
	s, err := parser.Parse(strings.Join(fields, " "))
	if err != nil {
		return nil, fmt.Errorf("parsing cron %q: %w", expr, err)
	}
	spec, ok := s.(*cron.SpecSchedule)
	if !ok {
		return nil, fmt.Errorf("parsing cron %q: unexpected schedule type %T", expr, s)
	}
	// robfig defaults to time.Local; GitHub evaluates schedules in UTC.
	spec.Location = time.UTC
	return spec, nil
}

// normalizeDOW rewrites day-of-week 7 to 0. Any list element that mentions
// 7 as a number (`7`, `5-7`, `1-7/2`) is expanded to explicit values; other
// elements (names, `*`, `*/2`, ranges below 7) pass through unchanged.
func normalizeDOW(field string) (string, error) {
	parts := strings.Split(field, ",")
	for i, p := range parts {
		rng, stepStr, hasStep := strings.Cut(p, "/")
		lo, hi, isRange := strings.Cut(rng, "-")
		if !isRange {
			hi = lo
		}
		if lo != "7" && hi != "7" {
			continue
		}
		a, errA := strconv.Atoi(lo)
		b, errB := strconv.Atoi(hi)
		if errA != nil || errB != nil {
			continue // leave it for the parser to reject
		}
		if hasStep && !isRange {
			b = 7 // "N/step" means N through the max
		}
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return "", fmt.Errorf("invalid day-of-week step in %q", p)
			}
			step = n
		}
		if a > b || b > 7 {
			return "", fmt.Errorf("invalid day-of-week range %q", p)
		}
		var vals []string
		for d := a; d <= b; d += step {
			vals = append(vals, strconv.Itoa(d%7))
		}
		parts[i] = strings.Join(vals, ",")
	}
	return strings.Join(parts, ","), nil
}

// Interval returns the longest gap between consecutive fires of the merged
// fire sequence of exprs, evaluated in UTC over a nine-year window, clamped
// to at least MinInterval. It errors if any expression is invalid or the
// expressions together fire fewer than twice in the window.
//
// Day matching follows robfig/cron (and Vixie cron): when both day-of-month
// and day-of-week are restricted the day matches if EITHER does; if either
// is `*` both must. A stepped wildcard such as `*/2` counts as restricted.
func Interval(exprs []string) (time.Duration, error) {
	if len(exprs) == 0 {
		return 0, errors.New("no cron expressions")
	}
	type entry struct {
		spec *cron.SpecSchedule
		// minutes of the day the cron fires on a matching day, ascending.
		minutes []int
	}
	entries := make([]entry, 0, len(exprs))
	for _, e := range exprs {
		s, err := Parse(e)
		if err != nil {
			return 0, err
		}
		var mins []int
		for h := range 24 {
			if s.Hour&bit(h) == 0 {
				continue
			}
			for m := range 60 {
				if s.Minute&bit(m) != 0 {
					mins = append(mins, h*60+m)
				}
			}
		}
		entries = append(entries, entry{spec: s, minutes: mins})
	}

	// Every cron fires at the same minutes on each day it matches, so the
	// merged sequence is walked a day at a time: exact over the window and
	// cheap (days × minutes) even for every-minute crons.
	var (
		fireAt  [minutesPerDay]bool
		longest time.Duration
		prev    time.Time
		fires   int
	)
	days := int(window / (24 * time.Hour))
	for d := range days {
		day := epoch.AddDate(0, 0, d)
		clear(fireAt[:])
		matched := false
		for _, e := range entries {
			if dayMatches(e.spec, day) {
				for _, m := range e.minutes {
					fireAt[m] = true
				}
				matched = true
			}
		}
		if !matched {
			continue
		}
		for m, on := range fireAt {
			if !on {
				continue
			}
			t := day.Add(time.Duration(m) * time.Minute)
			if fires > 0 {
				if gap := t.Sub(prev); gap > longest {
					longest = gap
				}
			}
			prev = t
			fires++
		}
	}
	if fires < 2 {
		return 0, fmt.Errorf("cron %q fires fewer than twice in the simulation window", strings.Join(exprs, "; "))
	}
	return max(longest, MinInterval), nil
}

// dayMatches mirrors robfig/cron's day matching (month, then DOM/DOW).
func dayMatches(s *cron.SpecSchedule, t time.Time) bool {
	if s.Month&bit(int(t.Month())) == 0 {
		return false
	}
	dom := s.Dom&bit(t.Day()) != 0
	dow := s.Dow&bit(int(t.Weekday())) != 0
	if s.Dom&starBit != 0 || s.Dow&starBit != 0 {
		return dom && dow
	}
	return dom || dow
}

// bit returns the SpecSchedule bitmask bit for a field value (all in 0..31).
func bit(n int) uint64 {
	if n < 0 || n > 63 {
		return 0
	}
	return uint64(1) << n
}
