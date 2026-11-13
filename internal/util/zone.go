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
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// LocalZoneName returns the IANA name of time.Local, the zone of every time
// amplio shows agents (FormatLocalISO), for clients that render times
// themselves. It is "" when the zone can't be named and isn't UTC all year:
// a client then keeps its own zone rather than show a wrong one.
var LocalZoneName = sync.OnceValue(func() string {
	return localZoneName(time.Local, zoneSystem{readlink: os.Readlink, readFile: os.ReadFile, dirs: zoneDirs},
		time.Now().Year())
})

// zoneDirs are the directories Go loads zone files from on Unix (see
// platformZoneSources in time/zoneinfo_unix.go).
var zoneDirs = []string{"/usr/share/zoneinfo", "/usr/share/lib/zoneinfo", "/usr/lib/locale/TZ", "/etc/zoneinfo"}

// zoneSystem is what localZoneName reads, as a value so tests can stand one in.
type zoneSystem struct {
	readlink func(string) (string, error)
	readFile func(string) ([]byte, error)
	dirs     []string // searched for a zone file by content
}

// localZoneName names loc, the zone Go loaded as time.Local: by the name it
// was loaded by (zoneName); else by a zone file with the same content as the
// one it was loaded from (a copied or bind-mounted /etc/localtime, or $TZ
// naming such a copy); else "UTC" if its offset is 0 all through year; else
// "".
func localZoneName(loc *time.Location, sys zoneSystem, year int) string {
	local := loc.String()
	if name := zoneName(local, sys.readlink); name != "" {
		return name
	}
	file := local // $TZ named a file
	if local == "Local" {
		file = "/etc/localtime"
	}
	if strings.HasPrefix(file, "/") {
		if data, err := sys.readFile(file); err == nil {
			if name := zoneNameByContent(data, sys.dirs); name != "" {
				return name
			}
		}
	}
	if utcAllYear(loc, year) {
		return "UTC"
	}
	return ""
}

// zoneName names the zone Go loaded as time.Local, given its name: the $TZ
// zone or file, "UTC" (also for an unloadable $TZ), or "Local" when $TZ is
// unset and Go read /etc/localtime, whose link target names the zone (see
// initLocal in time/zoneinfo_unix.go). "" when the name doesn't say: a file
// outside a zoneinfo directory, or an /etc/localtime that isn't a link.
func zoneName(local string, readlink func(string) (string, error)) string {
	if local != "Local" {
		return ianaName(local)
	}
	target, err := readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	return ianaName(target)
}

// ianaName is the IANA name a zone's name or file path ends in, if Go can
// load it by that name; else "" (LoadLocation also rejects a path outside
// zoneinfo). A path is cleaned first: a link written as
// /usr/share/zoneinfo//UTC names UTC.
func ianaName(name string) string {
	if strings.Contains(name, "/") {
		name = filepath.Clean(name)
	}
	if i := strings.LastIndex(name, "zoneinfo/"); i >= 0 {
		name = name[i+len("zoneinfo/"):]
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "posix/"), "right/")
	if _, err := time.LoadLocation(name); err != nil || name == "" || name == "Local" {
		return ""
	}
	return name
}

// zoneNameByContent names a zone file by a file with the same bytes in the
// first of dirs that has one, or "" if none does. Of several (links copied
// rather than symlinked), an Area/City name wins over a bare one, any other
// area over Etc, then the first in order.
func zoneNameByContent(data []byte, dirs []string) string {
	var names []string
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil // unreadable: skip it
			case d.IsDir():
				if path != dir && (d.Name() == "posix" || d.Name() == "right") {
					return fs.SkipDir // the same zones again, or with leap seconds
				}
				return nil
			case !d.Type().IsRegular():
				return nil // a symlink names a zone the walk reaches by its own name
			}
			if info, err := d.Info(); err != nil || info.Size() != int64(len(data)) {
				return nil
			}
			//nolint:gosec // G122: a regular file the walk found under the zoneinfo dir; a link swapped in meanwhile could only misname the zone
			if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, data) {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if name := filepath.ToSlash(rel); err == nil && ianaName(name) == name {
				names = append(names, name)
			}
			return nil
		})
		if len(names) > 0 {
			break
		}
	}
	if len(names) == 0 {
		return ""
	}
	rank := func(name string) int {
		switch {
		case !strings.Contains(name, "/"):
			return 2
		case strings.HasPrefix(name, "Etc/"):
			return 1
		}
		return 0
	}
	return slices.MinFunc(names, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
}

// utcAllYear reports whether loc's offset is 0 on the first of every month of
// year: then UTC renders its times exactly.
func utcAllYear(loc *time.Location, year int) bool {
	for m := time.January; m <= time.December; m++ {
		if _, offset := time.Date(year, m, 1, 12, 0, 0, 0, loc).Zone(); offset != 0 {
			return false
		}
	}
	return true
}
