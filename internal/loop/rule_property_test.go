// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package loop

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// ruleGen draws random RRULEs: allowed ones, from the whole allowlist
// (including filters that never match), and disallowed ones, an allowed rule
// with one feature from outside it.
type ruleGen struct{ r *rand.Rand }

var freqs = []string{"MINUTELY", "HOURLY", "DAILY", "WEEKLY", "MONTHLY", "YEARLY"}

func (g ruleGen) pick(xs ...string) string { return xs[g.r.Intn(len(xs))] }

// list draws 1..n distinct values from lo..hi.
func (g ruleGen) list(n, lo, hi int) string {
	seen := map[int]bool{}
	var vals []string
	for range 1 + g.r.Intn(n) {
		v := lo + g.r.Intn(hi-lo+1)
		if !seen[v] {
			seen[v] = true
			vals = append(vals, fmt.Sprint(v))
		}
	}
	return strings.Join(vals, ",")
}

func (g ruleGen) interval() int {
	switch g.r.Intn(4) {
	case 0:
		return 1
	case 1:
		return 1 + g.r.Intn(10)
	case 2:
		return []int{30, 90, 1440, 10080, 99999}[g.r.Intn(5)]
	default:
		return 1 + g.r.Intn(maxInterval)
	}
}

func (g ruleGen) allowed(now time.Time) string {
	freq := g.pick(freqs...)
	parts := []string{"FREQ=" + freq}
	if g.r.Intn(2) == 0 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", g.interval()))
	}
	switch g.r.Intn(4) {
	case 0:
		parts = append(parts, fmt.Sprintf("COUNT=%d", 1+g.r.Intn(1000)))
	case 1:
		parts = append(parts, "UNTIL="+now.Add(time.Duration(g.r.Intn(1000))*time.Hour).UTC().Format(icalUTC))
	}
	day := freq != "MINUTELY" && freq != "HOURLY"
	byMonth := day && g.r.Intn(2) == 0
	if byMonth {
		parts = append(parts, "BYMONTH="+g.list(4, 1, 12))
	}
	if day && g.r.Intn(2) == 0 {
		days := strings.Split(g.list(4, 1, 31), ",")
		for i := range days {
			if g.r.Intn(2) == 0 {
				days[i] = "-" + days[i] // from the month's end
			}
		}
		parts = append(parts, "BYMONTHDAY="+strings.Join(days, ","))
	}
	if day && g.r.Intn(2) == 0 {
		ord := ""
		if freq == "MONTHLY" || freq == "YEARLY" {
			ords := []string{"", "1", "-1", "2", "5"}
			if freq == "YEARLY" && !byMonth {
				ords = append(ords, "53", "-53", "20")
			}
			ord = g.pick(ords...)
		}
		parts = append(parts, "BYDAY="+ord+g.pick("TU", "TH", "SA", "SU")+","+ord+g.pick("MO", "WE", "FR"))
	}
	if g.r.Intn(2) == 0 {
		parts = append(parts, "BYHOUR="+g.list(3, 0, 23))
	}
	if g.r.Intn(2) == 0 {
		parts = append(parts, "BYMINUTE="+g.list(3, 0, 59))
	}
	if g.r.Intn(4) == 0 {
		parts = append(parts, "BYSECOND=0")
	}
	return "RRULE:" + strings.Join(parts, ";")
}

func (g ruleGen) disallowed(now time.Time) string {
	rule := g.allowed(now)
	switch g.r.Intn(14) {
	case 11:
		return "RRULE:FREQ=" + g.pick("DAILY", "WEEKLY") + ";BYHOUR=9,0" + g.pick("9", "09", "+9")
	case 12:
		return "RRULE:FREQ=MINUTELY;BYSECOND=0," + g.pick("00", "-0", "+0")
	case 13:
		return "RRULE:FREQ=HOURLY;COUNT=" + g.pick("0", "-1", "5;UNTIL=20300101T000000Z")
	case 9:
		return "RRULE:FREQ=" + g.pick("DAILY", "WEEKLY", "MONTHLY") + ";BYHOUR=3,9,3"
	case 10:
		return "RRULE:FREQ=" + g.pick("MONTHLY", "YEARLY;BYMONTH=2") + ";BYDAY=" + g.pick("6", "-6", "53", "20") + "MO"
	case 0:
		return rule + ";BYSETPOS=" + g.list(3, -10, 10)
	case 1:
		return rule + ";BYYEARDAY=" + g.list(3, 1, 366)
	case 2:
		return rule + ";BYWEEKNO=" + g.list(3, 1, 53)
	case 3:
		return rule + ";BYEASTER=0"
	case 4:
		return strings.Replace(rule, "FREQ=", "FREQ=SECONDLY;X-OLD=", 1)
	case 5:
		return rule + fmt.Sprintf(";INTERVAL=%d", maxInterval+1+g.r.Intn(1<<40))
	case 6:
		return "RRULE:FREQ=" + g.pick("MINUTELY", "HOURLY") + ";BYMONTH=2;BYMONTHDAY=30;BYHOUR=" + g.list(3, 0, 23)
	case 7:
		return rule + ";BYSECOND=" + g.list(3, 1, 59)
	default:
		return rule + ";BYMINUTE=" + strings.Repeat("1,", maxListLen) + "1"
	}
}

