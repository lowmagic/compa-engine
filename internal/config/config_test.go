package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/expert"
)

const sample = `apiVersion: compa.lowmag.cc/config/v1alpha1
kind: Config
experts:
  bundled: experts
  local: [/abs/override, ~/mine]
  snapshots: .compa/experts
routingLibrary: routing/library.yaml
collection:
  allowlist:
    find: {path: /usr/bin/find}
    awk: {path: /usr/bin/awk, deny: [--posix], replaceDefaultRules: true}
  exclude: [.statedir, .claude]
  limits: {outputBytes: 2048, timeoutSeconds: 5}
reasoning:
  maxEgressClass: internal_network
  models:
    gemini-3.8-flash: {provider: vertex, model: gemini-3.8-flash}
    claude-haiku-4-5: {provider: vertex, model: claude-haiku-4-5}
  profiles:
    companion: {model: gemini-3.8-flash}
    spare: {model: claude-haiku-4-5}
    ledger: {model: claude-haiku-4-5}
    editor: {model: gemini-3.8-flash}
    reviewer: {model: claude-haiku-4-5}
storage: {homeRetentionDays: 30}
`

func TestSupportName(t *testing.T) {
	for _, tc := range []struct {
		name, extra, want string
	}{
		{"omitted", "", "Support"},
		{"empty companion", "companion: {}", "Support"},
		{"configured", "companion: {supportName: Example Support}", "Example Support"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"+tc.extra+"\n"), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if got := c.SupportName(); got != tc.want {
				t.Errorf("SupportName() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (&Config{}).SupportName(); got != "Support" {
		t.Errorf("zero config SupportName() = %q", got)
	}
}

func TestSupportNameValidation(t *testing.T) {
	for _, value := range []string{`""`, `"line\nbreak"`, `"line\rbreak"`, `123`, `"` + strings.Repeat("a", 101) + `"`} {
		t.Run(value, func(t *testing.T) {
			_, err := Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\ncompanion:\n  supportName: "+value+"\n"), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "supportName") {
				t.Fatalf("want supportName validation error, got %v", err)
			}
		})
	}
}

// The label beside a support author's comment is its own field: unset it
// is the generic one whatever the organization's name, and it is held to
// the name's bounds.
func TestSupportRole(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	c, err := Parse([]byte(head+"companion: {supportName: Example Support}\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SupportRole(); got != "Support" {
		t.Errorf("SupportRole() with only a name = %q, want Support", got)
	}
	c, err = Parse([]byte(head+"companion: {supportName: Example Support, supportRole: Example Agent}\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.SupportRole() != "Example Agent" || c.SupportName() != "Example Support" {
		t.Errorf("SupportRole() = %q, SupportName() = %q", c.SupportRole(), c.SupportName())
	}
	for _, value := range []string{`""`, `"line\nbreak"`, `123`} {
		if _, err := Parse([]byte(head+"companion:\n  supportRole: "+value+"\n"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "supportRole") {
			t.Errorf("supportRole %s: want a validation error, got %v", value, err)
		}
	}
}

// The house text is taken as written, its line breaks kept, and one of
// white space alone is refused rather than making an empty section.
func TestHouse(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	c, err := Parse([]byte(head+"companion:\n  house: |\n    Support is Example Support.\n\n    - `examplectl` reads a live system.\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := "Support is Example Support.\n\n- `examplectl` reads a live system.\n"; c.Companion.House != want {
		t.Errorf("House = %q, want %q", c.Companion.House, want)
	}
	for _, value := range []string{`""`, `" \n "`, `123`} {
		if _, err := Parse([]byte(head+"companion:\n  house: "+value+"\n"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "house") {
			t.Errorf("house %s: want a validation error, got %v", value, err)
		}
	}
	if c, err := Parse([]byte(head), t.TempDir()); err != nil || c.Companion.House != "" {
		t.Errorf("no house: %q, %v", c.Companion.House, err)
	}
}

// TestChecksCommands: checks.commands takes program names, a hyphen among
// them, and refuses one with a space, a path or a repeat, which could
// never open a command line.
func TestChecksCommands(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	c, err := Parse([]byte(head+"checks: {commands: [examplectl, must-gather]}\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Checks.Commands, " "); got != "examplectl must-gather" {
		t.Errorf("Commands = %q", got)
	}
	for _, value := range []string{`["example ctl"]`, `[/usr/bin/examplectl]`, `[examplectl, examplectl]`} {
		if _, err := Parse([]byte(head+"checks: {commands: "+value+"}\n"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "commands") {
			t.Errorf("commands %s: want a validation error, got %v", value, err)
		}
	}
}

// TestOpenAI: the openai endpoint defaults to OpenRouter's base URL and the
// external_provider class, takes the class and key variable a deployment
// declares, and refuses a class it does not know, a key variable that is
// no variable's name, and a base URL that is not one.
func TestOpenAI(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	c, err := Parse([]byte(head), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if o := c.OpenAISettings(); o.BaseURL != "https://openrouter.ai/api/v1" || o.EgressClass != "external_provider" || o.APIKeyEnv != "" || o.CacheMarks {
		t.Errorf("defaults: %+v", o)
	}
	c, err = Parse([]byte(head+"reasoning:\n  openai: {baseURL: \"http://192.0.2.10:8000/v1\", apiKeyEnv: EXAMPLE_KEY, egressClass: internal_network, cacheMarks: true}\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if o := c.OpenAISettings(); o.BaseURL != "http://192.0.2.10:8000/v1" || o.EgressClass != "internal_network" || o.APIKeyEnv != "EXAMPLE_KEY" || !o.CacheMarks {
		t.Errorf("declared: %+v", o)
	}
	for _, bad := range []string{"egressClass: anywhere", "apiKeyEnv: sk-or-v1-0000", "baseURL: openrouter.ai"} {
		if _, err := Parse([]byte(head+"reasoning:\n  openai: {"+bad+"}\n"), t.TempDir()); err == nil {
			t.Errorf("openai {%s}: no error", bad)
		}
	}
}

func TestParse(t *testing.T) {
	c, err := Parse([]byte(sample), "/etc/compa")
	if err != nil {
		t.Fatal(err)
	}
	roots, warnings := c.ExpertRoots()
	home, _ := os.UserHomeDir()
	want := []expert.Root{{Path: "/etc/compa/experts", Source: expert.Bundled}, {Path: "/abs/override", Source: expert.Local}, {Path: filepath.Join(home, "mine"), Source: expert.Local}}
	if len(roots) != 3 {
		t.Fatalf("roots = %+v", roots)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Errorf("roots[%d] = %+v, want %+v", i, roots[i], want[i])
		}
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "overrides bundled experts") {
		t.Errorf("warnings = %v", warnings)
	}
	if c.SnapshotDir() != "/etc/compa/.compa/experts" || c.RoutingLibraryPath() != "/etc/compa/routing/library.yaml" {
		t.Errorf("paths: %s %s", c.SnapshotDir(), c.RoutingLibraryPath())
	}

	if got := strings.Join(c.Collection.Exclude, ","); got != ".statedir,.claude" {
		t.Errorf("exclude = %s", got)
	}

	p := c.Policy()
	if _, err := p.Check([]string{"find", ".", "-exec", "x"}); err == nil {
		t.Error("find keeps its default rules")
	}
	if _, err := p.Check([]string{"awk", `BEGIN{system("id")}`}); err != nil {
		t.Errorf("replaceDefaultRules drops the defaults: %v", err)
	}
	if _, err := p.Check([]string{"awk", "--posix"}); err == nil {
		t.Error("configured deny must apply")
	}
	if l := c.CollectLimits(); l.OutputBytes != 2048 || l.Timeout != 5*time.Second {
		t.Errorf("limits = %+v", l)
	}

	if c.HomeRetention() != 30*24*time.Hour {
		t.Errorf("home retention %s", c.HomeRetention())
	}
	if c.MaxEgressClass() != "internal_network" {
		t.Errorf("egress %s", c.MaxEgressClass())
	}
}

// The routing library is configured as routingLibrary and resolved against
// the configuration's directory; the name it replaced is an unknown field.
func TestRoutingLibrary(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	c, err := Parse([]byte(head+"routingLibrary: lib/keys.yaml\n"), "/etc/compa")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RoutingLibraryPath(); got != "/etc/compa/lib/keys.yaml" {
		t.Errorf("RoutingLibraryPath() = %q", got)
	}
	if c, err = Parse([]byte(head), "/etc/compa"); err != nil || c.RoutingLibraryPath() != "" {
		t.Errorf("unset: %q, %v", c.RoutingLibraryPath(), err)
	}
	if _, err := Parse([]byte(head+"sbrLibrary: lib/keys.yaml\n"), "/etc/compa"); err == nil || !strings.Contains(err.Error(), "additional properties 'sbrLibrary' not allowed") {
		t.Fatalf("want the old field refused, got %v", err)
	}
}

// The companion's profile is its default class's, as the configured
// profiles map it to a model, its source the configuration, and the
// ledger's, the editor's and the reviewer's likewise on their own classes;
// a task type with no class has none, and a class no profile names, as the
// editor's or the reviewer's where none is configured, none, its source the
// default.
func TestProfile(t *testing.T) {
	c, err := Parse([]byte(sample), "/")
	if err != nil {
		t.Fatal(err)
	}
	if pc := c.Profile("companion"); pc.Source != "configuration" || pc.Class != "companion" || !pc.Defined ||
		pc.ModelName != "gemini-3.8-flash" || pc.Provider != "vertex" || pc.Model != "gemini-3.8-flash" {
		t.Errorf("companion: %+v", pc)
	}
	if pc := c.Profile("comments.ledger"); pc.Class != "ledger" || !pc.Defined || pc.Model != "claude-haiku-4-5" {
		t.Errorf("the ledger: %+v", pc)
	}
	if pc := c.Profile("answers.edit"); pc.Class != "editor" || !pc.Defined || pc.Model != "gemini-3.8-flash" {
		t.Errorf("the editor: %+v", pc)
	}
	if pc := c.Profile("answers.review"); pc.Class != "reviewer" || !pc.Defined || pc.ModelName != "claude-haiku-4-5" {
		t.Errorf("the reviewer: %+v", pc)
	}
	if pc := c.Profile("analysis.diagnose"); pc.Defined {
		t.Errorf("a task type with no class has a model: %+v", pc)
	}
	bare, err := Parse([]byte(strings.NewReplacer("    editor: {model: gemini-3.8-flash}\n", "", "    reviewer: {model: claude-haiku-4-5}\n", "").Replace(sample)), "/")
	if err != nil {
		t.Fatal(err)
	}
	if pc := bare.Profile("answers.edit"); pc.Class != "editor" || pc.Defined || pc.Source != "default" {
		t.Errorf("the editor with no profile: %+v", pc)
	}
	if pc := bare.Profile("answers.review"); pc.Class != "reviewer" || pc.Defined {
		t.Errorf("the reviewer with no profile: %+v", pc)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"), "/")
	if err != nil {
		t.Fatal(err)
	}
	if c.HomeRetention() != DefaultHomeRetention {
		t.Errorf("default home retention %s", c.HomeRetention())
	}
	if c.MaxEgressClass() != "local_only" || len(c.Policy().Binaries) != 0 {
		t.Errorf("defaults: %s %v", c.MaxEgressClass(), c.Policy())
	}
	if roots, _ := c.ExpertRoots(); len(roots) != 0 {
		t.Errorf("roots = %v", roots)
	}
}

func TestParseRejects(t *testing.T) {
	for name, doc := range map[string]string{
		"wrong version":        "apiVersion: compa.lowmag.cc/config/v2\nkind: Config\n",
		"unknown field":        "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nharness: pi\n",
		"bad deny pattern":     "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\ncollection: {allowlist: {awk: {path: /usr/bin/awk, denyPatterns: ['(']}}}\n",
		"exclude with a slash": "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\ncollection: {exclude: [a/b]}\n",
		"exclude ..":           "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\ncollection: {exclude: ['..']}\n",
		"tasks, removed":       "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning: {models: {m: {provider: p, model: m}}, profiles: {a: {model: m}}, tasks: {analysis.diagnose: {profile: a}}}\n",
		"harnesses, removed":   "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning: {harnesses: [pi]}\n",
		"enrichment, removed":  "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nenrichment: {summarize: true}\n",
		"undefined model":      "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning: {profiles: {a: {model: m}}}\n",
		"a profile's provider": "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning: {models: {m: {provider: p, model: m}}, profiles: {a: {provider: p, model: m}}}\n",
	} {
		if _, err := Parse([]byte(doc), "/"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// A profile on a model that takes efforts names one it lists, since the
// model's own default is not tuned to this work; one on a model that takes
// none names none. The check holds on the layered configuration, so a
// user's file may name a model the deployment's defaults define.
func TestEffortChecked(t *testing.T) {
	const head = "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"
	const models = "  models:\n" +
		"    standard: {provider: vertex-gemini, model: gemini-3.8-flash, efforts: [low, medium, high]}\n" +
		"    lite: {provider: vertex-gemini, model: gemini-3.5-flash-lite, efforts: [minimal, low, medium, high]}\n" +
		"    claude: {provider: vertex-anthropic, model: claude-haiku-4-5}\n"
	profile := func(p string) string {
		return head + "reasoning:\n" + models + "  profiles:\n    companion: " + p + "\n"
	}
	for _, c := range []struct {
		name, doc string
		ok        bool
	}{
		{"a listed effort", profile("{model: standard, effort: high}"), true},
		{"minimal where listed", profile("{model: lite, effort: minimal}"), true},
		{"none on a model that takes none", profile("{model: claude}"), true},
		{"none on a model that takes efforts", profile("{model: standard}"), false},
		{"default", profile("{model: standard, effort: default}"), false},
		{"an effort the model does not list", profile("{model: standard, effort: minimal}"), false},
		{"an effort on a model that takes none", profile("{model: claude, effort: low}"), false},
		{"no such effort", profile("{model: standard, effort: extreme}"), false},
		{"no such model", profile("{model: gemini-3.8-flash, effort: high}"), false},
	} {
		_, err := Parse([]byte(c.doc), "/")
		if (err == nil) != c.ok {
			t.Errorf("%s: Parse = %v, want ok %v", c.name, err, c.ok)
		}
	}

	layered, err := Layered(
		Layer{Name: "defaults", Data: []byte(profile("{model: lite, effort: medium}"))},
		Layer{Name: "user", Data: []byte(head + "reasoning: {profiles: {companion: {model: lite, effort: low}}}\n")})
	if err != nil {
		t.Fatalf("Layered: %v", err)
	}
	if pc := layered.Profile("companion"); pc.Effort != "low" || pc.ModelName != "lite" || pc.Model != "gemini-3.5-flash-lite" {
		t.Errorf("Profile(companion) = %+v, want lite at low", pc)
	}
	if _, err := Layered(
		Layer{Name: "defaults", Data: []byte(head + "reasoning:\n" + models)},
		Layer{Name: "user", Data: []byte(head + "reasoning: {profiles: {companion: {model: standard, effort: minimal}}}\n")}); err == nil {
		t.Error("Layered: minimal on gemini-3.8-flash accepted")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compa.yaml")
	os.WriteFile(path, []byte(sample), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if roots, _ := c.ExpertRoots(); roots[0].Path != filepath.Join(dir, "experts") {
		t.Errorf("bundled root = %s", roots[0].Path)
	}
}

func TestVertexSettings(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "env-project")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")
	t.Setenv("CLOUD_ML_REGION", "")
	c, err := Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v, notes := c.VertexSettings()
	if v.Project != "env-project" || v.Location != "global" || len(notes) != 1 {
		t.Errorf("fallback: %+v %v", v, notes)
	}
	c, err = Parse([]byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning: {vertex: {project: p, location: us-east5}}\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if v, notes := c.VertexSettings(); v.Project != "p" || v.Location != "us-east5" || len(notes) != 0 {
		t.Errorf("configured: %+v %v", v, notes)
	}
}
