package engine

import (
	"os"
	"os/user"
)

// LoginName is the host login name, which records what the engineer did,
// such as an override: the only identity the engine has.
func LoginName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, k := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
