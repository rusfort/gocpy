package mod

import (
	"strconv"
	"strings"
)

type semver struct {
	major, minor, patch int
	pre                 string
	ok                  bool
}

func parseSemver(v string) semver {
	var s semver
	if !strings.HasPrefix(v, "v") {
		return s
	}
	v = v[1:]
	if i := strings.IndexByte(v, '+'); i >= 0 { // build metadata is ignored
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		s.pre = v[i+1:]
		v = v[:i]
		if s.pre == "" {
			return s
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return s
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" || len(p) > 1 && p[0] == '0' {
			return s
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return s
		}
		nums[i] = n
	}
	s.major, s.minor, s.patch, s.ok = nums[0], nums[1], nums[2], true
	return s
}

// IsValidVersion reports whether v is vMAJOR.MINOR.PATCH[-pre][+build].
func IsValidVersion(v string) bool { return parseSemver(v).ok }

// CompareVersions returns -1, 0 or +1 (semver precedence).
func CompareVersions(a, b string) int {
	x, y := parseSemver(a), parseSemver(b)
	for _, d := range [][2]int{{x.major, y.major}, {x.minor, y.minor}, {x.patch, y.patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case x.pre == y.pre:
		return 0
	case x.pre == "":
		return 1
	case y.pre == "":
		return -1
	}
	xs, ys := strings.Split(x.pre, "."), strings.Split(y.pre, ".")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		if xs[i] == ys[i] {
			continue
		}
		xn, xerr := strconv.Atoi(xs[i])
		yn, yerr := strconv.Atoi(ys[i])
		switch {
		case xerr == nil && yerr == nil:
			if xn < yn {
				return -1
			}
			return 1
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		case xs[i] < ys[i]:
			return -1
		default:
			return 1
		}
	}
	switch {
	case len(xs) < len(ys):
		return -1
	case len(xs) > len(ys):
		return 1
	}
	return 0
}
