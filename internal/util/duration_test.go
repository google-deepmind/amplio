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
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30m": 30 * time.Minute, "2h": 2 * time.Hour, "7d": 7 * 24 * time.Hour, "1d12h": 36 * time.Hour, "90s": 90 * time.Second,
	} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "d", "xd", "-1d", "7days", "soon", "-5m", "1000000d", "106751d2562047h"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
	for d, want := range map[time.Duration]string{
		7 * 24 * time.Hour: "7d", 36 * time.Hour: "1d12h", 90 * time.Minute: "1h30m", 45 * time.Second: "45s", time.Minute: "1m",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q; want %q", d, got, want)
		}
	}
}
