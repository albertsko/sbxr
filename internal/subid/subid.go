// Package subid reads and chooses subordinate id ranges in the format of /etc/subuid and /etc/subgid.
//
// The phase 2 installer reserves the range Name for pod VMs, apart from every other range, such as the one of the
// kubelet user that bounds the ids of Kubernetes pods with user namespaces. The shim takes each pod's ids from it.
package subid

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Name is the owner of the pod VM range in /etc/subuid and /etc/subgid.
const Name = "sbxr"

// Ids per pod VM, and the number of pod VMs that one node can run at once.
const (
	PerPod = 1 << 16
	Pods   = 512
	Count  = PerPod * Pods
)

// Range is one entry: Count ids from First, owned by Name.
type Range struct {
	Name  string
	First int
	Count int
}

func (r Range) end() int { return r.First + r.Count }

func (r Range) overlaps(o Range) bool { return r.First < o.end() && o.First < r.end() }

func (r Range) String() string {
	return r.Name + ":" + strconv.Itoa(r.First) + ":" + strconv.Itoa(r.Count)
}

// Parse reads the entries of a subordinate id file. It skips comments and lines it cannot read, as shadow does.
func Parse(text string) []Range {
	var out []Range

	for line := range strings.Lines(text) {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) != 3 || strings.HasPrefix(f[0], "#") {
			continue
		}

		first, err1 := strconv.Atoi(f[1])
		count, err2 := strconv.Atoi(f[2])

		if err1 == nil && err2 == nil && first >= 0 && count > 0 {
			out = append(out, Range{Name: f[0], First: first, Count: count})
		}
	}

	return out
}

// Find returns the range of name, which must be the same in both files and the only one of that name.
func Find(subuid, subgid, name string) (Range, error) {
	pick := func(text, file string) (Range, error) {
		var found []Range

		for _, r := range Parse(text) {
			if r.Name == name {
				found = append(found, r)
			}
		}

		if len(found) != 1 {
			return Range{}, fmt.Errorf("%s has %d entries for %s, want 1", file, len(found), name)
		}

		return found[0], nil
	}

	u, err := pick(subuid, "/etc/subuid")
	if err != nil {
		return u, err
	}

	g, err := pick(subgid, "/etc/subgid")
	if err != nil {
		return g, err
	}

	if u != g {
		return u, fmt.Errorf("the %s entries of /etc/subuid (%s) and /etc/subgid (%s) differ", name, u, g)
	}

	return u, nil
}

// ParseGetsubids reads the output of getsubids for one user, such as "0: kubelet 65536 7208960".
// Like the kubelet, it accepts exactly one range.
func ParseGetsubids(out string) (Range, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 || lines[0] == "" {
		return Range{}, fmt.Errorf("want one range, got %q", out)
	}

	f := strings.Fields(lines[0])
	if len(f) != 4 {
		return Range{}, fmt.Errorf("range %q is not valid", lines[0])
	}

	first, err1 := strconv.ParseUint(f[2], 10, 32)
	count, err2 := strconv.ParseUint(f[3], 10, 32)

	if err := errors.Join(err1, err2); err != nil || count == 0 {
		return Range{}, fmt.Errorf("range %q is not valid: %w", lines[0], err)
	}

	return Range{Name: f[1], First: int(first), Count: int(count)}, nil
}

// Choose returns the pod VM range for a node with these files: the existing Name entry when it is valid,
// or the lowest free range of Count ids above the host ids (0 to 65535), aligned to PerPod.
// It also keeps clear of reserved ranges that come from elsewhere, such as the kubelet range from NSS.
// New reports whether the files still need the entry.
func Choose(subuid, subgid string, reserved ...Range) (r Range, isNew bool, err error) {
	others := slices.DeleteFunc(append(append(Parse(subuid), Parse(subgid)...), reserved...), func(r Range) bool { return r.Name == Name })

	if existing, err := Find(subuid, subgid, Name); err == nil {
		for _, o := range others {
			if existing.overlaps(o) {
				return existing, false, fmt.Errorf("the %s entry %s overlaps the entry %s", Name, existing, o)
			}
		}

		return existing, false, nil
	}

	if slices.ContainsFunc(append(Parse(subuid), Parse(subgid)...), func(r Range) bool { return r.Name == Name }) {
		return Range{}, false, fmt.Errorf("the %s entries of /etc/subuid and /etc/subgid are not one equal entry; remove them", Name)
	}

	slices.SortFunc(others, func(a, b Range) int { return a.First - b.First })

	// The kernel treats id 2^32-1 as invalid, so the range ends one pod short of it.
	const limit = 1<<32 - PerPod

	candidate := Range{Name: Name, First: PerPod, Count: Count}

	for _, o := range others {
		if candidate.overlaps(o) {
			candidate.First = (o.end() + PerPod - 1) / PerPod * PerPod
		}
	}

	if candidate.end() > limit {
		return Range{}, false, errors.New("no free range of subordinate ids for sbxr pods")
	}

	return candidate, true, nil
}
