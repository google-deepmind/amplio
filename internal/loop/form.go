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
	"fmt"
	"time"

	"amplio/internal/event"
	"amplio/internal/util"
)

// Form is the loop form's fields: an interval, whether the first tick goes at
// once, and an end. Ends and Times exclude each other, as iCalendar's UNTIL
// and COUNT do; with neither, the loop never ends.
type Form struct {
	Every   time.Duration
	SendNow bool
	Ends    time.Duration // ends this long after now
	Times   int           // ends after this many ticks (with SendNow, the first counts)
}

const (
	day = 24 * time.Hour
	// icalUTC is iCalendar's UTC date-time format.
	icalUTC = "20060102T150405Z"
)

// Schedule builds the form's rule, anchored in UTC at now: DTSTART is now
// (SendNow) or one interval later; the frequency is the largest unit that
// divides the interval.
func (f Form) Schedule(now time.Time) (event.Schedule, error) {
	switch {
	case f.Every < MinInterval:
		return event.Schedule{}, fmt.Errorf("interval %s: loops tick at most every %s", util.FormatDuration(f.Every), util.FormatDuration(MinInterval))
	case f.Every%time.Minute != 0:
		return event.Schedule{}, fmt.Errorf("interval %s: use whole minutes", util.FormatDuration(f.Every))
	case f.Ends < 0 || f.Times < 0:
		return event.Schedule{}, fmt.Errorf("a loop's end can't be negative")
	case f.Ends > 0 && f.Times > 0:
		return event.Schedule{}, fmt.Errorf("end after a duration or after a number of ticks, not both")
	}
	now = now.UTC().Truncate(time.Second)
	start := now
	if !f.SendNow {
		start = now.Add(f.Every)
	}
	freq, n := "MINUTELY", f.Every/time.Minute
	switch {
	case f.Every%day == 0:
		freq, n = "DAILY", f.Every/day
	case f.Every%time.Hour == 0:
		freq, n = "HOURLY", f.Every/time.Hour
	}
	rule := fmt.Sprintf("RRULE:FREQ=%s;INTERVAL=%d", freq, n)
	switch {
	case f.Times > 0:
		rule += fmt.Sprintf(";COUNT=%d", f.Times)
	case f.Ends > 0:
		until := now.Add(f.Ends)
		if until.Before(start) {
			return event.Schedule{}, fmt.Errorf("the loop would end (after %s) before its first tick (after %s)",
				util.FormatDuration(f.Ends), util.FormatDuration(f.Every))
		}
		rule += ";UNTIL=" + until.Format(icalUTC)
	}
	return event.Schedule{Kind: event.ScheduleRRule, Spec: "DTSTART:" + start.Format(icalUTC) + "\n" + rule}, nil
}
