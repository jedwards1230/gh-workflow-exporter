// Package cronx derives the expected run interval of a GitHub Actions
// workflow from its `on.schedule[].cron` entries.
//
// GitHub schedules use the standard 5-field cron syntax evaluated in UTC.
// The expected interval is the shortest gap between two consecutive fire
// times across ALL of a workflow's cron entries, which is what a staleness
// alert needs: whatever the cron's shape, a healthy workflow never goes
// longer than a small multiple of that without a run.
package cronx

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// parser accepts exactly the 5-field form GitHub supports (minute, hour,
// day-of-month, month, day-of-week). Descriptors such as @daily are rejected
// because GitHub rejects them too.
var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

const (
	// minGap is the smallest gap a 5-field cron can produce; once found the
	// simulation can stop early.
	minGap = time.Minute
	// window bounds the simulation. Nine years covers every calendar cycle a
	// 5-field cron can express, including leap days (`0 0 29 2 *`).
	window = 9 * 366 * 24 * time.Hour
	// maxFires bounds the work for very frequent crons, whose minimum gap
	// shows up within the first few thousand fires.
	maxFires = 50000
)

// epoch is a fixed simulation start so the result is deterministic and does
// not drift between polls.
var epoch = time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)

// Parse parses a single GitHub cron expression. Runs of whitespace between
// fields are tolerated.
func Parse(expr string) (cron.Schedule, error) {
	norm := strings.Join(strings.Fields(expr), " ")
	if norm == "" {
		return nil, errors.New("empty cron expression")
	}
	s, err := parser.Parse(norm)
	if err != nil {
		return nil, fmt.Errorf("parsing cron %q: %w", expr, err)
	}
	return s, nil
}

// Interval returns the shortest gap between consecutive fire times across all
// of exprs, evaluated in UTC. It returns an error if any expression is
// invalid, or if the expressions together fire fewer than twice in the
// simulation window (so no gap exists).
func Interval(exprs []string) (time.Duration, error) {
	if len(exprs) == 0 {
		return 0, errors.New("no cron expressions")
	}
	scheds := make([]cron.Schedule, 0, len(exprs))
	for _, e := range exprs {
		s, err := Parse(e)
		if err != nil {
			return 0, err
		}
		scheds = append(scheds, s)
	}

	end := epoch.Add(window)
	// next[i] is the upcoming fire time of schedule i (zero = never fires).
	next := make([]time.Time, len(scheds))
	for i, s := range scheds {
		next[i] = s.Next(epoch.Add(-time.Second))
	}

	var (
		best  time.Duration
		prev  time.Time
		fires int
	)
	for fires < maxFires {
		// k-way merge: take the earliest pending fire.
		idx := -1
		for i, t := range next {
			if t.IsZero() || t.After(end) {
				continue
			}
			if idx == -1 || t.Before(next[idx]) {
				idx = i
			}
		}
		if idx == -1 {
			break
		}
		t := next[idx]
		// Advance every schedule sitting on this instant so identical fires
		// from different entries collapse into one.
		for i := range next {
			if next[i].Equal(t) {
				next[i] = scheds[i].Next(t)
			}
		}
		if !prev.IsZero() {
			if gap := t.Sub(prev); best == 0 || gap < best {
				best = gap
				if best <= minGap {
					break
				}
			}
		}
		prev = t
		fires++
	}
	if best == 0 {
		return 0, fmt.Errorf("cron %q fires fewer than twice in the simulation window", strings.Join(exprs, "; "))
	}
	return best, nil
}
