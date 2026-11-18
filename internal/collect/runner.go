package collect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/expert"
)

type Limits struct {
	// OutputBytes caps each stage's output; excess is dropped and the
	// result is marked truncated, never presented as complete.
	OutputBytes int
	// Timeout bounds the whole pipeline.
	Timeout time.Duration
	// MaxFileBytes caps how much of any one artifact a primitive reads.
	MaxFileBytes int64
	// MaxLineBytes caps one line for line-oriented primitives.
	MaxLineBytes int
	// StderrBytes caps the stderr kept from each command.
	StderrBytes int
	// MemoryBytes bounds a command's address space, where its sandbox can.
	MemoryBytes int64
}

// DefaultLimits are the proposed defaults; docs/COLLECTION.md lists them as
// open decisions.
var DefaultLimits = Limits{OutputBytes: 1 << 20, Timeout: 60 * time.Second, MaxFileBytes: 256 << 20, MaxLineBytes: 1 << 20, StderrBytes: 16 << 10, MemoryBytes: 4 << 30}

// Runner runs every collection, whatever asked for it.
type Runner struct {
	Policy Policy
	// Sandbox runs commands. With none, any pipeline containing a command
	// is unavailable; primitives still run.
	Sandbox Sandbox
	Limits  Limits
	// ScratchParent is where per-command scratch directories are made;
	// empty means the system temporary directory.
	ScratchParent string
	// Exclude names entries directly under the artifact root that neither
	// primitives nor commands may read, beside StoreDir, always excluded.
	Exclude []string
	// CredentialFiles name the files that hold only credentials, which no
	// primitive reads at any depth: a base name, or a path such as
	// auth/kubeconfig that a file's path ends with. Sandboxed commands are
	// not checked against them.
	CredentialFiles []string
	// MaxTimeout is the longest a run may be allowed when it asks for
	// more time than Limits.Timeout; DefaultMaxTimeout when zero.
	MaxTimeout time.Duration
	// allowance is a run's own time allowance (Allowing).
	allowance time.Duration
}

// DefaultMaxTimeout is the ceiling on a run's own time allowance:
// five minutes.
const DefaultMaxTimeout = 5 * time.Minute

// Ceiling is the longest a run may be allowed.
func (r *Runner) Ceiling() time.Duration {
	if r.MaxTimeout > 0 {
		return r.MaxTimeout
	}
	return DefaultMaxTimeout
}

// Allowing is the runner for one run allowed d instead of its timeout, up
// to the ceiling. The allowance is part of what ran, so a longer run is
// other evidence than a shorter one.
func (r *Runner) Allowing(d time.Duration) *Runner {
	c := *r
	if d > 0 {
		c.allowance = min(d, r.Ceiling())
	}
	return &c
}

func (r *Runner) exclusions() []string {
	out := []string{StoreDir}
	for _, name := range r.Exclude {
		if name != StoreDir {
			out = append(out, name)
		}
	}
	return out
}

const (
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusUnavailable = "unavailable"
)

