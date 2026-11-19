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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"amplio/internal/event"
	"amplio/internal/util"

	"github.com/teambition/rrule-go"
)

// MinInterval is the shortest time between two ticks of a loop, whatever its
// schedule says.
const MinInterval = time.Minute

const (
	maxSpecLen   = 4 << 10
	maxPromptLen = 64 << 10
)

// maxOccurrences bounds one walk over a schedule (rrule-go always walks from
// DTSTART): about 3.8 years of a once-a-minute rule, half a second. A walk
// stays far below it: COUNT is at most maxCount, and the scheduler moves the
// DTSTART of a rule without COUNT forward as the loop ticks (Rule.Rebase). A
// variable so tests can lower it.
var maxOccurrences = 2_000_000

// ErrInvalid marks an error in what the caller asked for (a bad schedule, an
// empty prompt), as opposed to a failure to do it.
var ErrInvalid = errors.New("invalid loop")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Rule is a parsed schedule. Its walks (Next, window, Rebase) are not safe for
// concurrent use: rrule-go sorts the set's dates in place as it walks them.
// Describe and Start are.
type Rule struct {
	set     *rrule.Set
	lines   []string // the spec, one property per line
	loc     *time.Location
	summary string // Describe, computed once
}

// listSpace is whitespace after a comma, inside a property's value list.
var listSpace = regexp.MustCompile(`,\s+`)

// NormalizeSpec puts each property of an iCalendar spec on its own line.
// Separating them with spaces is accepted too: none of the properties a
// schedule uses can contain whitespace, except after a comma in a list.
func NormalizeSpec(spec string) string {
	return strings.Join(strings.Fields(listSpace.ReplaceAllString(spec, ",")), "\n")
}

// Parse parses a schedule. A floating time (no "Z", no TZID) is read in loc,
// the server's zone, so a rule like "Mondays at 9:00" follows the clock
// agents see; a plain interval is anchored in UTC and means elapsed time.
//
// The spec is a DTSTART line followed by at most one RRULE and any RDATE and
// EXDATE lines (rrule-go would silently ignore anything else, and keep only the
// last RRULE). The RRULE must be in the allowlist checkRRuleText, checkLists
// and checkRRule enforce, under which its occurrences fall on distinct minutes
// (one second value), so at least MinInterval apart; dates added with RDATE are
// held to that when they fire.
func Parse(s event.Schedule, loc *time.Location) (*Rule, error) {
	if s.Kind != event.ScheduleRRule {
		return nil, invalidf("schedule kind %q: the only kind is %q", s.Kind, event.ScheduleRRule)
	}
	if len(s.Spec) > maxSpecLen {
		return nil, invalidf("schedule spec is %d bytes; the limit is %d", len(s.Spec), maxSpecLen)
	}
	lines := strings.Split(NormalizeSpec(s.Spec), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, invalidf("schedule spec is empty")
	}
	rrules, dates := 0, 0
	for i, line := range lines {
		name := propertyName(line)
		switch {
		case i == 0 && name != "DTSTART":
			return nil, invalidf("schedule spec must start with DTSTART, not %q", line)
		case i == 0:
		case name == "RRULE":
			rrules++
			if err := checkRRuleText(line); err != nil {
				return nil, err
			}
			if err := checkRRuleOptions(line); err != nil {
				return nil, err
			}
		case name == "RDATE":
			dates++
		case name == "EXDATE":
		default:
			return nil, invalidf("schedule spec line %q: want RRULE, RDATE or EXDATE after DTSTART", line)
		}
	}
	if rrules > 1 {
		return nil, invalidf("schedule spec has %d RRULE lines; at most one is supported", rrules)
	}
	if rrules+dates == 0 {
		return nil, invalidf("schedule spec has no RRULE or RDATE: it would never tick")
	}
	set, err := parseSet(lines, loc)
	if err != nil {
		return nil, invalidf("schedule spec: %v", err)
	}
	r := &Rule{set: set, lines: lines, loc: loc}
	if rr := set.GetRRule(); rr != nil {
		if err := checkRRule(rr.OrigOptions, set.GetDTStart()); err != nil {
			return nil, err
		}
	}
	r.summary = r.describe()
	return r, nil
}

