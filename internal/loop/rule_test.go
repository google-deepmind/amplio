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
	"strings"
	"testing"
	"time"

	"amplio/internal/event"

	"github.com/teambition/rrule-go"
)

func sched(spec string) event.Schedule { return event.Schedule{Kind: event.ScheduleRRule, Spec: spec} }

func mustParse(t *testing.T, spec string, loc *time.Location) *Rule {
	t.Helper()
	r, err := Parse(sched(spec), loc)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return r
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zone data for %s: %v", name, err)
	}
	return loc
}

// occurrences lists the first n occurrences after t.
func occurrences(t *testing.T, r *Rule, after time.Time, n int) []time.Time {
	t.Helper()
	var out []time.Time
	for range n {
		next, err := r.Next(after)
		if err != nil {
			t.Fatal(err)
		}
		if next.IsZero() {
			break
		}
		out = append(out, next)
		after = next
	}
	return out
}

func TestParse_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    event.Schedule
		want string
	}{
		{"kind", event.Schedule{Kind: "cron", Spec: "* * * * *"}, `kind "cron"`},
		{"rdate list spaced, then junk", sched("DTSTART:20261110T120000Z\nRDATE:20261110T130000Z, 20261110T140000Z junk"), `"junk"`},
		{"empty", sched("  \n "), "empty"},
		{"too long", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY\nRDATE:" + strings.Repeat("20261110T130000Z,", 300)), "limit is"},
		{"no dtstart first", sched("RRULE:FREQ=HOURLY\nDTSTART:20261110T120000Z"), "must start with DTSTART"},
		{"unknown property", sched("DTSTART:20261110T120000Z\nEXRULE:FREQ=DAILY"), "want RRULE, RDATE or EXDATE"},
		{"two rrules", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY\nRRULE:FREQ=DAILY"), "2 RRULE lines"},
		{"nothing to fire", sched("DTSTART:20261110T120000Z\nEXDATE:20261110T120000Z"), "never tick"},
		{"bad rule", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=SOMETIMES"), "undefined frequency"},
		{"bad dtstart", sched("DTSTART:yesterday\nRRULE:FREQ=HOURLY"), "StrToDtStart"},
		{"secondly", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=SECONDLY;INTERVAL=90"), "SECONDLY isn't supported"},
		{"by second", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;BYSECOND=0,30"), "BYSECOND other than 0"},
		{"dtstart in the rrule", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;DTSTART=20200101T000000Z"), "its own DTSTART"},
		// Outside the allowlist.
		{"bare rrule", sched("DTSTART:20261110T120000Z RRULE"), "RRULE line is empty"},
		{"bare rrule, lower case", sched("DTSTART:20261110T120000Z rrule"), "RRULE line is empty"},
		{"unknown part", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;FOO=1"), `"FOO" isn't supported`},
		{"bysetpos", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;BYSETPOS=8"), `"BYSETPOS" isn't supported`},
		{"bysetpos minutely", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;BYSETPOS=2;UNTIL=20280101T000000Z"), `"BYSETPOS"`},
		{"byyearday", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;BYYEARDAY=100"), `"BYYEARDAY"`},
		{"byweekno", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=YEARLY;BYWEEKNO=20"), `"BYWEEKNO"`},
		{"byeaster", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=YEARLY;BYEASTER=0"), `"BYEASTER"`},
		{"huge interval", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=9223372036854775800;BYHOUR=12"), "want 1 to 100000"},
		{"max interval", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MONTHLY;INTERVAL=9223372036854775807"), "want 1 to 100000"},
		{"zero interval", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;INTERVAL=0"), "want 1 to 100000"},
		{"day filter on minutely", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=MINUTELY;BYHOUR=23;BYMONTH=2;BYMONTHDAY=30"), "MINUTELY takes no"},
		{"day filter on hourly", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=HOURLY;BYDAY=MO"), "HOURLY takes no"},
		{"ordinal on weekly", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=WEEKLY;BYDAY=2MO"), "needs FREQ=MONTHLY or YEARLY"},
		{"ordinal past a month", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=MONTHLY;BYDAY=53MO"), "at most 5 of a weekday"},
		{"ordinal past bymonth", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=YEARLY;BYMONTH=2;BYDAY=6MO"), "at most 5 of a weekday"},
		{"ordinal past a year", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=YEARLY;BYDAY=54MO"), "byday must be between 1 and 53"},
		{"negative ordinal past a year", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=YEARLY;BYDAY=-54FR"), "byday must be between 1 and 53"},
		{"mixed ordinals", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=MONTHLY;BYDAY=1MO,FR"), "mixes days"},
		{"repeated value", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=DAILY;BYHOUR=9,+9"), "BYHOUR lists 9 twice"},
		{"count with until", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;COUNT=100;UNTIL=20261112T150000Z"), "COUNT occurrences or at UNTIL, not both"},
		{"zero count", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;COUNT=0"), "COUNT=0: want 1 to 1000000"},
		{"negative count", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;COUNT=-3"), "COUNT=-3: want 1"},
		{"huge count", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;COUNT=1000001"), "COUNT=1000001: want 1"},
		{"part given twice", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;COUNT=2;COUNT=5"), "COUNT is given twice"},
		{"long list", sched("DTSTART:20261110T000000Z\nRRULE:FREQ=DAILY;BYMONTHDAY=" + strings.Repeat("1,", 64) + "1"), "65 values; at most 64"},
		// rrule-go would spin forever on these: the INTERVAL grid never lands on the filter.
		{"hourly grid misses byhour", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=2;BYHOUR=1"), "never reaches BYHOUR=1"},
		{"daily grid misses byhour", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=24;BYHOUR=9"), "never reaches"},
		{"minutely grid misses byminute", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=60;BYMINUTE=30"), "never reaches"},
		{"odd minutes", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=2;BYMINUTE=1"), "never reaches"},
		{"minutely grid misses byhour", sched("DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=1440;BYHOUR=9"), "never reaches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseWithin(t, tc.s, time.UTC)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse = %v; want ErrInvalid containing %q", err, tc.want)
			}
		})
	}
}

// parseWithin parses, failing the test if Parse panics or doesn't return
// within 5 s.
func parseWithin(t *testing.T, s event.Schedule, loc *time.Location) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("panic: %v", p)
			}
		}()
		_, err := Parse(s, loc)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("Parse(%q) still running after 5s", s.Spec)
		return nil
	}
}

func TestParse_Accepts(t *testing.T) {
	for _, spec := range []string{
		"DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=1",
		"DTSTART:20261110T120000Z RRULE:FREQ=MINUTELY;INTERVAL=90",                                 // space-separated
		"DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=2;BYHOUR=0,2,14",                     // even hours, on the grid
		"DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=5;BYHOUR=1",                          // gcd(5, 24) = 1
		"DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;BYMINUTE=30",                    // on the half hours
		"DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=1440;BYHOUR=12",                    // daily at noon
		"DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;BYHOUR=9,13;BYMINUTE=30",                       // calendars' "every 4h at :30"
		"DTSTART:20261110T120000Z\nRDATE:20261110T130000Z, 20261110T140000Z",                       // a space after a comma
		"DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30",                       // never matches, but ends (year 9999)
		"DTSTART:20261111T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0;BYSECOND=0", // the future form
		"DTSTART:20261110T120000Z\nRRULE:FREQ=MONTHLY;BYDAY=1MO,-1FR",                              // ordinals, monthly
		"DTSTART:20261110T120000Z\nRRULE:FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29",                      // leap days
		"DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;INTERVAL=100000",                               // the largest INTERVAL
		"DTSTART:20261111T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0;BYSECOND=0\nRDATE:20261110T153000",
		"DTSTART;TZID=Europe/Zurich:20261111T090000\nRRULE:FREQ=DAILY;COUNT=2",
		"DTSTART:20261110T120000Z\nRDATE:20261110T130000Z,20261110T140000Z", // dates only
	} {
		if err := parseWithin(t, sched(spec), time.UTC); err != nil {
			t.Errorf("Parse(%q): %v", spec, err)
		}
	}
	// Across spring-forward, the RRULE iterator repeats 01:00 for the skipped
	// 02:00; that isn't two ticks at once.
	if err := parseWithin(t, sched("DTSTART:20270313T120000\nRRULE:FREQ=HOURLY"), mustLoad(t, "America/Los_Angeles")); err != nil {
		t.Errorf("floating hourly across spring-forward: %v", err)
	}
}

func TestRule_CountUntilNever(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	before := t0.Add(-time.Second)
	count := mustParse(t, "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;COUNT=3", time.UTC)
	if got := occurrences(t, count, before, 10); len(got) != 3 || !got[2].Equal(t0.Add(time.Hour)) {
		t.Errorf("COUNT=3: %v", got)
	}
	until := mustParse(t, "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=90;UNTIL=20261110T170000Z", time.UTC)
	if got := occurrences(t, until, before, 10); len(got) != 4 || !got[3].Equal(t0.Add(270*time.Minute)) {
		t.Errorf("every 90m until 17:00: %v", got)
	}
	never := mustParse(t, "DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=1", time.UTC)
	if got := occurrences(t, never, t0.Add(1000*time.Hour), 3); len(got) != 3 {
		t.Errorf("never-ending rule stopped: %v", got)
	}
}

// A rule with times of day uses floating time, read in the server's zone: 9:00
// means 9:00 there, also across a daylight-saving change.
func TestRule_FloatingTimeFollowsServerZone(t *testing.T) {
	spec := "DTSTART:20270312T090000\nRRULE:FREQ=DAILY;COUNT=3"
	for _, zone := range []string{"America/Los_Angeles", "Europe/Zurich"} {
		loc := mustLoad(t, zone)
		got := occurrences(t, mustParse(t, spec, loc), time.Time{}, 5)
		if len(got) != 3 {
			t.Fatalf("%s: %v", zone, got)
		}
		for _, o := range got {
			if l := o.In(loc); l.Hour() != 9 || l.Minute() != 0 {
				t.Errorf("%s: occurrence %v is not 9:00 local", zone, l)
			}
		}
		if zone == "America/Los_Angeles" && got[2].Sub(got[1]) != 23*time.Hour {
			t.Errorf("DST change (Mar 14) should make one day 23h: %v", got)
		}
	}
	// A UTC anchor is elapsed time, whatever the zone.
	utc := mustParse(t, "DTSTART:20270313T170000Z\nRRULE:FREQ=DAILY;COUNT=2", mustLoad(t, "America/Los_Angeles"))
	if got := occurrences(t, utc, time.Time{}, 2); got[1].Sub(got[0]) != 24*time.Hour {
		t.Errorf("UTC-anchored daily rule across DST: %v", got)
	}
}

func TestRule_Window(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r := mustParse(t, "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=1", time.UTC)
	due, next, err := r.window(t0, t0.Add(150*time.Second))
	if err != nil || !due.Equal(t0.Add(2*time.Minute)) || !next.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("window = %v, %v, %v; want the latest due (12:02) and the next (12:03)", due, next, err)
	}
	if due, _, _ := r.window(t0.Add(2*time.Minute), t0.Add(150*time.Second)); !due.IsZero() {
		t.Errorf("nothing is due after the spent 12:02; got %v", due)
	}
}

func TestRule_WalkIsBounded(t *testing.T) {
	defer func(n int) { maxOccurrences = n }(maxOccurrences)
	maxOccurrences = 100
	r := mustParse(t, "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY", time.UTC)
	if _, err := r.Next(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Next far past DTSTART = %v; want the walk refused", err)
	}
}

func TestRule_Describe(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	for _, tc := range []struct{ spec, want string }{
		{"DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=30", "every 30m"},
		{"DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=90;COUNT=5", "every 1h30m, 5 times"},
		{"DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=2", "every 2h"},
		{"DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;INTERVAL=1;COUNT=1", "every 1h, 1 time"},
		{"DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY;INTERVAL=1;UNTIL=20261117T233000Z", "every 1d until 2026-11-17T15:30:00-08:00"},
		{"DTSTART:20261111T090000\nRRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0", "RRULE:FREQ=WEEKLY;BYDAY=MO,WE,FR;BYHOUR=9;BYMINUTE=0"},
		{"DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY\nRDATE:20261110T113000Z", "RRULE:FREQ=HOURLY RDATE:20261110T113000Z"},
	} {
		if got := mustParse(t, tc.spec, la).Describe(); got != tc.want {
			t.Errorf("Describe(%q) = %q; want %q", tc.spec, got, tc.want)
		}
	}
}

// A panic in rrule-go while walking (which the allowlist rules out) is an
// error, not a crash of the timer goroutine that walks.
func TestRule_PanicIsAnError(t *testing.T) {
	lines := []string{"DTSTART:20261110T120000Z", "RRULE:FREQ=MONTHLY;BYDAY=53MO"} // past Parse, rrule-go panics on it
	set, err := rrule.StrSliceToRRuleSetInLoc(lines, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	r := &Rule{set: set, lines: lines, loc: time.UTC}
	if _, err := r.Next(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "evaluating the schedule failed") {
		t.Errorf("Next on a panicking rule = %v", err)
	}
}

// The BY lists are checked by value, as rrule-go reads them: a value spelt
// twice (9, 09, +9; 0 and -0) would multiply the times of day rrule-go builds
// for every period, and count toward COUNT once per spelling.
func TestParse_RefusesRepeatedValuesHoweverSpelt(t *testing.T) {
	for _, rr := range []string{
		"RRULE:FREQ=MINUTELY;BYSECOND=0,00,000",
		"RRULE:FREQ=MINUTELY;BYSECOND=0,-0",
		"RRULE:FREQ=MINUTELY;BYSECOND=0,0",
		"RRULE:FREQ=DAILY;BYHOUR=9,09,009;BYMINUTE=0",
		"RRULE:FREQ=DAILY;BYHOUR=9;BYMINUTE=0,00",
		"RRULE:FREQ=DAILY;BYHOUR=9,+9",
		"RRULE:FREQ=HOURLY;BYMINUTE=5,05,-0,0",
		"RRULE:FREQ=YEARLY;BYMONTH=1,01",
		"RRULE:FREQ=MONTHLY;BYMONTHDAY=-1,-01",
		"RRULE:FREQ=WEEKLY;BYDAY=MO,MO",
		"RRULE:FREQ=MONTHLY;BYDAY=1MO,+1MO",
		"RRULE:FREQ=MONTHLY;BYDAY=-1FR,-01FR",
	} {
		spec := "DTSTART:20261110T120000Z\n" + rr
		if err := parseWithin(t, sched(spec), time.UTC); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "twice") {
			t.Errorf("Parse(%s) = %v; want it refused for a repeated value", rr, err)
		}
	}
	// Distinct values, and the same weekday with different ordinals, are fine.
	for _, rr := range []string{
		"RRULE:FREQ=DAILY;BYHOUR=9,10;BYMINUTE=0,30",
		"RRULE:FREQ=MONTHLY;BYDAY=1MO,-1MO",
	} {
		if err := parseWithin(t, sched("DTSTART:20261110T120000Z\n"+rr), time.UTC); err != nil {
			t.Errorf("Parse(%s) = %v", rr, err)
		}
	}
}

// The largest rule the old spelling check let through, 64 spellings in each of
// BYHOUR, BYMINUTE and BYSECOND (262,144 times a day, GBs and seconds per
// walk), is refused before rrule-go builds anything.
func TestParse_RefusesSpeltOutWorstCaseFast(t *testing.T) {
	spell := func(values, total int) string {
		var out []string
		for i := range total {
			v := i % values
			out = append(out, fmt.Sprintf("%0*d", 1+i/values, v))
		}
		return strings.Join(out, ",")
	}
	spec := "DTSTART:20260101T000000Z\nRRULE:FREQ=YEARLY;BYDAY=MO,TU,WE,TH,FR,SA,SU;BYHOUR=" + spell(24, 64) +
		";BYMINUTE=" + spell(60, 64) + ";BYSECOND=" + spell(1, 64)
	if len(spec) > maxSpecLen {
		t.Fatalf("spec is %d bytes", len(spec))
	}
	_, err := within(t, 100*time.Millisecond, "Parse(worst case)", func() error {
		_, err := Parse(sched(spec), time.UTC)
		return err
	})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "twice") {
		t.Errorf("Parse(worst case) = %v; want it refused for a repeated value", err)
	}
}

// COUNT counts each occurrence once, with several times of day too.
func TestRule_CountCountsEachOccurrence(t *testing.T) {
	r := mustParse(t, "DTSTART:20261110T080000Z\nRRULE:FREQ=DAILY;COUNT=4;BYHOUR=9,17;BYMINUTE=0", time.UTC)
	got := occurrences(t, r, time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC), 10)
	want := []time.Time{
		time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 11, 17, 0, 0, 0, time.UTC),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("COUNT=4 at 9:00 and 17:00 gave %v; want %v", got, want)
	}
	if d := r.Describe(); d != "RRULE:FREQ=DAILY;COUNT=4;BYHOUR=9,17;BYMINUTE=0" {
		t.Errorf("Describe = %q", d)
	}
}

// Rebase moves DTSTART up to an occurrence without changing any later one,
// for each way a rule's grid can hang off its DTSTART.
// On the day clocks spring forward, rrule-go gives the times of day in the
// skipped hour as the hour before, after the ones already there: BYHOUR=1,2
// at :15 and :45 yields 01:15, 01:45, 01:15, 01:45. The due one stays the
// latest.
func TestRule_WindowAcrossSpringForward(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	r := mustParse(t, "DTSTART:20270314T000000\nRRULE:FREQ=DAILY;BYHOUR=1,2;BYMINUTE=15,45;COUNT=3", la)
	due, _, err := r.window(time.Date(2027, 3, 14, 1, 0, 0, 0, la), time.Date(2027, 3, 14, 1, 50, 0, 0, la))
	if want := time.Date(2027, 3, 14, 1, 45, 0, 0, la); err != nil || !due.Equal(want) {
		t.Errorf("due = %v, %v; want %v", due, err, want)
	}
}

// A schedule at 02:30 has no 02:30 on the day clocks spring forward: rrule-go
// gives 01:30 that day. Rebasing onto it would move every later occurrence to
// 01:30, so that tick doesn't rebase; the next one does, onto a real 02:30.
func TestRule_RebaseSkipsSpringForward(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	r := mustParse(t, "DTSTART:20270207T023000\nRRULE:FREQ=WEEKLY", la)
	skipped := time.Date(2027, 3, 14, 1, 30, 0, 0, la) // the occurrence that day
	if got := occurrences(t, r, time.Date(2027, 3, 10, 0, 0, 0, 0, la), 1); !got[0].Equal(skipped) {
		t.Fatalf("the Mar 14 occurrence is %v; want %v", got[0], skipped)
	}
	if rebased, ok := r.Rebase(skipped); ok {
		t.Errorf("rebased onto %v: %q, next %v", skipped, rebased.Spec(), occurrences(t, rebased, skipped, 1))
	}
	next := time.Date(2027, 3, 21, 2, 30, 0, 0, la)
	rebased, ok := r.Rebase(next)
	if !ok {
		t.Fatalf("not rebased onto %v", next)
	}
	if got, want := occurrences(t, rebased, next, 3), occurrences(t, r, next, 3); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after %v the rebased rule gives %v; want %v", next, got, want)
	}
}

func TestRule_Rebase(t *testing.T) {
	la := mustLoad(t, "America/Los_Angeles")
	for _, tc := range []struct {
		name, spec string
		loc        *time.Location
		at         time.Time // rebase at this time
		wantStart  string    // the new DTSTART line
	}{
		{"every 7 minutes", "DTSTART:20261110T120000Z\nRRULE:FREQ=MINUTELY;INTERVAL=7", time.UTC,
			time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), "DTSTART:20270228T235700Z"},
		{"seconds from DTSTART", "DTSTART:20261110T120030Z\nRRULE:FREQ=HOURLY;INTERVAL=5", time.UTC,
			time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), "DTSTART:20270228T220030Z"},
		{"times of day", "DTSTART:20261110T080000Z\nRRULE:FREQ=DAILY;INTERVAL=3;BYHOUR=9,17;BYMINUTE=15", time.UTC,
			time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC), "DTSTART:20270226T171500Z"},
		{"every other week", "DTSTART:20261111T090000\nRRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,FR;WKST=SU", la,
			time.Date(2027, 3, 20, 0, 0, 0, 0, la), "DTSTART:20270319T090000"},
		{"monthly on the 31st", "DTSTART:20261231T100000Z\nRRULE:FREQ=MONTHLY;INTERVAL=2;UNTIL=20300101T000000Z", time.UTC,
			time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC), "DTSTART:20270831T100000Z"},
		{"zone", "DTSTART;TZID=Europe/Zurich:20261111T090000\nRRULE:FREQ=DAILY", time.UTC,
			time.Date(2027, 7, 1, 12, 0, 0, 0, time.UTC), "DTSTART;TZID=Europe/Zurich:20270701T090000"},
		{"with dates", "DTSTART:20261110T120000Z\nRRULE:FREQ=DAILY\nRDATE:20261225T180000Z", time.UTC,
			time.Date(2027, 1, 1, 13, 0, 0, 0, time.UTC), "DTSTART:20270101T120000Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mustParse(t, tc.spec, tc.loc)
			rebased, ok := r.Rebase(tc.at)
			if !ok {
				t.Fatal("not rebased")
			}
			if got := strings.Split(rebased.Spec(), "\n")[0]; got != tc.wantStart {
				t.Errorf("DTSTART line %q; want %q", got, tc.wantStart)
			}
			if rebased.Describe() != r.Describe() {
				t.Errorf("Describe %q; was %q", rebased.Describe(), r.Describe())
			}
			if got, want := occurrences(t, rebased, tc.at, 50), occurrences(t, r, tc.at, 50); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("after %v the rebased rule gives\n%v; want\n%v", tc.at, got, want)
			}
		})
	}
	for _, spec := range []string{
		"DTSTART:20261110T120000Z\nRRULE:FREQ=HOURLY;COUNT=10000",               // COUNT counts from DTSTART
		"DTSTART:20261110T120000Z\nRDATE:20261111T120000Z",                      // no RRULE
		"DTSTART:20261231T100000Z\nRRULE:FREQ=MONTHLY;INTERVAL=2;BYMONTHDAY=31", // no occurrence since DTSTART
	} {
		if _, ok := mustParse(t, spec, time.UTC).Rebase(time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)); ok {
			t.Errorf("%q rebased", spec)
		}
	}
}
