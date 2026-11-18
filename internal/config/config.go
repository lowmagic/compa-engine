// Package config loads the engine's versioned, typed configuration
// (compa.lowmag.cc/config/v1alpha1).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/schema"
)

// Config mirrors the configuration schema.
type Config struct {
	APIVersion     string     `json:"apiVersion"`
	Kind           string     `json:"kind"`
	Experts        Experts    `json:"experts"`
	RoutingLibrary string     `json:"routingLibrary,omitempty"`
	Collection     Collection `json:"collection"`
	Reasoning      Reasoning  `json:"reasoning"`
	Storage        Storage    `json:"storage"`
	Comments       Comments   `json:"comments"`
	Companion      Companion  `json:"companion"`
	Checks         Checks     `json:"checks"`

	// dir is the configuration file's directory; relative paths resolve
	// against it.
	dir string
}

// Checks are the answer checks' settings.
type Checks struct {
	// Commands are the programs, beside the engine's own, whose command
	// lines an answer gives to be run, not claims to look for.
	Commands []string `json:"commands,omitempty"`
}

// Companion names the support side in the companion's presentation, and
// holds the deployment's house section of the system prompt.
type Companion struct {
	SupportName string `json:"supportName,omitempty"`
	SupportRole string `json:"supportRole,omitempty"`
	// House is free text the system prompt carries as a section of its
	// own: who "support" is, the deployment's knowledge sources and its
	// tools' conventions.
	House string `json:"house,omitempty"`
}

// SupportName returns the deployment's support name, or the generic label.
func (c *Config) SupportName() string {
	if c.Companion.SupportName != "" {
		return c.Companion.SupportName
	}
	return "Support"
}

// SupportRole returns what the deployment calls a support author as a
// person, such as an agent, or the generic label.
func (c *Config) SupportRole() string {
	if c.Companion.SupportRole != "" {
		return c.Companion.SupportRole
	}
	return "Support"
}

type Experts struct {
	Bundled   string   `json:"bundled,omitempty"`
	Local     []string `json:"local,omitempty"`
	Snapshots string   `json:"snapshots,omitempty"`
}

type Collection struct {
	Allowlist map[string]AllowEntry `json:"allowlist,omitempty"`
	// Exclude names entries directly under an artifact root that collection
	// never reads, beside the case store, which is always excluded.
	Exclude []string `json:"exclude,omitempty"`
	Limits  Limits   `json:"limits"`
}

type AllowEntry struct {
	Path                string   `json:"path"`
	Deny                []string `json:"deny,omitempty"`
	DenyPatterns        []string `json:"denyPatterns,omitempty"`
	ReplaceDefaultRules bool     `json:"replaceDefaultRules,omitempty"`
}

type Limits struct {
	OutputBytes    int `json:"outputBytes,omitempty"`
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// MaxTimeoutSeconds is the most a collection may ask to be allowed;
	// five minutes when unset.
	MaxTimeoutSeconds int   `json:"maxTimeoutSeconds,omitempty"`
	MaxFileBytes      int64 `json:"maxFileBytes,omitempty"`
	MaxLineBytes      int   `json:"maxLineBytes,omitempty"`
}

// Comments are the intentions for drafting case comments and the house
// style: the engine's base layer gives a general set (Base), and a layer
// naming either replaces it whole.
type Comments struct {
	Style      string      `json:"style,omitempty"`
	Intentions []Intention `json:"intentions,omitempty"`
}

// Intention is what a comment draft is for: its label, the audience — the
// customer or an internal note — and the purpose a model drafts it to.
type Intention struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Audience string `json:"audience"`
	Purpose  string `json:"purpose"`
}

type Storage struct {
	// HomeRetentionDays is how long a case stored under the engineer's
	// home directory, for want of a case folder, is kept unused.
	HomeRetentionDays int `json:"homeRetentionDays,omitempty"`
}

// DefaultHomeRetention is how long an unused case under the engineer's
// home directory is kept (docs/SPEC.md §2).
const DefaultHomeRetention = 14 * 24 * time.Hour