// The RRULE allowlist. rrule-go can't be interrupted, so it is only given
// rules whose evaluation is provably cheap:
//   - FREQ is MINUTELY, HOURLY, DAILY, WEEKLY, MONTHLY or YEARLY, and INTERVAL
//     at most maxInterval (larger ones overflow rrule-go's arithmetic).
//   - The only other parts are COUNT, UNTIL, WKST and the filters BYMONTH,
//     BYMONTHDAY, BYDAY, BYHOUR, BYMINUTE and BYSECOND=0, each listing at most
//     maxListLen values. BYSETPOS, BYYEARDAY, BYWEEKNO, BYEASTER and other
//     BYSECONDs are refused.
//   - MINUTELY and HOURLY rules take no day filter (BYMONTH, BYMONTHDAY,
//     BYDAY): one that never matches makes rrule-go step minute by minute (or
//     hour by hour) to year 9999. Their BYHOUR/BYMINUTE must be reachable from
//     DTSTART in steps of INTERVAL: rrule-go loops until the step lands on one,
//     with no other bound, and the reachable values are those congruent to
//     DTSTART modulo gcd(INTERVAL, period). So each occurrence costs at most
//     1440 steps of that loop.
//   - BYDAY with an ordinal (1MO, -1FR) only on MONTHLY and YEARLY, within ±5
//     when it counts within a month (rrule-go panics past it), and not mixed
//     with plain days (rrule-go then yields nothing).
//   - No list names a value twice, however spelt (9, 09 and +9 are one value).
//     rrule-go builds a period's times of day as the product of BYHOUR,
//     BYMINUTE and BYSECOND, repeats included, before it yields any, and
//     counts each toward COUNT; distinct values keep that product at most
//     maxTimesOfDay.
//   - COUNT is 1 to maxCount, and not given with UNTIL (RFC 5545 forbids it;
//     rrule-go would stop at whichever comes first). No part is given twice.
//
// Then every walk is bounded: a DAILY or slower rule whose day filters never
// match stops at year 9999 after at most ~2.9M days (about 0.15 s; WEEKLY and
// slower far less), and a walk that finds occurrences is capped at
// maxOccurrences. Add and Edit also refuse a DTSTART more than maxStartAge in
// the past, which bounds the walk from DTSTART to now.
const (
	maxInterval   = 100_000
	maxListLen    = 64
	maxCount      = 1_000_000
	maxTimesOfDay = 24 * 60
)

var rrulePartAllowed = map[string]bool{
	"FREQ": true, "INTERVAL": true, "COUNT": true, "UNTIL": true, "WKST": true,
	"BYMONTH": true, "BYMONTHDAY": true, "BYDAY": true, "BYHOUR": true, "BYMINUTE": true, "BYSECOND": true,
}

// checkRRuleText applies the allowlist's limits that must hold before rrule-go
// parses the line: allowed parts, each once, list lengths, INTERVAL and COUNT
// (rrule-go reads COUNT=0 and a negative COUNT as no COUNT).
func checkRRuleText(line string) error {
	_, value, _ := strings.Cut(line, ":")
	if strings.TrimSpace(value) == "" {
		return invalidf("the RRULE line is empty")
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ";") {
		k, v, _ := strings.Cut(part, "=")
		k = strings.ToUpper(k)
		switch {
		case k == "DTSTART":
			return invalidf("the RRULE sets its own DTSTART; put it on the DTSTART line")
		case !rrulePartAllowed[k]:
			return invalidf("RRULE part %q isn't supported in a loop's schedule", k)
		case seen[k]:
			return invalidf("RRULE part %s is given twice", k)
		case strings.HasPrefix(k, "BY") && strings.Count(v, ",") >= maxListLen:
			return invalidf("%s lists %d values; at most %d", k, strings.Count(v, ",")+1, maxListLen)
		case k == "INTERVAL":
			if n, err := strconv.Atoi(v); err != nil || n < 1 || n > maxInterval {
				return invalidf("INTERVAL=%s: want 1 to %d", v, maxInterval)
			}
		case k == "COUNT":
			if n, err := strconv.Atoi(v); err != nil || n < 1 || n > maxCount {
				return invalidf("COUNT=%s: want 1 to %d", v, maxCount)
			}
		}
		seen[k] = true
	}
	if seen["COUNT"] && seen["UNTIL"] {
		return invalidf("an RRULE ends after COUNT occurrences or at UNTIL, not both")
	}
	return nil
}

