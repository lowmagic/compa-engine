package collect

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// ArgRules reject arguments that could execute code or write files, even
// inside the sandbox. They are data: a deployment's configuration supplies
// them per binary, and DefaultArgRules covers common tools.
type ArgRules struct {
	// Deny lists options that are refused, as a getopt parser reads them:
	// "-x" also refuses "-x=value", "-xvalue" and "-x" in a cluster such as
	// "-nx"; "--long" also refuses its abbreviations, such as "--lo".
	Deny []string `json:"deny,omitempty"`
	// DenyPatterns are regular expressions; any argument matching one is
	// refused, as is any option whose attached or "=" value matches. They
	// catch dangerous constructs inside script arguments.
	DenyPatterns []string `json:"deny_patterns,omitempty"`
}

// DefaultArgRules are the argument rules for binaries a deployment commonly
// allowlists. They err toward refusing: a refused command is reported, and
// the analyst can ask for a narrower one.
var DefaultArgRules = map[string]ArgRules{
	"find": {Deny: []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls"}},
	"awk":  awkRules,
	"gawk": awkRules,
	"mawk": awkRules,
	"sed": {
		Deny: []string{"-i", "--in-place", "-f", "--file"},
		DenyPatterns: []string{
			// The e, w, W, r and R commands, alone or after an address.
			`(^|[;\n{}])\s*([0-9]+|\$|/[^/]*/)?(\s*,\s*([0-9]+|\$|/[^/]*/))?\s*!?\s*[ewWrR]\b`,
			// The e and w flags of the s command.
			`(^|[;\n{}])\s*([0-9]+|\$|/[^/]*/)?\s*s(.).*?(.).*?(.)[gpiImM0-9]*[ew]`,
		},
	},
	"xargs": {DenyPatterns: []string{`.`}},
}

var awkRules = ArgRules{
	Deny: []string{"-f", "--file", "-i", "--include", "-l", "--load", "-E", "--exec"},
	DenyPatterns: []string{
		`\bsystem\s*\(`,
		`\|`,                        // pipes to or from commands, and |&
		`\b(print|printf)\b[^;}]*>`, // output redirection
		`\bgetline\b`,
	},
}

type Allowed struct {
	// Path is the absolute path executed; the argv name is only a key.
	Path  string   `json:"path"`
	Rules ArgRules `json:"rules"`
}

// Policy is the deployment's command allowlist, keyed by the name commands
// use as argv[0].
type Policy struct {
	Binaries map[string]Allowed
}

// Allow adds name at path with the default argument rules for the binary
// path names, if any: an alias still runs the binary, and takes its rules.
func (p *Policy) Allow(name, path string) {
	if p.Binaries == nil {
		p.Binaries = map[string]Allowed{}
	}
	p.Binaries[name] = Allowed{Path: path, Rules: DefaultArgRules[filepath.Base(path)]}
}

// Allowlist returns the allowlisted names, sorted, as the command tool's
// description lists them.
func (p Policy) Allowlist() []string {
	out := make([]string, 0, len(p.Binaries))
	for n := range p.Binaries {
		out = append(out, n)
	}
	return sortedStrings(out)
}

type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return e.Reason }

func refused(format string, args ...any) error {
	return &RefusedError{Reason: fmt.Sprintf(format, args...)}
}

func (p Policy) Check(argv []string) (Allowed, error) {
	if len(argv) == 0 {
		return Allowed{}, refused("empty command")
	}
	a, ok := p.Binaries[argv[0]]
	if !ok {
		return Allowed{}, refused("binary %q is not allowlisted", argv[0])
	}
	if !filepath.IsAbs(a.Path) {
		return Allowed{}, refused("allowlist entry %q has a relative path", argv[0])
	}
	for _, arg := range argv[1:] {
		for _, d := range a.Rules.Deny {
			if deniedArg(arg, d) {
				return Allowed{}, refused("%s: argument %q is not allowed", argv[0], arg)
			}
		}
		for _, pat := range a.Rules.DenyPatterns {
			re, err := compiled(pat)
			if err != nil {
				return Allowed{}, refused("%s: bad argument rule %q: %v", argv[0], pat, err)
			}
			for _, text := range argTexts(arg) {
				if re.MatchString(text) {
					return Allowed{}, refused("%s: argument %q matches a denied construct", argv[0], arg)
				}
			}
		}
	}
	return a, nil
}

// deniedArg reports whether arg gives the denied option d as a getopt
// parser would read it: d itself, or with its value after "="; a short
// option anywhere in a cluster of them or before an attached value (-ni,
// -i.bak); a long option by any abbreviation (--in-pl). It errs toward
// refusing: a letter of an attached value counts as an option.
func deniedArg(arg, d string) bool {
	if arg == d || strings.HasPrefix(arg, d+"=") {
		return true
	}
	switch {
	case len(d) == 2 && d[0] == '-' && d[1] != '-':
		return len(arg) > 2 && arg[0] == '-' && arg[1] != '-' && strings.IndexByte(arg[1:], d[1]) >= 0
	case strings.HasPrefix(d, "--"):
		name, _, _ := strings.Cut(arg, "=")
		return len(name) > 2 && strings.HasPrefix(name, "--") && strings.HasPrefix(d, name)
	}
	return false
}

// argTexts are the texts in arg a denied construct may hide in: arg
// itself, a long option's value after "=", and each value a short option
// in a cluster could take attached (-ew /tmp/out is sed's -e with the
// script "w /tmp/out").
func argTexts(arg string) []string {
	out := []string{arg}
	switch {
	case strings.HasPrefix(arg, "--"):
		if _, v, ok := strings.Cut(arg, "="); ok {
			out = append(out, v)
		}
	case len(arg) > 2 && arg[0] == '-':
		for i := 2; i < len(arg); i++ {
			out = append(out, arg[i:])
		}
	}
	return out
}

// patterns holds each argument rule's pattern compiled, by its source, so
// a rule is compiled once rather than for every argument it checks.
var patterns sync.Map

type compiledPattern struct {
	re  *regexp.Regexp
	err error
}

func compiled(pat string) (*regexp.Regexp, error) {
	if c, ok := patterns.Load(pat); ok {
		if cp, ok := c.(compiledPattern); ok {
			return cp.re, cp.err
		}
	}
	re, err := regexp.Compile(pat)
	patterns.Store(pat, compiledPattern{re, err})
	return re, err
}