func (c *Config) HomeRetention() time.Duration {
	if d := c.Storage.HomeRetentionDays; d > 0 {
		return time.Duration(d) * 24 * time.Hour
	}
	return DefaultHomeRetention
}

type Reasoning struct {
	MaxEgressClass string             `json:"maxEgressClass,omitempty"`
	Vertex         Vertex             `json:"vertex,omitempty"`
	OpenAI         OpenAI             `json:"openai,omitempty"`
	Profiles       map[string]Profile `json:"profiles,omitempty"`
	Models         map[string]Model   `json:"models,omitempty"`
	BarredAccounts *BarredAccounts    `json:"barredAccounts,omitempty"`
}

// OpenAI is the OpenAI-compatible endpoint the openai provider calls.
type OpenAI struct {
	// BaseURL is the endpoint's, up to /chat/completions; OpenRouter's
	// where unset.
	BaseURL string `json:"baseURL,omitempty"`
	// APIKeyEnv names the environment variable holding its API key; unset,
	// no key is sent.
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
	// EgressClass is where the endpoint's calls go, as the deployment
	// declares it; external_provider where unset.
	EgressClass string `json:"egressClass,omitempty"`
	// CacheMarks sends the engine's cache breaks as cache_control marks,
	// which OpenRouter passes to the models that take them.
	CacheMarks bool `json:"cacheMarks,omitempty"`
}

// BarredAccounts names the deployment's list of accounts barred from AI
// processing, for which the engine makes no model call, and what it tells
// the engineer before they override a check that could not be made.
type BarredAccounts struct {
	// List is the file: one account number a line, blank lines and #
	// comments ignored.
	List string `json:"list"`
	// OverrideInstructions is shown before an override, so the engine
	// names no customer class itself.
	OverrideInstructions string `json:"overrideInstructions,omitempty"`
}

type Vertex struct {
	Project  string `json:"project,omitempty"`
	Location string `json:"location,omitempty"`
	// CacheMinutes is how long an explicit Gemini cache lives; zero is the
	// provider's default.
	CacheMinutes int `json:"cacheMinutes,omitempty"`
}

// OpenAISettings is the openai provider's endpoint, its defaults filled
// in: OpenRouter's base URL and the external_provider class.
func (c *Config) OpenAISettings() OpenAI {
	o := c.Reasoning.OpenAI
	if o.BaseURL == "" {
		o.BaseURL = "https://openrouter.ai/api/v1"
	}
	if o.EgressClass == "" {
		o.EgressClass = "external_provider"
	}
	return o
}

// Profile is a task class's model, by its name under reasoning.models, and
// the effort its calls send.
type Profile struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

// Model is a model the profiles use: its provider, its ID there and the
// efforts it takes. The efforts are checked live: a model refuses one it
// does not take, as gemini-3.8-flash refuses minimal. A model that lists
// none is sent none.
type Model struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Efforts  []string `json:"efforts,omitempty"`
}

// Load reads, validates and checks a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Parse decodes configuration YAML; relative paths resolve against dir.
func Parse(data []byte, dir string) (*Config, error) {
	doc, err := schema.DecodeYAML(data)
	if err == nil {
		err = schema.Validate(schema.Config, doc)
	}
	if err != nil {
		return nil, err
	}
	j, _ := json.Marshal(doc)
	c := &Config{dir: dir}
	if err := json.Unmarshal(j, c); err != nil {
		return nil, err
	}
	for name, e := range c.Collection.Allowlist {
		for _, p := range e.DenyPatterns {
			if _, err := regexp.Compile(p); err != nil {
				return nil, fmt.Errorf("collection.allowlist.%s: deny pattern %q: %v", name, p, err)
			}
		}
	}
	for _, name := range c.Collection.Exclude {
		if err := collect.CheckExclude(name); err != nil {
			return nil, fmt.Errorf("collection.exclude: %v", err)
		}
	}
	// A model that takes efforts is always sent one: its own default is
	// not tuned to this work.
	for name, p := range c.Reasoning.Profiles {
		m, ok := c.Reasoning.Models[p.Model]
		switch {
		case !ok:
			return nil, fmt.Errorf("reasoning.profiles.%s: model %q is not defined (reasoning.models)", name, p.Model)
		case len(m.Efforts) == 0 && p.Effort != "":
			return nil, fmt.Errorf("reasoning.profiles.%s: effort %q: model %s takes no effort", name, p.Effort, p.Model)
		case len(m.Efforts) > 0 && !slices.Contains(m.Efforts, p.Effort):
			return nil, fmt.Errorf("reasoning.profiles.%s: effort %q: model %s takes %s", name, p.Effort, p.Model, strings.Join(m.Efforts, ", "))
		}
	}
	return c, nil
}