// checkLists applies the allowlist's limits on the BY lists, by value: rrule-go
// reads 9, 09 and +9 alike.
func checkLists(o rrule.ROption) error {
	for _, l := range []struct {
		name   string
		values []int
	}{
		{"BYMONTH", o.Bymonth}, {"BYMONTHDAY", o.Bymonthday}, {"BYHOUR", o.Byhour},
		{"BYMINUTE", o.Byminute}, {"BYSECOND", o.Bysecond},
	} {
		if v, ok := repeated(l.values); ok {
			return invalidf("%s lists %d twice", l.name, v)
		}
	}
	if d, ok := repeated(o.Byweekday); ok {
		return invalidf("BYDAY lists %s twice", d.String())
	}
	for _, s := range o.Bysecond {
		if s != 0 {
			return invalidf("BYSECOND other than 0 isn't supported (ticks are at least a minute apart)")
		}
	}
	// Implied by the checks above and rrule-go's ranges, but it is the bound
	// that matters, so it is checked as such.
	if n := max(len(o.Byhour), 1) * max(len(o.Byminute), 1) * max(len(o.Bysecond), 1); n > maxTimesOfDay {
		return invalidf("BYHOUR, BYMINUTE and BYSECOND make %d times of day; at most %d", n, maxTimesOfDay)
	}
	return nil
}

// repeated returns a value that values lists twice.
func repeated[T comparable](values []T) (T, bool) {
	seen := map[T]bool{}
	for _, v := range values {
		if seen[v] {
			return v, true
		}
		seen[v] = true
	}
	var zero T
	return zero, false
}

// checkRRule applies the rest of the allowlist to the parsed RRULE.
func checkRRule(o rrule.ROption, start time.Time) error {
	interval := max(o.Interval, 1)
	dayFilters := len(o.Bymonth) + len(o.Bymonthday) + len(o.Byweekday)
	// An ordinal counts weekdays within the period: a month (MONTHLY, or
	// YEARLY with BYMONTH), else a year. rrule-go panics past the period's end.
	inMonth := o.Freq == rrule.MONTHLY || len(o.Bymonth) > 0
	plain, ordinal := 0, 0
	for _, d := range o.Byweekday {
		if d.N() == 0 {
			plain++
		} else {
			ordinal++
		}
	}
	if plain > 0 && ordinal > 0 {
		// rrule-go yields nothing for such a list.
		return invalidf("BYDAY mixes days with an ordinal (1MO) and without (FR); use one kind, or two loops")
	}
	for _, d := range o.Byweekday {
		switch n := d.N(); {
		case n == 0:
		case o.Freq != rrule.MONTHLY && o.Freq != rrule.YEARLY:
			return invalidf("BYDAY with an ordinal (%s) needs FREQ=MONTHLY or YEARLY", d.String())
		case inMonth && (n > 5 || n < -5):
			return invalidf("BYDAY=%s: a month has at most 5 of a weekday", d.String())
		}
	}
	switch o.Freq {
	case rrule.SECONDLY:
		return invalidf("FREQ=SECONDLY isn't supported: ticks are at least %s apart; use MINUTELY", util.FormatDuration(MinInterval))
	case rrule.HOURLY:
		if dayFilters > 0 {
			return invalidf("FREQ=HOURLY takes no BYMONTH, BYMONTHDAY or BYDAY; use DAILY with a BYHOUR list")
		}
		if len(o.Byhour) > 0 && !onGrid(start.Hour(), interval, 24, o.Byhour) {
			return invalidf("every %d hours from %02d:00, the schedule never reaches BYHOUR=%s", interval, start.Hour(), ints(o.Byhour))
		}
	case rrule.MINUTELY:
		if dayFilters > 0 {
			return invalidf("FREQ=MINUTELY takes no BYMONTH, BYMONTHDAY or BYDAY; use DAILY with BYHOUR and BYMINUTE lists")
		}
		if len(o.Byhour) == 0 && len(o.Byminute) == 0 {
			break
		}
		hours, minutes := o.Byhour, o.Byminute
		if len(hours) == 0 {
			hours = seq(24)
		}
		if len(minutes) == 0 {
			minutes = seq(60)
		}
		var targets []int
		for _, h := range hours {
			for _, m := range minutes {
				targets = append(targets, 60*h+m)
			}
		}
		if !onGrid(60*start.Hour()+start.Minute(), interval, 24*60, targets) {
			return invalidf("every %d minutes from %02d:%02d, the schedule never reaches its BYHOUR/BYMINUTE times",
				interval, start.Hour(), start.Minute())
		}
	case rrule.YEARLY, rrule.MONTHLY, rrule.WEEKLY, rrule.DAILY:
	}
	return nil
}

