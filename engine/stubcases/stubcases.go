// Package stubcases is a CaseSource over synthetic cases in a directory, one
// YAML file per case, so the engine runs end to end with no case system.
package stubcases

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lowmagic/compa-engine/engine"
)

// Source reads <dir>/<case ID>.yaml on every call, so an edit to a fixture
// is seen the next time a case is read, as a changed comment would be.
type Source struct {
	dir string
}

func New(dir string) *Source { return &Source{dir: dir} }

func (s *Source) Name() string { return "stub" }

type caseFile struct {
	ID               string        `yaml:"id"`
	SourceID         string        `yaml:"sourceId"`
	Subject          string        `yaml:"subject"`
	Description      string        `yaml:"description"`
	Status           string        `yaml:"status"`
	InternalStatus   string        `yaml:"internalStatus"`
	WaitingOnSupport bool          `yaml:"waitingOnSupport"`
	Severity         string        `yaml:"severity"`
	Product          string        `yaml:"product"`
	ProductVersion   string        `yaml:"productVersion"`
	ClusterID        string        `yaml:"clusterId"`
	ClusterVersion   string        `yaml:"clusterVersion"`
	RoutingKeys      []string      `yaml:"routingKeys"`
	Components       []string      `yaml:"components"`
	Tags             []string      `yaml:"tags"`
	Account          partyFile     `yaml:"account"`
	Owner            partyFile     `yaml:"owner"`
	Created          time.Time     `yaml:"created"`
	Updated          time.Time     `yaml:"updated"`
	Comments         []commentFile `yaml:"comments"`
}

type partyFile struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Number string `yaml:"number"`
}

type commentFile struct {
	ID     string    `yaml:"id"`
	Author partyFile `yaml:"author"`
	// AuthorKind is who wrote the comment, such as support or customer,
	// as a source reports it; empty when the fixture does not say, as a
	// source may not.
	AuthorKind string    `yaml:"authorKind"`
	Automated  bool      `yaml:"automated"`
	Visibility string    `yaml:"visibility"`
	Created    time.Time `yaml:"created"`
	Updated    time.Time `yaml:"updated"`
	Body       string    `yaml:"body"`
	// Synthetic is the fixture's own mark: it is accepted, is not part of
	// a comment and reaches none, and the source does not report it.
	Synthetic bool `yaml:"synthetic"`
}

func (p partyFile) party() engine.Party {
	return engine.Party{ID: p.ID, Name: p.Name, Number: p.Number}
}

func (s *Source) Case(ctx context.Context, id string) (engine.Case, error) {
	f, err := s.read(ctx, id)
	if err != nil {
		return engine.Case{}, err
	}
	return engine.Case{
		ID: f.ID, SourceID: f.SourceID,
		Subject: f.Subject, Description: f.Description,
		Status: f.Status, InternalStatus: f.InternalStatus, WaitingOnSupport: f.WaitingOnSupport,
		Severity: f.Severity,
		Product:  f.Product, ProductVersion: f.ProductVersion,
		ClusterID: f.ClusterID, ClusterVersion: f.ClusterVersion,
		RoutingKeys: f.RoutingKeys, Components: f.Components, Tags: f.Tags,
		Account: f.Account.party(), Owner: f.Owner.party(),
		Created: f.Created, Updated: f.Updated,
	}, nil
}

func (s *Source) Comments(ctx context.Context, id string) ([]engine.Comment, error) {
	f, err := s.read(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Comment, len(f.Comments))
	for i, c := range f.Comments {
		out[i] = engine.Comment{
			ID: c.ID, Author: c.Author.party(), AuthorKind: c.AuthorKind, Automated: c.Automated,
			Visibility: engine.Visibility(c.Visibility), Created: c.Created, Updated: c.Updated, Body: c.Body,
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.Before(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Source) read(ctx context.Context, id string) (caseFile, error) {
	if err := ctx.Err(); err != nil {
		return caseFile{}, err
	}
	if err := engine.CheckCaseID(id); err != nil {
		return caseFile{}, err
	}
	path := filepath.Join(s.dir, id+".yaml")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return caseFile{}, fmt.Errorf("case %s: %w in the stub source (no %s)", id, engine.ErrCaseNotFound, path)
	}
	if err != nil {
		return caseFile{}, fmt.Errorf("case %s: %w", id, err)
	}
	var f caseFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return caseFile{}, fmt.Errorf("case %s: %s: %w", id, path, err)
	}
	if f.ID != id {
		return caseFile{}, fmt.Errorf("case %s: %s says its ID is %q", id, path, f.ID)
	}
	seen := map[string]bool{}
	for i, c := range f.Comments {
		if c.ID == "" || seen[c.ID] {
			return caseFile{}, fmt.Errorf("case %s: %s: comment %d has an empty or repeated ID %q", id, path, i, c.ID)
		}
		seen[c.ID] = true
		switch engine.Visibility(c.Visibility) {
		case engine.VisibilityPrivate, engine.VisibilityPartner, engine.VisibilityPublic:
		default:
			return caseFile{}, fmt.Errorf("case %s: %s: comment %s has visibility %q, want private, partner or public", id, path, c.ID, c.Visibility)
		}
	}
	return f, nil
}