// What says what ran: a test with its parameters, or an ad-hoc pipeline.
type What struct {
	TestID      string `json:"test_id,omitempty"`
	TestVersion string `json:"test_version,omitempty"`
	// Expert or Catalog names the package that defines the test, with its
	// version. A catalog tool's result does not name the expert that asked
	// for it, so the same tool run for two experts is one piece of evidence.
	Expert  string         `json:"expert,omitempty"`
	Catalog string         `json:"catalog,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	// Pipeline is what actually ran, with parameters substituted.
	Pipeline []expert.Stage `json:"pipeline"`
	// Timeout is the run's own time allowance, where it asked for one
	// other than the runner's; a longer run is other evidence.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// Stop says why a run ended before its input did and how far it got, so
// neither the engineer nor a model reads a search that stopped as one
// that found everything there is.
type Stop struct {
	// By is timeout, match_limit, output_cap or read_bound.
	By string `json:"by"`
	// At is the file and line it stopped in, where a primitive was
	// reading files.
	At string `json:"at,omitempty"`
	// FilesGiven and FilesRead count the files a primitive was given
	// and opened; NotReached names the first files it never opened, up
	// to MaxNotReached, and MoreNotReached counts the rest.
	FilesGiven     int      `json:"files_given,omitempty"`
	FilesRead      int      `json:"files_read,omitempty"`
	NotReached     []string `json:"not_reached,omitempty"`
	MoreNotReached int      `json:"more_not_reached,omitempty"`
	// Notes say what else the reader must know, such as which matches a
	// search kept or a file read only to the read bound.
	Notes []string `json:"notes,omitempty"`
	// After is the time allowed, for a run that timed out.
	After time.Duration `json:"after,omitempty"`
}

// seconds says a duration as the engineer reads one: 60 s, 5 min.
func seconds(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return fmt.Sprintf("%d s", d/time.Second)
}

// MaxNotReached bounds the files a stop names.
const MaxNotReached = 20

// Short is the stop as a card says it: what stopped it and how many files
// it never read.
func (s *Stop) Short() string {
	if s == nil {
		return ""
	}
	out := map[string]string{"timeout": "timed out", "match_limit": "stopped at the match limit", "output_cap": "stopped at the output cap", "read_bound": "read a file only in part"}[s.By]
	if out == "" {
		out = "stopped: " + s.By
	}
	if s.By == "timeout" && s.After > 0 {
		out += " after " + seconds(s.After)
	}
	if n := len(s.NotReached) + s.MoreNotReached; n > 0 {
		out += fmt.Sprintf(" · %d of %d files not read", n, s.FilesGiven)
	}
	return out
}

// Summary is the stop in one line, as the engineer and a model read it.
func (s *Stop) Summary() string {
	if s == nil {
		return ""
	}
	why := map[string]string{"timeout": "timed out", "match_limit": "reached its match limit", "output_cap": "reached the output cap", "read_bound": "read a file only to the read bound"}[s.By]
	if why == "" {
		why = s.By
	}
	if s.By == "timeout" && s.After > 0 {
		why += " after " + seconds(s.After)
	}
	out := "stopped: " + why
	if s.At != "" {
		out += " in " + s.At
	}
	if n := len(s.NotReached) + s.MoreNotReached; n > 0 {
		out += fmt.Sprintf("; not reached: %d of %d files (%s", n, s.FilesGiven, strings.Join(s.NotReached, ", "))
		if s.MoreNotReached > 0 {
			out += fmt.Sprintf(", and %d more", s.MoreNotReached)
		}
		out += ")"
	}
	for _, n := range s.Notes {
		out += "; " + n
	}
	return out
}

type StageRecord struct {
	Primitive  string        `json:"primitive,omitempty"`
	Argv       []string      `json:"argv,omitempty"`
	ExitStatus int           `json:"exit_status"`
	Truncated  bool          `json:"truncated"`
	Duration   time.Duration `json:"duration"`
	Stderr     string        `json:"stderr,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// Record is a collection result, stored as evidence.
type Record struct {
	ID         string        `json:"id"`
	What       What          `json:"what"`
	Status     string        `json:"status"`
	Reason     string        `json:"reason,omitempty"`
	ExitStatus int           `json:"exit_status"`
	Truncated  bool          `json:"truncated"`
	StartedAt  time.Time     `json:"started_at"`
	Duration   time.Duration `json:"duration"`
	PathsRead  []string      `json:"paths_read,omitempty"`
	// Stopped says why a run ended before its input did, and how far it
	// got; nil when it read everything it was given.
	Stopped *Stop         `json:"stopped,omitempty"`
	Stages  []StageRecord `json:"stages,omitempty"`
	Sandbox string        `json:"sandbox,omitempty"`
	Output  []byte        `json:"output"`
}

type Sink interface {
	Put(Record) error
}

// evidenceID derives a record's ID from what ran and what it produced, so
// identical results share one piece of evidence.
func evidenceID(r Record) string {
	h := sha256.New()
	j, _ := json.Marshal(r.What)
	h.Write(j)
	fmt.Fprintf(h, "\x00%s\x00%d\x00%v\x00", r.Status, r.ExitStatus, r.Truncated)
	h.Write(r.Output)
	return "ev-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func (r *Runner) limits() Limits {
	l := r.Limits
	d := DefaultLimits
	if l.OutputBytes <= 0 {
		l.OutputBytes = d.OutputBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = d.Timeout
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = d.MaxLineBytes
	}
	if l.StderrBytes <= 0 {
		l.StderrBytes = d.StderrBytes
	}
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = d.MemoryBytes
	}
	return l
}