// onGrid reports whether stepping from start by interval, modulo period,
// reaches any of targets: the values congruent to start mod gcd(interval, period).
func onGrid(start, interval, period int, targets []int) bool {
	g := gcd(interval, period)
	for _, t := range targets {
		if ((t-start)%g+g)%g == 0 {
			return true
		}
	}
	return false
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func ints(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

// propertyName returns an iCalendar line's property name ("DTSTART" for
// "DTSTART;TZID=...:...").
func propertyName(line string) string {
	if i := strings.IndexAny(line, ";:"); i > 0 {
		return strings.ToUpper(line[:i])
	}
	return strings.ToUpper(line)
}

// Spec is the rule's spec, one property per line.
func (r *Rule) Spec() string { return strings.Join(r.lines, "\n") }

// errTooLong is returned when a walk over the schedule exceeds maxOccurrences.
var errTooLong = fmt.Errorf("%w: the schedule has too many occurrences before the time asked about (is DTSTART far in the past?)",
	ErrInvalid)

// checkRRuleOptions checks the RRULE's lists and builds it on its own, so that
// a rule rrule-go refuses is reported with rrule-go's reason: its set parser,
// used next, reports only "NewRRule failed: <nil>" (e.g. for BYDAY=54MO, a
// weekday ordinal past a year's 53).
func checkRRuleOptions(line string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = invalidf("%s: rrule-go failed: %v", line, p)
		}
	}()
	o, err := rrule.StrToROption(strings.TrimPrefix(line, "RRULE:"))
	if err != nil {
		return invalidf("%s: %v", line, err)
	}
	// Before NewRRule, which builds the times of day.
	if err := checkLists(*o); err != nil {
		return err
	}
	if _, err := rrule.NewRRule(*o); err != nil {
		return invalidf("%s: %v", line, err)
	}
	return nil
}

// parseSet is rrule-go's parser, with a panic turned into an error.
func parseSet(lines []string, loc *time.Location) (set *rrule.Set, err error) {
	defer func() {
		if p := recover(); p != nil {
			set, err = nil, fmt.Errorf("rrule-go failed: %v", p)
		}
	}()
	return rrule.StrSliceToRRuleSetInLoc(lines, loc)
}

// window walks the occurrences once: due is the latest in (after, now], next
// the first after now. Either is zero when there is none. A panic in rrule-go
// (the allowlist should rule them out) is an error, not a crashed timer.
//
// rrule-go's occurrences aren't always in order: on the day clocks spring
// forward, the times of day in the skipped hour come out as the hour before,
// after the ones already there (BYHOUR=1,2 gives 01:15, 01:45, 01:15, 01:45).
// So due only ever moves later.
func (r *Rule) window(after, now time.Time) (due, next time.Time, err error) {
	defer func() {
		if p := recover(); p != nil {
			due, next, err = time.Time{}, time.Time{}, fmt.Errorf("%w: evaluating the schedule failed: %v", ErrInvalid, p)
		}
	}()
	it := r.set.Iterator()
	for i := 0; i < maxOccurrences; i++ {
		t, ok := it()
		if !ok {
			return due, time.Time{}, nil
		}
		if t.After(now) {
			return due, t, nil
		}
		if t.After(after) && t.After(due) {
			due = t
		}
	}
	return time.Time{}, time.Time{}, errTooLong
}

// Next returns the first occurrence after t, or zero if the schedule has
// ended.
func (r *Rule) Next(t time.Time) (time.Time, error) {
	_, next, err := r.window(t, t)
	return next, err
}

