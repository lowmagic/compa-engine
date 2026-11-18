package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
)

// Case is a support case's metadata as its source reports it. The engine
// does not correct what a source reports; the routing keys, in particular, are the
// case's canonical routing keys because the source says so (docs/ARCHITECTURE.md).
// A source leaves empty what it does not have.
type Case struct {
	// ID is the case number engineers use, and names the case's folder.
	ID string
	// SourceID is the source's own record ID, where it differs from ID.
	SourceID       string
	Subject        string
	Description    string
	Status         string
	InternalStatus string
	// WaitingOnSupport is the source's own judgment that the next action
	// is support's, which may not follow from Status alone.
	WaitingOnSupport bool
	Severity         string
	Product          string
	ProductVersion   string
	ClusterID        string
	ClusterVersion   string
	// RoutingKeys are the keys the source reports for routing the case
	// to experts; the engine does not correct them.
	RoutingKeys []string
	// Components and Tags are explicit routing facts supplied by the source,
	// not labels the engine infers from the subject or description.
	Components []string
	Tags       []string
	Account    Party
	Owner      Party
	Created    time.Time
	Updated    time.Time
}

// Party is an account, a user or a group as a source names it.
type Party struct {
	ID   string
	Name string
	// Number is the account's number as support uses it, where the source
	// has one; the engine keeps it apart from the case's evidence, for the
	// deployment's list of accounts barred from AI processing, and never
	// shows it to a model.
	Number string
}

type Visibility string

const (
	VisibilityPrivate Visibility = "private"
	VisibilityPartner Visibility = "partner"
	VisibilityPublic  Visibility = "public"
)

// The author kinds the engine has words of its own for. A source may
// report any other, such as partner or vendor: the engine carries it as
// given and shows it under its own name.
const (
	AuthorSupport  = "support"
	AuthorCustomer = "customer"
)

type Comment struct {
	ID     string
	Author Party
	// AuthorKind is who the author is, as the source records it: support,
	// customer, or any other kind the source has, one word; empty when the
	// source does not say. Never guessed from a name or an address.
	AuthorKind string
	// Automated is the source's own mark that an automated process wrote
	// the comment; false when it does not say. Never guessed from a name.
	Automated  bool
	Visibility Visibility
	Created    time.Time
	// Updated is when the comment was last edited; zero if it never was.
	Updated time.Time
	Body    string
}

// CaseSource reads case metadata and comments. It is read-only: posting a
// private comment is post-MVP work, added then as a separate interface a
// source may also implement.
type CaseSource interface {
	// Name identifies the source in evidence provenance.
	Name() string
	Case(ctx context.Context, id string) (Case, error)
	// Comments returns the case's comments oldest first, ties broken by ID.
	Comments(ctx context.Context, id string) ([]Comment, error)
}

// ArtifactSource locates a case's artifacts: must-gathers, sosreports and
// logs, as a directory tree the runner mounts read-only. Fetching,
// extracting and granting access are the source's business; the engine
// unpacks nothing and records what the source reports.
type ArtifactSource interface {
	// Name identifies the source in evidence provenance.
	Name() string
	// Root returns the case's artifact directory as an absolute path with
	// every symlink resolved, so collection never follows one out of it
	// later.
	Root(ctx context.Context, caseID string) (string, error)
	// Attachments lists what the case's artifact directory holds, sorted by
	// name.
	Attachments(ctx context.Context, caseID string) ([]Attachment, error)
}

// ArtifactFetcher is implemented beside ArtifactSource by a source that can
// fetch a case's artifacts when Root finds none; the engine looks for it on
// Options.Artifacts. Before a command that opens a case makes its store,
// the engine offers the fetch on the engineer's terminal, and runs it on a
// yes, so a case folder it brings is where the store goes.
type ArtifactFetcher interface {
	// FetchCommand is what Fetch runs, as the engineer would type it.
	FetchCommand(caseID string) string
	// Fetch fetches the case's artifacts, attached to the engineer's
	// terminal: in and out are its input and output.
	Fetch(ctx context.Context, caseID string, in io.Reader, out io.Writer) error
}

// Attachment is one thing a case's artifacts hold, as its source reports
// it: an attachment the case received, or, for a source that knows no
// better, an entry in the directory.
type Attachment struct {
	Name string
	// Path is the attachment's own file, relative to the root; empty when
	// the source keeps none.
	Path string
	// Dir is where the attachment's contents are, relative to the root:
	// the directory it was extracted to, or the attachment itself when it
	// is a directory. Empty when there is none.
	Dir string
	// Size is the attachment's size in bytes as the source reports it, -1
	// when unknown.
	Size  int64
	State AttachmentState
	// Kind is what the source says the attachment is (a must-gather, a
	// sosreport), empty when it does not say. The engine does not guess.
	Kind string
}

type AttachmentState string

const (
	// AttachmentPresent is available as it is: a plain file or a directory.
	AttachmentPresent AttachmentState = "present"
	// AttachmentExtracted is an archive whose contents are in Dir.
	AttachmentExtracted AttachmentState = "extracted"
	// AttachmentNotExtracted is an archive the source has not extracted.
	AttachmentNotExtracted AttachmentState = "not_extracted"
	AttachmentPending      AttachmentState = "pending"
	AttachmentFailed       AttachmentState = "failed"
	// AttachmentUnknown is for a source that cannot tell which of the
	// others applies.
	AttachmentUnknown AttachmentState = "unknown"
)

// Valid reports whether s is one of the states above.
func (s AttachmentState) Valid() bool {
	switch s {
	case AttachmentPresent, AttachmentExtracted, AttachmentNotExtracted, AttachmentPending, AttachmentFailed, AttachmentUnknown:
		return true
	}
	return false
}

