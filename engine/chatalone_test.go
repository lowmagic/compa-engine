package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// chatEngine is an engine whose companion is the fixture provider, with
// the deployment's barred-accounts list at list where one is given.
func chatEngine(t *testing.T, list string) (*Engine, *modelfixture.Provider) {
	t.Helper()
	barred := ""
	if list != "" {
		barred = "  barredAccounts: {list: " + list + "}\n"
	}
	e, err := New(Options{Cases: StubCases("../testdata/cases"), Artifacts: LocalDir(t.TempDir()), Home: t.TempDir(), Config: []byte("apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nreasoning:\n  maxEgressClass: local_only\n" + barred +
		"  models:\n    fixture-model: {provider: fixture, model: fixture-model}\n  profiles:\n    companion: {model: fixture-model}\n")})
	if err != nil {
		t.Fatal(err)
	}
	f := modelfixture.New()
	e.models = map[string]model.Provider{"fixture": f}
	return e, f
}

// TestChatOverADirectoryUnderAList: a troubleshooting chat with no case
// holds no case's data, so its calls go out under a barred-accounts list;
// over a directory of artifacts, whose account cannot be checked, the list
// refuses every call, and there is no override to give. Without a list it
// runs.
func TestChatOverADirectoryUnderAList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "journal.log"), []byte("synthetic line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, f := chatEngine(t, barredList(t, "1000000001"))
	alone, err := e.StartChat(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer alone.Close()
	if _, err := alone.Talk(ctx, "How do I read a vmcore?", nil); err != nil {
		t.Errorf("a chat with no directory: %v", err)
	}
	over, err := e.StartChat(ctx, "dir-chat", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer over.Close()
	calls := len(f.Requests())
	if _, err := over.Talk(ctx, "What does the journal say?", nil); !errors.Is(err, companion.ErrBarred) {
		t.Errorf("a chat over a directory under a list: %v, want refused", err)
	}
	if n := len(f.Requests()) - calls; n != 0 {
		t.Errorf("the model was called %d times for the directory", n)
	}

	free, f2 := chatEngine(t, "")
	open, err := free.StartChat(ctx, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if _, err := open.Talk(ctx, "What does the journal say?", nil); err != nil || len(f2.Requests()) != 1 {
		t.Errorf("with no list: %v, %d calls", err, len(f2.Requests()))
	}
}

// A chat's name names its store's directory, so it is checked as a case ID
// is: a name that would reach outside the chats' directory, or name none,
// is refused before anything is made.
func TestChatNameIsChecked(t *testing.T) {
	e, _ := chatEngine(t, "")
	for _, name := range []string{"../escaped", "..", "a/b", "-x", "a\\b"} {
		s, err := e.StartChat(context.Background(), name, "")
		if err == nil {
			s.Close()
		}
		if !errors.Is(err, ErrInvalidCaseID) {
			t.Errorf("chat %q: %v, want refused as an invalid name", name, err)
		}
	}
	share := filepath.Join(e.home, ".local", "share", "compa")
	if entries, _ := os.ReadDir(share); len(entries) != 0 {
		t.Errorf("refused names left %v in %s", entries, share)
	}
}

// A chat's directory is kept with it by its real path: the chat resumed
// without --dir is over it again, and with the same directory, through a
// link too, it opens; with another, or with one where the chat began
// without, it is refused, so a chat's evidence never mixes two roots.
func TestChatKeepsItsDirectory(t *testing.T) {
	ctx := context.Background()
	target, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	e, _ := chatEngine(t, "")
	root := func(name, dir string) (string, error) {
		t.Helper()
		s, err := e.StartChat(ctx, name, dir)
		if err != nil {
			return "", err
		}
		defer s.Close()
		return s.c.root, nil
	}

	if got, err := root("over-a-link", link); err != nil || got != target {
		t.Errorf("started over a link: root %q, %v; want %q", got, err, target)
	}
	if got, err := root("over-a-link", ""); err != nil || got != target {
		t.Errorf("resumed without --dir: root %q, %v; want %q", got, err, target)
	}
	if got, err := root("over-a-link", target); err != nil || got != target {
		t.Errorf("resumed with the same directory: root %q, %v", got, err)
	}
	if _, err := root("over-a-link", other); err == nil || !strings.Contains(err.Error(), target) {
		t.Errorf("resumed over another directory: %v, want refused naming %s", err, target)
	}
	if got, err := root("over-a-link", ""); err != nil || got != target {
		t.Errorf("after a refusal: root %q, %v; want %q", got, err, target)
	}

	if got, err := root("no-dir", ""); err != nil || got != "" {
		t.Fatalf("started with no directory: root %q, %v", got, err)
	}
	if _, err := root("no-dir", other); err == nil {
		t.Error("a chat begun with no directory resumed over one")
	}
}
