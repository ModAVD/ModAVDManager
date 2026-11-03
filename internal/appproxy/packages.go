package appproxy

import (
	"bufio"
	"bytes"
	"slices"
	"strconv"
	"strings"
)

// PackagesList is where Android keeps every installed package's uid (root can read it). It is replaced by a rename
// on every install, update and removal.
const PackagesList = "/data/system/packages.list"

// App uids of user 0 (Process.FIRST_APPLICATION_UID … LAST_APPLICATION_UID). Other users and profiles are not
// covered.
const (
	firstAppUID = 10000
	lastAppUID  = 19999
)

// ParsePackages reads packages.list ("name uid debuggable dataDir seinfo gids …" per line): package → uid, for
// apps of user 0.
func ParsePackages(b []byte) map[string]int {
	m := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !ValidPackage(f[0]) {
			continue
		}
		uid, err := strconv.Atoi(f[1])
		if err != nil || uid < firstAppUID || uid > lastAppUID {
			continue
		}
		m[f[0]] = uid
	}
	return m
}

// ResolveApps maps the listed packages to uids: the status of each, and the sorted uids to route (a uid once even
// when listed packages share it).
func ResolveApps(apps []string, installed map[string]int) ([]AppStatus, []int) {
	byUID := map[int][]string{}
	for p, uid := range installed {
		byUID[uid] = append(byUID[uid], p)
	}
	var st []AppStatus
	var uids []int
	for _, p := range apps {
		a := AppStatus{Package: p, UID: installed[p]}
		if a.UID != 0 {
			for _, o := range byUID[a.UID] {
				if o != p {
					a.SharedWith = append(a.SharedWith, o)
				}
			}
			slices.Sort(a.SharedWith)
			uids = append(uids, a.UID)
		}
		st = append(st, a)
	}
	slices.Sort(uids)
	return st, slices.Compact(uids)
}

// UIDRange is a run of uids, both ends included.
type UIDRange struct{ First, Last int }

func (r UIDRange) String() string { return strconv.Itoa(r.First) + "-" + strconv.Itoa(r.Last) }

// Ranges merges sorted, distinct uids into runs.
func Ranges(uids []int) []UIDRange {
	var out []UIDRange
	for _, u := range uids {
		if n := len(out); n > 0 && out[n-1].Last+1 == u {
			out[n-1].Last = u
			continue
		}
		out = append(out, UIDRange{u, u})
	}
	return out
}