// resolve expands ~ and makes a path absolute against the config directory.
func (c *Config) resolve(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.dir, p)
	}
	return filepath.Clean(p)
}

// ExpertRoots returns the expert roots in precedence order: the bundled
// set, then local overrides. Local packages override bundled ones with a
// warning from discovery; each active local root is also reported here so
// the engineer always sees that overrides are in effect.
func (c *Config) ExpertRoots() ([]expert.Root, []string) {
	var roots []expert.Root
	var warnings []string
	if c.Experts.Bundled != "" {
		roots = append(roots, expert.Root{Path: c.resolve(c.Experts.Bundled), Source: expert.Bundled})
	}
	for _, l := range c.Experts.Local {
		p := c.resolve(l)
		roots = append(roots, expert.Root{Path: p, Source: expert.Local})
		warnings = append(warnings, fmt.Sprintf("local expert root %s is active and overrides bundled experts of the same ID and version", p))
	}
	return roots, warnings
}

// SnapshotDir is where pinned experts are snapshotted; empty if unset.
func (c *Config) SnapshotDir() string {
	if c.Experts.Snapshots == "" {
		return ""
	}
	return c.resolve(c.Experts.Snapshots)
}

// BarredAccountsPath is the resolved path of the list of accounts barred
// from AI processing; empty where none is configured, which turns the
// rule off.
func (c *Config) BarredAccountsPath() string {
	if c.Reasoning.BarredAccounts == nil || c.Reasoning.BarredAccounts.List == "" {
		return ""
	}
	return c.resolve(c.Reasoning.BarredAccounts.List)
}

// RoutingLibraryPath is the resolved routing library path; empty if unset.
func (c *Config) RoutingLibraryPath() string {
	if c.RoutingLibrary == "" {
		return ""
	}
	return c.resolve(c.RoutingLibrary)
}

// Policy builds the command policy. Each entry starts from the default
// argument rules for its binary, adds the configured ones, or replaces
// them when replaceDefaultRules is set.
func (c *Config) Policy() collect.Policy {
	p := collect.Policy{Binaries: map[string]collect.Allowed{}}
	for name, e := range c.Collection.Allowlist {
		rules := collect.ArgRules{}
		if !e.ReplaceDefaultRules {
			d := collect.DefaultArgRules[filepath.Base(e.Path)]
			rules.Deny = append(rules.Deny, d.Deny...)
			rules.DenyPatterns = append(rules.DenyPatterns, d.DenyPatterns...)
		}
		rules.Deny = append(rules.Deny, e.Deny...)
		rules.DenyPatterns = append(rules.DenyPatterns, e.DenyPatterns...)
		p.Binaries[name] = collect.Allowed{Path: e.Path, Rules: rules}
	}
	return p
}

// CollectLimits fills unset values with defaults.
func (c *Config) CollectLimits() collect.Limits {
	l := c.Collection.Limits
	return collect.Limits{
		OutputBytes:  l.OutputBytes,
		Timeout:      time.Duration(l.TimeoutSeconds) * time.Second,
		MaxFileBytes: l.MaxFileBytes,
		MaxLineBytes: l.MaxLineBytes,
	}
}

// MaxEgressClass is the widest egress configured; local_only by default.
func (c *Config) MaxEgressClass() string {
	if c.Reasoning.MaxEgressClass == "" {
		return "local_only"
	}
	return c.Reasoning.MaxEgressClass
}