// within runs f, failing the test if it panics or takes longer than limit (its
// fastest of up to three runs: f must be repeatable).
func within(t *testing.T, limit time.Duration, what string, f func() error) (time.Duration, error) {
	t.Helper()
	type result struct {
		err error
		d   time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				done <- result{fmt.Errorf("panic: %v", p), time.Since(start)}
			}
		}()
		err := f()
		done <- result{err, time.Since(start)}
	}()
	select {
	case r := <-done:
		if r.err != nil && strings.HasPrefix(r.err.Error(), "panic:") {
			t.Errorf("%s: %v", what, r.err)
		}
		// Over the limit: time it again, twice at most, and count the fastest.
		// A busy machine can deschedule one run for a while, but a rule that
		// really walks too far is slow every time.
		panicked := r.err != nil && strings.HasPrefix(r.err.Error(), "panic:")
		for retry := 0; retry < 2 && r.d > limit && !panicked; retry++ {
			start := time.Now()
			_ = f()
			r.d = min(r.d, time.Since(start))
		}
		if r.d > limit {
			t.Errorf("%s took %s; the limit is %s", what, r.d, limit)
		}
		return r.d, r.err
	case <-time.After(limit + 5*time.Second):
		t.Fatalf("%s: still running after %s", what, limit+5*time.Second)
		return 0, nil
	}
}

// Every rule the allowlist accepts is validated and its next tick found within
// a second (the walk that Add, Edit and each tick make); every rule outside it
// is refused at once. A seeded sample, so failures reproduce.
func TestRules_EvaluateQuickly(t *testing.T) {
	limit, refuseLimit, n := time.Second, 100*time.Millisecond, 150
	if raceEnabled {
		limit, refuseLimit, n = 8*time.Second, time.Second, 40
	}
	g := ruleGen{rand.New(rand.NewSource(20261110))} //nolint:gosec // seeded, so a failure reproduces
	la := mustLoad(t, "America/Los_Angeles")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	var accepted, slowest int
	var slowestD time.Duration
	var slowestSpec string
	for i := range n {
		start := now.Add(-time.Duration(g.r.Int63n(int64(maxStartAge)))).Add(time.Duration(g.r.Intn(30*24)) * time.Hour)
		dtstart := "DTSTART:" + start.UTC().Format(icalUTC)
		loc := time.UTC
		if g.r.Intn(2) == 0 {
			dtstart, loc = "DTSTART:"+start.In(la).Format("20060102T150405"), la
		}
		spec := dtstart + "\n" + g.allowed(now)
		d, err := within(t, limit, spec, func() error {
			r, err := Parse(sched(spec), loc)
			if err != nil {
				return err
			}
			if _, err := r.Next(now.Add(-addGrace)); err != nil {
				return err
			}
			_, _, err = r.window(now.Add(-time.Hour), now)
			return err
		})
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", spec, err)
		}
		if d > slowestD {
			slowest, slowestD, slowestSpec = i, d, spec
		}
	}
	t.Logf("allowed: %d of %d accepted (the rest refused by the grid check); slowest #%d %s: %q",
		accepted, n, slowest, slowestD, slowestSpec)
	if accepted < n*2/3 {
		t.Errorf("only %d of %d allowlist rules accepted: the generator or the allowlist drifted", accepted, n)
	}
	for range n {
		spec := "DTSTART:" + now.Format(icalUTC) + "\n" + g.disallowed(now)
		_, err := within(t, refuseLimit, spec, func() error {
			_, err := Parse(sched(spec), time.UTC)
			return err
		})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted outside the allowlist: %q (%v)", spec, err)
		}
	}
}

// A rebased rule (a never-ending loop's, as it ticks) has the same occurrences
// after the time it was rebased at as the rule it came from: for allowlist rules
// without COUNT, in UTC and in a zone with daylight saving.
func TestRules_RebaseKeepsOccurrences(t *testing.T) {
	n := 120
	if raceEnabled {
		n = 30
	}
	g := ruleGen{rand.New(rand.NewSource(20261112))} //nolint:gosec // seeded, so a failure reproduces
	la := mustLoad(t, "America/Los_Angeles")
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	rebased := 0
	for range n {
		start := now.Add(-time.Duration(g.r.Int63n(int64(30 * 24 * time.Hour))))
		dtstart := "DTSTART:" + start.UTC().Format(icalUTC)
		loc := time.UTC
		if g.r.Intn(2) == 0 {
			dtstart, loc = "DTSTART:"+start.In(la).Format("20060102T150405"), la
		}
		spec := dtstart + "\n" + g.allowed(now.AddDate(3, 0, 0))
		if strings.Contains(spec, "COUNT=") {
			continue
		}
		r, err := Parse(sched(spec), loc)
		if err != nil {
			continue // off the grid
		}
		at := start.Add(time.Duration(g.r.Int63n(int64(2 * 366 * 24 * time.Hour))))
		re, ok := r.Rebase(at)
		if !ok {
			continue
		}
		rebased++
		if got, want := firstAfter(re, at, 30), firstAfter(r, at, 30); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q rebased at %v to %q:\n got %v\nwant %v", spec, at, re.Spec(), got, want)
		}
	}
	t.Logf("%d of %d rules rebased", rebased, n)
	if rebased < n/4 {
		t.Errorf("only %d of %d rules rebased: the generator drifted", rebased, n)
	}
}

// firstAfter lists r's first n occurrences after t, in one walk.
func firstAfter(r *Rule, t time.Time, n int) []time.Time {
	var out []time.Time
	it := r.set.Iterator()
	for o, ok := it(); ok && len(out) < n; o, ok = it() {
		if o.After(t) {
			out = append(out, o)
		}
	}
	return out
}
