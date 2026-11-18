package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// TestHouseSectionInTheCachedPrefix: a configuration's house text reaches
// every call once, in the system prompt, which precedes the first cache
// break, and in nothing after it; a second call repeats it byte for byte;
// and a deployment with none has no such section.
func TestHouseSectionInTheCachedPrefix(t *testing.T) {
	const house = "Support is Example Support.\n\nIts knowledge base is the Example library; `examplectl` reads a live system."
	s, f := companionSession(t, script(
		modelfixture.Calling(call("c1", "grep", map[string]any{"pattern": "(?i)error", "path": "namespaces/openshift-ovn-kubernetes"})),
		modelfixture.Text("Errors are logged."),
	))
	without := s.e.companionSystem()
	if strings.Contains(without, "## This deployment") {
		t.Errorf("a deployment with no house text has the section:\n%s", without)
	}
	s.e.config.Companion.House = house
	if _, err := s.Talk(context.Background(), "What do the OVN logs say?", nil); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	for i, r := range reqs {
		if n := strings.Count(r.System, house); n != 1 {
			t.Errorf("call %d: the system prompt holds the house text %d times, want 1", i+1, n)
		}
		if !strings.HasPrefix(r.System, without) {
			t.Errorf("call %d: the house text is not appended to the prompt without it", i+1)
		}
		breaks := 0
		for j, m := range r.Messages {
			text := m.Text
			for _, res := range m.Results {
				text += res.Text
			}
			if strings.Contains(text, "Example Support") {
				t.Errorf("call %d: message %d holds the house text, past the system prompt", i+1, j)
			}
			if m.Break {
				breaks++
			}
		}
		if breaks == 0 {
			t.Errorf("call %d: no cache break follows the system prompt", i+1)
		}
	}
	if reqs[0].System != reqs[1].System {
		t.Error("the system prompt changed between two calls of one turn")
	}
}

// TestPromptsNameNoOrganization: no prompt file the engine embeds names an
// organization. Who "support" is belongs to a deployment's house text.
func TestPromptsNameNoOrganization(t *testing.T) {
	named := regexp.MustCompile(`(?i)red\s*hat`)
	files := 0
	err := filepath.WalkDir("../internal", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		files++
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, at := range named.FindAllIndex(text, -1) {
			t.Errorf("%s:%d names %q", path, 1+strings.Count(string(text[:at[0]]), "\n"), text[at[0]:at[1]])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The companion's, the customer rules, the ledger's, the editor's
	// and the reviewer's.
	if files < 5 {
		t.Errorf("read %d prompt files under internal/, want at least 5", files)
	}
}