// DefaultProfileClass is each task type's default model-profile class
// (docs/TECH_STACK.md): the companion's conversation and its consults on
// companion, the case ledger's calls on ledger, the editor's check of
// each answer on editor, and the reviewer's second opinion on reviewer.
var DefaultProfileClass = map[string]string{
	"companion":       "companion",
	"comments.ledger": "ledger",
	"answers.edit":    "editor",
	"answers.review":  "reviewer",
}

// ProfileChoice is a resolved model profile and where the choice came from.
type ProfileChoice struct {
	Class  string
	Source string // configuration, engineer (WithModel) or default where none is configured
	// ModelName is the model's name under reasoning.models; Provider and
	// Model are its provider and its ID there.
	ModelName, Provider, Model string
	// Effort is what the profile's calls send, or nothing for a model
	// that takes none.
	Effort string
	// Defined is false when no profile mapping names a model for Class.
	Defined bool
}

// Profile resolves a task type's model profile: its default class, as
// the configured profiles map it to a model.
func (c *Config) Profile(task string) ProfileChoice {
	pc := ProfileChoice{Class: DefaultProfileClass[task], Source: "default"}
	p, ok := c.Reasoning.Profiles[pc.Class]
	if !ok {
		return pc
	}
	m := c.Reasoning.Models[p.Model]
	pc.Source, pc.ModelName, pc.Provider, pc.Model, pc.Effort, pc.Defined = "configuration", p.Model, m.Provider, m.Model, p.Effort, true
	return pc
}

// WithModel is a profile choice with the engineer's model and effort in
// place of its own: the model by its name under reasoning.models, the
// effort one it lists, or, left empty, the profile's own where the model
// lists it. A model that takes no effort takes none. Its source is the
// engineer.
func (c *Config) WithModel(pc ProfileChoice, model, effort string) (ProfileChoice, error) {
	m, ok := c.Reasoning.Models[model]
	if !ok {
		var names []string
		for n := range c.Reasoning.Models {
			names = append(names, n)
		}
		sort.Strings(names)
		return pc, fmt.Errorf("no model %q is configured; the models are %s", model, strings.Join(names, ", "))
	}
	switch {
	case len(m.Efforts) == 0 && effort != "":
		return pc, fmt.Errorf("%s takes no effort", model)
	case len(m.Efforts) == 0:
	case effort == "" && slices.Contains(m.Efforts, pc.Effort):
		effort = pc.Effort
	case effort == "":
		return pc, fmt.Errorf("say the effort for %s: %s", model, strings.Join(m.Efforts, ", "))
	case !slices.Contains(m.Efforts, effort):
		return pc, fmt.Errorf("%s takes the efforts %s, not %s", model, strings.Join(m.Efforts, ", "), effort)
	}
	pc.Source, pc.ModelName, pc.Provider, pc.Model, pc.Effort, pc.Defined = "engineer", model, m.Provider, m.Model, effort, true
	return pc, nil
}

// VertexSettings returns the Vertex project and location: from
// configuration, else from the environment variables the Google and
// Anthropic tools use, with the location defaulting to global. Each
// fallback taken is reported, so the engineer sees where they came from.
func (c *Config) VertexSettings() (Vertex, []string) {
	v := c.Reasoning.Vertex
	var notes []string
	if v.Project == "" {
		for _, env := range []string{"GOOGLE_CLOUD_PROJECT", "ANTHROPIC_VERTEX_PROJECT_ID"} {
			if p := os.Getenv(env); p != "" {
				v.Project = p
				notes = append(notes, "reasoning.vertex.project is unset; using "+env)
				break
			}
		}
	}
	if v.Location == "" {
		for _, env := range []string{"GOOGLE_CLOUD_LOCATION", "CLOUD_ML_REGION"} {
			if l := os.Getenv(env); l != "" {
				v.Location = l
				notes = append(notes, "reasoning.vertex.location is unset; using "+env)
				break
			}
		}
	}
	if v.Location == "" {
		v.Location = "global"
	}
	return v, notes
}
