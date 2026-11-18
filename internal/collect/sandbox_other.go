//go:build !darwin && !linux

package collect

// DefaultSandbox has macOS sandbox-exec and Linux bubblewrap; elsewhere
// there is none, and commands are unavailable.
func DefaultSandbox() (Sandbox, error) {
	return nil, ErrNoSandbox
}
