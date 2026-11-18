// Package semver parses Semantic Versions and the ranges experts declare
// for engineApi.
//
// A range is one or more comparator sets separated by "||"; a comparator set
// is space-separated comparators that must all hold, each an operator (>=, >,
// <=, <, =, or none for =) followed by a full version. A pre-release version
// satisfies a range only if some comparator in the set names a pre-release
// of the same major.minor.patch, as npm and Cargo do.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed Semantic Version. Build metadata is kept for display
// and ignored in comparison.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

// Parse parses a full MAJOR.MINOR.PATCH version with optional pre-release
// and build metadata.
func Parse(s string) (Version, error) {
	var v Version
	rest := s
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		v.Build = rest[i+1:]
		if v.Build == "" {
			return Version{}, fmt.Errorf("version %q: empty build metadata", s)
		}
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		pre := rest[i+1:]
		rest = rest[:i]
		if pre == "" {
			return Version{}, fmt.Errorf("version %q: empty pre-release", s)
		}
		for _, id := range strings.Split(pre, ".") {
			if id == "" || (isNumeric(id) && len(id) > 1 && id[0] == '0') {
				return Version{}, fmt.Errorf("version %q: invalid pre-release identifier %q", s, id)
			}
			v.Pre = append(v.Pre, id)
		}
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH", s)
	}
	nums := make([]uint64, 3)
	for i, p := range parts {
		if p == "" || !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("version %q: invalid number %q", s, p)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// MustParse is Parse for constants; it panics on error.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func isNumeric(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Compare returns -1, 0 or +1 as v is less than, equal to or greater than w,
// by SemVer precedence.
func (v Version) Compare(w Version) int {
	for _, d := range [][2]uint64{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(v.Pre) == 0 && len(w.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(w.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(w.Pre); i++ {
		if c := comparePre(v.Pre[i], w.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(v.Pre) < len(w.Pre):
		return -1
	case len(v.Pre) > len(w.Pre):
		return 1
	}
	return 0
}

func comparePre(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		x, _ := strconv.ParseUint(a, 10, 64)
		y, _ := strconv.ParseUint(b, 10, 64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

type comparator struct {
	op string
	v  Version
}

func (c comparator) holds(v Version) bool {
	r := v.Compare(c.v)
	switch c.op {
	case ">=":
		return r >= 0
	case ">":
		return r > 0
	case "<=":
		return r <= 0
	case "<":
		return r < 0
	}
	return r == 0
}

type Range struct {
	text string
	sets [][]comparator
}

// ParseRange parses a range such as ">=0.1.0 <0.2.0".
func ParseRange(s string) (Range, error) {
	r := Range{text: s}
	for _, set := range strings.Split(s, "||") {
		fields := strings.Fields(set)
		if len(fields) == 0 {
			return Range{}, fmt.Errorf("range %q: empty comparator set", s)
		}
		var cs []comparator
		for _, f := range fields {
			op := ""
			for _, candidate := range []string{">=", "<=", ">", "<", "="} {
				if strings.HasPrefix(f, candidate) {
					op = candidate
					break
				}
			}
			v, err := Parse(strings.TrimPrefix(f, op))
			if err != nil {
				return Range{}, fmt.Errorf("range %q: %w", s, err)
			}
			if op == "" {
				op = "="
			}
			cs = append(cs, comparator{op, v})
		}
		r.sets = append(r.sets, cs)
	}
	return r, nil
}

func (r Range) String() string { return r.text }

func (r Range) Contains(v Version) bool {
	for _, set := range r.sets {
		if setContains(set, v) {
			return true
		}
	}
	return false
}

func setContains(set []comparator, v Version) bool {
	allowPre := len(v.Pre) == 0
	for _, c := range set {
		if !c.holds(v) {
			return false
		}
		if len(c.v.Pre) > 0 && c.v.Major == v.Major && c.v.Minor == v.Minor && c.v.Patch == v.Patch {
			allowPre = true
		}
	}
	return allowPre
}

// CheckCompatible reports an error unless every version the range admits
// lies in one compatibility line: one minor version before 1.0, where a
// minor version may break compatibility, and one major version from 1.0.
// A dependency range must not reach a breaking release, or resolving it
// again could select one.
func (r Range) CheckCompatible() error {
	var line *Version
	for _, set := range r.sets {
		lo, hi, hiInclusive := bounds(set)
		if lo == nil {
			return fmt.Errorf("range %q has no lower bound; it must stay within one minor version before 1.0, one major version after", r.text)
		}
		if hi == nil {
			return fmt.Errorf("range %q has no upper bound; it must stay within one %s, such as >=%s <%s", r.text, lineName(lo), lo, nextLine(lineStart(*lo)))
		}
		start := lineStart(*lo)
		next := nextLine(start)
		core := Version{Major: hi.Major, Minor: hi.Minor, Patch: hi.Patch}
		// A pre-release of the next line's first version, such as
		// <0.2.0-rc.1, admits other pre-releases of it.
		if c := core.Compare(next); c > 0 || c == 0 && (hiInclusive || len(hi.Pre) > 0) {
			return fmt.Errorf("range %q admits versions from %s, past the %s starting at %s", r.text, next, lineName(lo), start)
		}
		if line == nil {
			line = &start
		} else if line.Compare(start) != 0 {
			return fmt.Errorf("range %q spans the lines starting at %s and %s; it must stay within one", r.text, line, start)
		}
	}
	return nil
}

// bounds returns a comparator set's greatest lower bound and least upper
// bound, nil where it has none.
func bounds(set []comparator) (lo, hi *Version, hiInclusive bool) {
	for _, c := range set {
		v := c.v
		if c.op != "<" && c.op != "<=" && (lo == nil || v.Compare(*lo) > 0) {
			lo = &v
		}
		if c.op != ">" && c.op != ">=" && (hi == nil || v.Compare(*hi) < 0 || v.Compare(*hi) == 0 && c.op == "<") {
			hi = &v
			hiInclusive = c.op != "<"
		}
	}
	return lo, hi, hiInclusive
}

func lineStart(v Version) Version {
	if v.Major == 0 {
		return Version{Minor: v.Minor}
	}
	return Version{Major: v.Major}
}

func nextLine(start Version) Version {
	if start.Major == 0 {
		return Version{Minor: start.Minor + 1}
	}
	return Version{Major: start.Major + 1}
}

func lineName(lo *Version) string {
	if lo != nil && lo.Major > 0 {
		return "major version"
	}
	return "minor version"
}