type Request struct {
	// ArtifactRoot is the directory primitives read and commands see
	// read-only.
	ArtifactRoot string
	What         What
	// Vars are parameter values. When non-nil, {{name}} values in the
	// pipeline are substituted and jq/yq see them as $name.
	Vars map[string]any
}

var wholeTemplate = regexp.MustCompile(`^\{\{([a-z][a-z0-9_]*)\}\}$`)

// substitute replaces whole {{name}} values. Values in argv become strings;
// values in primitive parameters keep their type.
func substitute(stages []expert.Stage, vars map[string]any) ([]expert.Stage, error) {
	if vars == nil {
		return stages, nil
	}
	var subErr error
	sub := func(s string) any {
		m := wholeTemplate.FindStringSubmatch(s)
		if m == nil {
			return s
		}
		v, ok := vars[m[1]]
		if !ok {
			subErr = fmt.Errorf("parameter %q has no value", m[1])
			return s
		}
		return v
	}
	out := make([]expert.Stage, len(stages))
	for i, st := range stages {
		if st.Primitive == "" {
			argv := make([]string, len(st.Argv))
			for j, a := range st.Argv {
				argv[j] = argString(sub(a))
			}
			out[i] = expert.Stage{Argv: argv}
			continue
		}
		params, err := stageParams(st)
		if err != nil {
			return nil, err
		}
		resolved := make(map[string]any, len(params))
		for k, v := range params {
			if s, ok := v.(string); ok {
				resolved[k] = sub(s)
			} else {
				resolved[k] = v
			}
		}
		out[i] = expert.Stage{Primitive: st.Primitive, Params: resolved}
	}
	return out, subErr
}

func argString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

// Check reports whether every command stage of a pipeline is allowed by
// the policy, without running anything. Run checks again.
func (r *Runner) Check(stages []expert.Stage) error {
	for i, st := range stages {
		if st.Primitive != "" {
			continue
		}
		if _, err := r.Policy.Check(st.Argv); err != nil {
			return fmt.Errorf("stage %d: %w", i, err)
		}
	}
	return nil
}

