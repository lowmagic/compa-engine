package engine

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// BuildVersion says what the running binary was built from, as Go stamped
// it: the commit, whether the checkout had changes not committed, and when
// the commit was made; or that the build carries none, as one not built
// from a git checkout does. compa version prints it, and the help view shows
// it, so the engineer can tell which build is running (found live: an
// older binary ran than the one built, and nothing said so).
func BuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "compa: no build information"
	}
	var rev, at string
	modified := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			at = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return "compa: built without a commit (" + info.GoVersion + ")"
	}
	out := fmt.Sprintf("compa %s", rev[:min(len(rev), 12)])
	if modified {
		out += " with changes not committed"
	}
	if at != "" {
		out += ", committed " + strings.Replace(at, "T", " ", 1)
	}
	return out + " (" + info.GoVersion + ")"
}
