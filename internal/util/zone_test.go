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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZoneName(t *testing.T) {
	link := func(target string) func(string) (string, error) {
		return func(string) (string, error) { return target, nil }
	}
	noLink := func(string) (string, error) { return "", errors.New("not a symlink") }
	tests := []struct {
		name, local string
		readlink    func(string) (string, error)
		want        string
	}{
		{"$TZ names a zone", "Europe/Zurich", noLink, "Europe/Zurich"},
		{"$TZ names a zone file", "/usr/share/zoneinfo/Asia/Kolkata", noLink, "Asia/Kolkata"},
		{"$TZ empty or unloadable", "UTC", noLink, "UTC"},
		{"/etc/localtime links to a zone", "Local", link("/usr/share/zoneinfo/America/Los_Angeles"), "America/Los_Angeles"},
		{"relative link", "Local", link("../usr/share/zoneinfo/Asia/Tokyo"), "Asia/Tokyo"},
		{"macOS link", "Local", link("/var/db/timezone/zoneinfo/Europe/Paris"), "Europe/Paris"},
		{"posix zone", "Local", link("/usr/share/zoneinfo/posix/Europe/Berlin"), "Europe/Berlin"},
		{"doubled slash in the link", "Local", link("/usr/share/zoneinfo//UTC"), "UTC"},
		{"doubled slashes in a zone path", "Local", link("/usr/share/zoneinfo//America//Los_Angeles"), "America/Los_Angeles"},
		{"/etc/localtime is a copy", "Local", noLink, ""},
		{"$TZ names a file outside zoneinfo", "/srv/localtime", noLink, ""},
		{"link outside zoneinfo", "Local", link("/etc/my-zone"), ""},
		{"link to no zone", "Local", link("/usr/share/zoneinfo/Mars/Olympus"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := zoneName(tc.local, tc.readlink); got != tc.want {
				t.Errorf("zoneName(%q) = %q, want %q", tc.local, got, tc.want)
			}
		})
	}
}

// LocalZoneName names the zone this process actually loaded from $TZ, checked
// in a child process per $TZ.
func TestLocalZoneName_FollowsTZ(t *testing.T) {
	if os.Getenv("AMPLIO_ZONE_CHILD") == "1" {
		fmt.Printf("\nzone=%s\n", LocalZoneName())
		return
	}
	// zoneIn runs the child with $TZ set to tz, or unset when tz is nil.
	zoneIn := func(tz *string) string {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestLocalZoneName_FollowsTZ$")
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "TZ=") {
				cmd.Env = append(cmd.Env, e)
			}
		}
		cmd.Env = append(cmd.Env, "AMPLIO_ZONE_CHILD=1")
		if tz != nil {
			cmd.Env = append(cmd.Env, "TZ="+*tz)
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("child: %v", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if zone, ok := strings.CutPrefix(line, "zone="); ok {
				return zone
			}
		}
		t.Fatalf("child printed no zone: %s", out)
		return ""
	}
	cases := map[string]string{
		"Europe/Zurich": "Europe/Zurich",
		":Asia/Tokyo":   "Asia/Tokyo",
		"":              "UTC",
		"Not/A_Zone":    "UTC", // Go falls back to UTC, so that is the zone in use
	}
	// A copy of a zone file outside zoneinfo (as a copied or bind-mounted
	// /etc/localtime is): named by its content, not reported as UTC.
	if tokyo, err := os.ReadFile("/usr/share/zoneinfo/Asia/Tokyo"); err == nil {
		copied := filepath.Join(t.TempDir(), "localtime")
		if err := os.WriteFile(copied, tokyo, 0o600); err != nil { //nolint:gosec // G703: a path under the test's own TempDir
			t.Fatal(err)
		}
		cases[copied] = "Asia/Tokyo"
	}
	for tz, want := range cases {
		if got := zoneIn(&tz); got != want {
			t.Errorf("TZ=%q: LocalZoneName = %q, want %q", tz, got, want)
		}
	}
	// TZ unset: the child loads /etc/localtime as time.Local. Expect what the
	// whole lookup makes of that file, the content match and the UTC rule
	// included, not only the link's name.
	if data, err := os.ReadFile("/etc/localtime"); err == nil {
		loc, err := time.LoadLocationFromTZData("Local", data)
		if err != nil {
			t.Fatal(err)
		}
		sys := zoneSystem{readlink: os.Readlink, readFile: os.ReadFile, dirs: zoneDirs}
		want := localZoneName(loc, sys, time.Now().Year())
		if got := zoneIn(nil); got != want {
			target, _ := os.Readlink("/etc/localtime")
			t.Errorf("TZ unset: LocalZoneName = %q, want %q (/etc/localtime -> %q)", got, want, target)
		}
	}
}

