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

package util

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// maxDays is the most days a time.Duration holds (about 106,000).
const maxDays = int(math.MaxInt64 / int64(24*time.Hour))

// ParseDuration parses a Go duration ("30m", "1h30m"), also accepting a
// leading number of days ("7d", "1d12h").
func ParseDuration(s string) (time.Duration, error) {
	bad := fmt.Errorf("duration %q: want e.g. 30m, 2h, 7d or 1d12h", s)
	rest := strings.TrimSpace(s)
	var days time.Duration
	if i := strings.IndexByte(rest, 'd'); i > 0 {
		n, err := strconv.Atoi(rest[:i])
		if err != nil || n < 0 || n > maxDays {
			return 0, bad
		}
		days, rest = time.Duration(n)*24*time.Hour, rest[i+1:]
		if rest == "" {
			return days, nil
		}
	}
	d, err := time.ParseDuration(rest)
	if err != nil || d < 0 || d > math.MaxInt64-days {
		return 0, bad
	}
	return days + d, nil
}

// FormatDuration renders d in the units ParseDuration reads, largest first and
// without zero parts: "7d", "1d12h", "1h30m", "45s".
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.size
		}
	}
	if b.Len() == 0 {
		return d.String() // under a second
	}
	return b.String()
}
