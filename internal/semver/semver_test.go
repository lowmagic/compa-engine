package semver

import (
	"sort"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, ok := range []string{"0.1.0", "1.2.3-rc.1", "1.0.0-alpha+build.5", "10.20.30"} {
		v, err := Parse(ok)
		if err != nil {
			t.Errorf("Parse(%q): %v", ok, err)
		} else if v.String() != ok {
			t.Errorf("String() = %q, want %q", v, ok)
		}
	}
	for _, bad := range []string{"", "1.0", "1.0.0.0", "01.0.0", "1.0.0-", "1.0.0-01", "1.0.0+", "v1.0.0", "1.a.0", "1..0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): want error", bad)
		}
	}
}

func TestPrecedence(t *testing.T) {
	// The ordering example from the SemVer 2.0.0 specification.
	want := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0"}
	got := append([]string(nil), want...)
	sort.Slice(got, func(i, j int) bool { return MustParse(got[j]).Compare(MustParse(got[i])) > 0 })
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", got, want)
		}
	}
	if MustParse("1.0.0+a").Compare(MustParse("1.0.0+b")) != 0 {
		t.Error("build metadata must not affect precedence")
	}
}

func TestRange(t *testing.T) {
	cases := []struct {
		r, v string
		want bool
	}{
		{">=0.1.0 <0.2.0", "0.1.0", true},
		{">=0.1.0 <0.2.0", "0.1.9", true},
		{">=0.1.0 <0.2.0", "0.2.0", false},
		{">=0.1.0 <0.2.0", "0.0.9", false},
		{">=0.1.0 <0.2.0", "0.1.5-rc.1", false},
		{">=0.1.5-rc.0 <0.2.0", "0.1.5-rc.1", true},
		{"0.1.0", "0.1.0", true},
		{"=0.1.0", "0.1.1", false},
		{"<0.1.0 || >=1.0.0", "1.2.0", true},
		{"<0.1.0 || >=1.0.0", "0.5.0", false},
		{">0.1.0 <=0.3.0", "0.3.0", true},
	}
	for _, c := range cases {
		r, err := ParseRange(c.r)
		if err != nil {
			t.Fatalf("ParseRange(%q): %v", c.r, err)
		}
		if got := r.Contains(MustParse(c.v)); got != c.want {
			t.Errorf("%q contains %s = %v, want %v", c.r, c.v, got, c.want)
		}
	}
	for _, bad := range []string{"", ">=0.1", "~0.1.0", "^1.0.0", ">=0.1.0 ||", "latest"} {
		if _, err := ParseRange(bad); err == nil {
			t.Errorf("ParseRange(%q): want error", bad)
		}
	}
}

func TestCheckCompatible(t *testing.T) {
	for r, want := range map[string]string{
		">=0.1.0 <0.2.0":                   "",
		">=0.1.3 <0.2.0":                   "",
		">=0.1.0 <=0.1.9":                  "",
		"0.1.4":                            "",
		">0.1.0 <0.1.5 || >=0.1.7 <0.2.0":  "",
		">=0.1.0-rc.1 <0.2.0":              "",
		">=1.2.0 <2.0.0":                   "",
		">=0.1.0":                          "no upper bound; it must stay within one minor version, such as >=0.1.0 <0.2.0",
		"<0.2.0":                           "no lower bound",
		">=0.1.0 <1.0.0":                   "admits versions from 0.2.0",
		">=0.1.0 <=0.2.0":                  "admits versions from 0.2.0",
		">=0.1.0 <0.2.0-rc.1":              "admits versions from 0.2.0",
		">=0.1.0 <0.3.0 <0.2.0":            "",
		">=0.1.0 <0.2.0 || >=0.2.0 <0.3.0": "spans the lines starting at 0.1.0 and 0.2.0",
		">=1.0.0 <3.0.0":                   "past the major version starting at 1.0.0",
		">=1.0.0":                          "within one major version, such as >=1.0.0 <2.0.0",
	} {
		rng, err := ParseRange(r)
		if err != nil {
			t.Fatalf("ParseRange(%q): %v", r, err)
		}
		err = rng.CheckCompatible()
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", r, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: got %v, want an error containing %q", r, err, want)
		}
	}
}
