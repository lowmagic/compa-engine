package engine

import (
	"time"

	"github.com/lowmagic/compa-engine/internal/collect"
)

// Sandbox runs collection commands under a containment mechanism: the
// platform's default (DefaultSandbox) unless a deployment passes its own in
// Options.Sandbox. Every one must pass the contract in engine/sandboxtest.
type Sandbox = collect.Sandbox

// SandboxSpec is one sandboxed process, and the contract its sandbox
// keeps.
type SandboxSpec = collect.SandboxSpec

// ErrNoSandbox says the platform has no sandbox mechanism; commands are then
// unavailable, and primitives still run.
var ErrNoSandbox = collect.ErrNoSandbox

// DefaultSandbox is the platform's mechanism: sandbox-exec on macOS,
// bubblewrap at /usr/bin/bwrap on Linux, its memory bound with prlimit
// where /usr/bin/prlimit exists.
func DefaultSandbox() (Sandbox, error) { return collect.DefaultSandbox() }

// runner is the engine's collection runner, as configuration describes
// it, its commands in the deployment's sandbox or the platform's. With
// neither, commands report unavailable and primitives still run.
func (e *Engine) runner() *collect.Runner {
	if e.collector != nil {
		return e.collector
	}
	r := &collect.Runner{Policy: e.config.Policy(), Limits: e.config.CollectLimits(), Exclude: e.config.Collection.Exclude,
		CredentialFiles: e.credentialFiles, MaxTimeout: time.Duration(e.config.Collection.Limits.MaxTimeoutSeconds) * time.Second}
	if r.Sandbox = e.sandbox; r.Sandbox == nil {
		if sb, err := collect.DefaultSandbox(); err == nil {
			r.Sandbox = sb
		}
	}
	e.collector = r
	return r
}