var (
	ErrCaseNotFound  = errors.New("case not found")
	ErrNoArtifacts   = errors.New("no artifact directory")
	ErrInvalidCaseID = errors.New("invalid case ID")
)

// CheckCaseID reports whether id can name a directory, which every source
// that maps case IDs to paths relies on.
func CheckCaseID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidCaseID)
	case id == "." || id == "..":
		return fmt.Errorf("%w %q: cannot name a directory", ErrInvalidCaseID, id)
	case len(id) > 255:
		return fmt.Errorf("%w: longer than 255 bytes", ErrInvalidCaseID)
	case strings.HasPrefix(id, "-"):
		return fmt.Errorf("%w %q: starts with '-', which a command would read as an option", ErrInvalidCaseID, id)
	}
	for _, r := range id {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w %q: contains %q, so it cannot name one directory", ErrInvalidCaseID, id, r)
		}
	}
	return nil
}

// Options are what deployment code supplies when it builds the engine.
// Sources are passed in rather than registered, so a binary's sources are
// fixed at compile time.
type Options struct {
	Cases     CaseSource
	Artifacts ArtifactSource
	// Config is the deployment's configuration, a YAML document in the
	// public config schema (api/config/v1alpha1), so deployment code needs
	// no engine-internal type; empty means the defaults. LayerConfig lays
	// the engineer's files over it. Relative paths in it resolve against
	// the working directory.
	Config []byte
	// Home is the engineer's home directory, where a case with no case
	// folder is stored; empty means the user's own.
	Home string
	// Bundle is the expert set built into the deployment: package
	// directories under experts/ and the routing library at routing/library.yaml.
	// It is the bundled expert root and the routing library unless the
	// configuration names others.
	Bundle fs.FS
	// CredentialFiles name the files that hold only credentials and
	// nothing a diagnosis needs, which no primitive reads, at any depth:
	// a base name, or a path such as auth/kubeconfig that a file's path
	// ends with. They are compiled into the deployment, so no
	// configuration file can remove one.
	CredentialFiles []string
	// References are the deployment's knowledge-base adapters, one per
	// service ID; none where the deployment reaches no knowledge base.
	References []ReferenceAdapter
	// Sandbox runs collection commands in place of the platform's default
	// mechanism (DefaultSandbox); nil means the default. It must pass
	// engine/sandboxtest.
	Sandbox Sandbox
}

type Engine struct {
	cases     CaseSource
	artifacts ArtifactSource
	config    *config.Config
	home      string
	// references are the knowledge-base adapters by service ID.
	references map[string]ReferenceAdapter
	// lazy guards installed, made on first use, which background work
	// reaches from several goroutines.
	lazy      sync.Mutex
	installed *installed
	collector *collect.Runner
	bundle    fs.FS
	bundleDir string
	// credentialFiles are the deployment's, from Options.
	credentialFiles []string
	// sandbox is the deployment's, from Options; nil means the default.
	sandbox Sandbox
	// defaults is the deployment's configuration, from Options, and
	// configFrom names the configuration's layers, in order.
	defaults   []byte
	configFrom []string
	// models are the companion's model providers by ID, each made once and
	// kept, since a provider holds its own caches across calls; tests set
	// it to the fixture provider.
	modelsMu sync.Mutex
	models   map[string]model.Provider
}

func New(o Options) (*Engine, error) {
	if o.Cases == nil {
		return nil, errors.New("engine: no case source; pass Options.Cases (stubcases.New for none)")
	}
	if o.Artifacts == nil {
		return nil, errors.New("engine: no artifact source; pass Options.Artifacts (localdir.New for a directory of cases)")
	}
	e := &Engine{cases: o.Cases, artifacts: o.Artifacts, home: o.Home, bundle: o.Bundle, credentialFiles: o.CredentialFiles, sandbox: o.Sandbox, defaults: o.Config}
	refs, err := referencesByService(o.References)
	if err != nil {
		return nil, err
	}
	e.references = refs
	c, err := config.Layered(config.Layer{Name: "the engine's base", Data: config.Base}, config.Layer{Name: "deployment configuration", Data: o.Config})
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	e.config = c
	if e.home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("engine: no home directory: %w", err)
		}
		e.home = home
	}
	return e, nil
}

// LayerConfig lays the engineer's configuration over the deployment's:
// the engine's base, the built-in defaults, then the user's file,
// $XDG_CONFIG_HOME/compa/config.yaml, if there is one, then file, where
// not empty, which must exist.
func (e *Engine) LayerConfig(file string) error {
	layers := []config.Layer{{Name: "the engine's base", Data: config.Base}, {Name: "the built-in defaults", Data: e.defaults}}
	names := []string{}
	if len(e.defaults) > 0 {
		names = append(names, "the built-in defaults")
	}
	add := func(path string, required bool) error {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) && !required {
			return nil
		}
		if err != nil {
			return err
		}
		abs, _ := filepath.Abs(path)
		layers = append(layers, config.Layer{Name: abs, Data: data, Dir: filepath.Dir(abs)})
		names = append(names, abs)
		return nil
	}
	if err := add(userConfigPath(e.home), false); err != nil {
		return err
	}
	if file != "" {
		if err := add(file, true); err != nil {
			return err
		}
	}
	c, err := config.Layered(layers...)
	if err != nil {
		return err
	}
	e.config, e.configFrom = c, names
	return nil
}

// ConfigSources name the configuration's layers, in order, once
// LayerConfig has laid them.
func (e *Engine) ConfigSources() []string { return e.configFrom }

// userConfigPath is $XDG_CONFIG_HOME/compa/config.yaml, by default under
// ~/.config.
func userConfigPath(home string) string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "compa", "config.yaml")
}