// Run executes a request. It always returns a Record: a refused or
// unavailable collection is recorded with its reason and nothing runs. A
// pipeline is checked completely before any stage starts, so it never runs
// partially because a later stage was refused.
func (r *Runner) Run(ctx context.Context, req Request) Record {
	lim := r.limits()
	if r.allowance > 0 {
		lim.Timeout, req.What.Timeout = r.allowance, r.allowance
	}
	rec := Record{What: req.What, StartedAt: time.Now().UTC()}
	finish := func() Record {
		rec.Duration = time.Since(rec.StartedAt)
		rec.ID = evidenceID(rec)
		return rec
	}
	unavailable := func(format string, args ...any) Record {
		rec.Status = StatusUnavailable
		rec.Reason = fmt.Sprintf(format, args...)
		return finish()
	}

	stages, err := substitute(req.What.Pipeline, req.Vars)
	if err != nil {
		return unavailable("%v", err)
	}
	rec.What.Pipeline = stages
	if len(stages) == 0 {
		return unavailable("empty pipeline")
	}
	type plan struct {
		prim    any
		allowed Allowed
	}
	plans := make([]plan, len(stages))
	for i, st := range stages {
		if st.Primitive != "" {
			params, err := stageParams(st)
			if err != nil {
				return unavailable("stage %d: %v", i, err)
			}
			p, err := decodeParams(st.Primitive, params)
			if err != nil {
				return unavailable("stage %d: %v", i, err)
			}
			plans[i].prim = p
			continue
		}
		a, err := r.Policy.Check(st.Argv)
		if err != nil {
			return unavailable("stage %d: %v", i, err)
		}
		if r.Sandbox == nil {
			return unavailable("stage %d: commands need a sandbox, and none is configured", i)
		}
		plans[i].allowed = a
	}

	for _, name := range r.Exclude {
		if err := CheckExclude(name); err != nil {
			return unavailable("%v", err)
		}
	}
	root, err := openArtifactRoot(req.ArtifactRoot, r.exclusions(), r.CredentialFiles)
	if err != nil {
		return unavailable("artifact root: %v", err)
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()
	if r.Sandbox != nil {
		rec.Sandbox = r.Sandbox.Name()
	}

	var input []byte
	rec.Status = StatusOK
	for i, st := range stages {
		out := newCapped(lim.OutputBytes)
		sr := StageRecord{Primitive: st.Primitive, Argv: st.Argv}
		start := time.Now()
		var err error
		cut := false
		var env *primEnv
		if st.Primitive != "" {
			env = &primEnv{ctx: ctx, root: root, out: out, input: input, hasInput: i > 0, vars: req.Vars, maxFileSize: lim.MaxFileBytes, maxLineSize: lim.MaxLineBytes}
			err = runPrimitive(st.Primitive, plans[i].prim, env)
			cut = env.inputCut
			// The first stage to stop early says how far it got.
			if s := env.stop(err, ctx.Err() != nil); s != nil && rec.Stopped == nil {
				rec.Stopped = s
			}
			if errors.Is(err, errOutputFull) {
				err = nil
			}
		} else {
			sr.ExitStatus, sr.Stderr, err = r.runCommand(ctx, plans[i].allowed, st.Argv, req.ArtifactRoot, input, out, lim)
		}
		sr.Duration = time.Since(start)
		sr.Truncated = out.truncated || cut
		rec.Truncated = rec.Truncated || sr.Truncated
		rec.Stages = append(rec.Stages, sr)
		input = out.Bytes()
		if ctx.Err() != nil {
			rec.Status, rec.Reason = StatusFailed, fmt.Sprintf("stage %d: timed out after %s", i, lim.Timeout)
			rec.Stages[i].Error = rec.Reason
			if rec.Stopped == nil {
				rec.Stopped = &Stop{By: "timeout"}
			}
			break
		}
		if err != nil {
			rec.Status, rec.Reason = StatusFailed, fmt.Sprintf("stage %d: %v", i, err)
			rec.Stages[i].Error = err.Error()
			break
		}
		if sr.ExitStatus != 0 {
			rec.Status, rec.ExitStatus = StatusFailed, sr.ExitStatus
			rec.Reason = fmt.Sprintf("stage %d exited with status %d", i, sr.ExitStatus)
			break
		}
	}
	if rec.Stopped != nil && rec.Stopped.By == "timeout" {
		rec.Stopped.After = lim.Timeout
	}
	rec.Output = input
	rec.PathsRead = sortedStrings(root.paths())
	return finish()
}

// runCommand runs one argv stage in the sandbox with a minimal fixed
// environment and a scratch directory that is removed afterwards.
func (r *Runner) runCommand(ctx context.Context, a Allowed, argv []string, artifactRoot string, stdin []byte, out *cappedBuffer, lim Limits) (int, string, error) {
	scratch, err := os.MkdirTemp(r.ScratchParent, "compa-scratch-")
	if err != nil {
		return -1, "", err
	}
	defer os.RemoveAll(scratch)
	stderr := newCapped(lim.StderrBytes)
	var deny []string
	for _, name := range r.exclusions() {
		deny = append(deny, filepath.Join(artifactRoot, name))
	}
	cmd, err := r.Sandbox.Command(ctx, SandboxSpec{
		Argv:        append([]string{a.Path}, argv[1:]...),
		Scratch:     scratch,
		ReadOnly:    []string{artifactRoot},
		Deny:        deny,
		Env:         []string{"PATH=/usr/bin:/bin", "HOME=" + scratch, "TMPDIR=" + scratch, "LANG=C", "LC_ALL=C"},
		MemoryBytes: lim.MemoryBytes,
		Stdin:       bytes.NewReader(stdin),
		Stdout:      commandWriter{out},
		Stderr:      commandWriter{stderr},
	})
	if err != nil {
		return -1, "", err
	}
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(stderr.Bytes()), nil
	}
	if err != nil {
		return -1, string(stderr.Bytes()), err
	}
	return 0, string(stderr.Bytes()), nil
}