// Rebase returns the rule with DTSTART moved up to the latest occurrence of its
// RRULE at or before t, if that changes none of the occurrences after it: the
// RRULE has no COUNT (which counts from DTSTART), the new DTSTART reads back as
// the same instant (not a wall time that clocks going back repeat), and the
// next occurrences after t are the same. The INTERVAL grid (every n minutes,
// days, weeks...) and the values rrule-go takes from DTSTART when the RRULE
// omits them (the time of day, the weekday) are then those of the old DTSTART,
// as they are for any of its occurrences, except on the day clocks spring
// forward: an occurrence in the skipped hour comes out an hour earlier, and as
// a DTSTART it would move every later one too. The last check catches that;
// the next tick rebases instead. RDATE and EXDATE lines stay as they are.
func (r *Rule) Rebase(t time.Time) (*Rule, bool) {
	rr := r.set.GetRRule()
	if rr == nil || rr.OrigOptions.Count > 0 {
		return nil, false
	}
	start, err := latest(rr, t)
	if err != nil || !start.After(r.Start()) {
		return nil, false
	}
	name, value, _ := strings.Cut(r.lines[0], ":")
	layout := "20060102T150405"
	if strings.HasSuffix(strings.ToUpper(value), "Z") {
		start, layout = start.UTC(), icalUTC
	} else {
		start = start.In(r.Start().Location())
	}
	lines := slices.Clone(r.lines)
	lines[0] = name + ":" + start.Format(layout)
	rebased, err := Parse(event.Schedule{Kind: event.ScheduleRRule, Spec: strings.Join(lines, "\n")}, r.loc)
	if err != nil || !rebased.Start().Equal(start) {
		return nil, false
	}
	want, err := r.following(t, rebaseCheck)
	if err != nil {
		return nil, false
	}
	got, err := rebased.following(t, rebaseCheck)
	if err != nil || !slices.EqualFunc(got, want, time.Time.Equal) {
		return nil, false
	}
	return rebased, true
}

// rebaseCheck is how many occurrences after the rebase point Rebase compares.
const rebaseCheck = 3

// following returns the first n occurrences after t (fewer if the schedule
// ends), in one walk, with a panic in rrule-go turned into an error.
func (r *Rule) following(t time.Time, n int) (out []time.Time, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("rrule-go failed: %v", p)
		}
	}()
	it := r.set.Iterator()
	for i := 0; i < maxOccurrences && len(out) < n; i++ {
		o, ok := it()
		if !ok {
			return out, nil
		}
		if o.After(t) {
			out = append(out, o)
		}
	}
	if len(out) < n {
		return nil, errTooLong
	}
	return out, nil
}

// latest returns rr's latest occurrence at or before t (zero if none), with a
// panic in rrule-go turned into an error.
func latest(rr *rrule.RRule, t time.Time) (at time.Time, err error) {
	defer func() {
		if p := recover(); p != nil {
			at, err = time.Time{}, fmt.Errorf("rrule-go failed: %v", p)
		}
	}()
	return rr.Before(t, true), nil
}

// Describe summarizes the rule in a few words for a tick's header: "every
// 30m", "every 1h, 5 times", "every 1d until 2026-11-17T15:30:00-08:00". A
// rule that isn't a plain interval reads as its own text, without DTSTART.
func (r *Rule) Describe() string { return r.summary }

// Start is the rule's DTSTART.
func (r *Rule) Start() time.Time { return r.set.GetDTStart() }

func (r *Rule) describe() string {
	rr := r.set.GetRRule()
	if rr == nil || len(r.set.GetRDate()) > 0 || len(r.set.GetExDate()) > 0 {
		return strings.Join(r.lines[1:], " ")
	}
	o := rr.OrigOptions
	unit, ok := map[rrule.Frequency]time.Duration{
		rrule.MINUTELY: time.Minute,
		rrule.HOURLY:   time.Hour,
		rrule.DAILY:    24 * time.Hour,
	}[o.Freq]
	if !ok || len(o.Bysetpos)+len(o.Bymonth)+len(o.Bymonthday)+len(o.Byyearday)+len(o.Byweekno)+
		len(o.Byweekday)+len(o.Byhour)+len(o.Byminute)+len(o.Bysecond)+len(o.Byeaster) > 0 {
		return strings.Join(r.lines[1:], " ")
	}
	s := "every " + util.FormatDuration(time.Duration(max(o.Interval, 1))*unit)
	switch {
	case o.Count > 0:
		s += fmt.Sprintf(", %d %s", o.Count, plural(o.Count, "time"))
	case !o.Until.IsZero():
		s += " until " + o.Until.In(r.loc).Format(time.RFC3339)
	}
	return s
}