// zoneFixture reads system zone files by name, skipping the test if there are
// none.
func zoneFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/usr/share/zoneinfo", name))
	if err != nil {
		t.Skipf("no system zone files: %v", err)
	}
	return data
}

// A zone Go loaded from a file its name doesn't place (a copied
// /etc/localtime, or $TZ naming a copy) is named by a zone file with the same
// bytes; failing that, it is "UTC" only if it is UTC all year, else unnamed.
func TestLocalZoneName_ByContent(t *testing.T) {
	tokyo, london, utc := zoneFixture(t, "Asia/Tokyo"), zoneFixture(t, "Europe/London"), zoneFixture(t, "Etc/UTC")
	// dir holds zone files: name -> content.
	dir := func(files map[string][]byte) string {
		d := t.TempDir()
		for name, data := range files {
			path := filepath.Join(d, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	noLink := func(string) (string, error) { return "", errors.New("not a symlink") }
	// name runs localZoneName for a zone loaded from data, as local ("Local":
	// from /etc/localtime; a path: $TZ named it).
	name := func(local string, data []byte, dirs ...string) string {
		t.Helper()
		loc, err := time.LoadLocationFromTZData(local, data)
		if err != nil {
			t.Fatal(err)
		}
		file := local
		if local == "Local" {
			file = "/etc/localtime"
		}
		readFile := func(path string) ([]byte, error) {
			if path != file {
				return nil, os.ErrNotExist
			}
			return data, nil
		}
		return localZoneName(loc, zoneSystem{readlink: noLink, readFile: readFile, dirs: dirs}, 2026)
	}

	zones := dir(map[string][]byte{"Europe/Paris": zoneFixture(t, "Europe/Paris"), "Asia/Tokyo": tokyo,
		"Japan": tokyo, "Etc/GMT-9": tokyo, "posix/Asia/Seoul": tokyo, "right/Asia/Seoul": tokyo})
	if err := os.Symlink("Asia/Tokyo", filepath.Join(zones, "Asia/Pyongyang")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		what, local string
		data        []byte
		dirs        []string
		want        string
	}{
		{"a copied /etc/localtime", "Local", tokyo, []string{zones}, "Asia/Tokyo"},
		{"$TZ naming a copy", "/srv/tz/localtime", tokyo, []string{zones}, "Asia/Tokyo"},
		{"Area/City over Etc over a bare name", "Local", tokyo, []string{dir(map[string][]byte{"Egypt": tokyo,
			"Etc/GMT-9": tokyo, "Pacific/Palau": tokyo})}, "Pacific/Palau"},
		{"Etc over a bare name", "Local", tokyo, []string{dir(map[string][]byte{"Egypt": tokyo, "Etc/GMT-9": tokyo})}, "Etc/GMT-9"},
		{"only a bare name", "Local", tokyo, []string{dir(map[string][]byte{"Japan": tokyo})}, "Japan"},
		{"the first directory with a match", "Local", tokyo,
			[]string{t.TempDir() + "/missing", dir(map[string][]byte{"Japan": tokyo}), zones}, "Japan"},
		{"no match, UTC all year", "Local", utc, []string{zones}, "UTC"},
		{"no match, UTC in winter only", "Local", london, []string{zones}, ""},
		{"no match", "/srv/tz/localtime", tokyo, []string{dir(map[string][]byte{"Europe/Paris": london})}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.what, func(t *testing.T) {
			if got := name(tc.local, tc.data, tc.dirs...); got != tc.want {
				t.Errorf("localZoneName = %q, want %q", got, tc.want)
			}
		})
	}
}
