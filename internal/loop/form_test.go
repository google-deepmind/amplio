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
	"strings"
	"testing"
	"time"
)

func TestForm_Schedule(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 30, 12, 345, time.FixedZone("PST", -8*3600))
	for _, tc := range []struct {
		name string
		f    Form
		want string
	}{
		{"send now, 7 days", Form{Every: 30 * time.Minute, SendNow: true, Ends: 7 * day},
			"DTSTART:20261110T233012Z\nRRULE:FREQ=MINUTELY;INTERVAL=30;UNTIL=20261117T233012Z"},
		{"after one interval, 5 times", Form{Every: 90 * time.Minute, Times: 5},
			"DTSTART:20261111T010012Z\nRRULE:FREQ=MINUTELY;INTERVAL=90;COUNT=5"},
		{"hours, never", Form{Every: 2 * time.Hour, SendNow: true},
			"DTSTART:20261110T233012Z\nRRULE:FREQ=HOURLY;INTERVAL=2"},
		{"days", Form{Every: day, SendNow: true, Times: 3},
			"DTSTART:20261110T233012Z\nRRULE:FREQ=DAILY;INTERVAL=1;COUNT=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.f.Schedule(now)
			if err != nil || s.Spec != tc.want {
				t.Fatalf("Schedule = %q, %v; want %q", s.Spec, err, tc.want)
			}
			if _, err := Parse(s, time.UTC); err != nil {
				t.Fatalf("the built rule doesn't parse: %v", err)
			}
		})
	}
}

// What the form says is what the rule does: with Send now the first tick is
// now and counts toward Times.
func TestForm_Occurrences(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		f     Form
		first time.Time
		n     int
	}{
		{Form{Every: time.Minute, SendNow: true, Times: 3}, now, 3},
		{Form{Every: time.Minute, Times: 3}, now.Add(time.Minute), 3},
		{Form{Every: 10 * time.Minute, SendNow: true, Ends: 25 * time.Minute}, now, 3},
	} {
		s, err := tc.f.Schedule(now)
		if err != nil {
			t.Fatal(err)
		}
		got := occurrences(t, mustParse(t, s.Spec, time.UTC), now.Add(-time.Second), 10)
		if len(got) != tc.n || !got[0].Equal(tc.first) {
			t.Errorf("%+v: occurrences %v; want %d starting %v", tc.f, got, tc.n, tc.first)
		}
	}
}

func TestForm_Rejects(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		f    Form
		want string
	}{
		{Form{Every: 30 * time.Second}, "at most every 1m"},
		{Form{Every: 90 * time.Second}, "whole minutes"},
		{Form{Every: time.Hour, Ends: time.Hour, Times: 2}, "not both"},
		{Form{Every: time.Hour, Ends: 30 * time.Minute}, "before its first tick"},
		{Form{Every: time.Hour, Times: -1}, "negative"},
	} {
		if _, err := tc.f.Schedule(now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v; want %q", tc.f, err, tc.want)
		}
	}
}
