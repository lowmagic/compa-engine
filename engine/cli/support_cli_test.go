package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic comments exercise the public CLI and the persisted exclusions:
// by support, under the deployment's names for it, and by an author kind
// the engine has no word for, which a source may report all the same.
func TestSupportCommentCutover(t *testing.T) {
	e := newEnv(t)
	data := "id: '00000001'\ncomments:\n  - {id: support-note, authorKind: support, author: {name: Sam Synthetic}, visibility: private, body: Synthetic support note.}\n  - {id: customer-note, authorKind: customer, author: {name: Jo Synthetic}, visibility: public, body: Synthetic customer note.}\n  - {id: partner-note, authorKind: partner, author: {name: Pat Synthetic}, visibility: public, body: Synthetic partner note.}\n"
	if err := os.WriteFile(filepath.Join(e.stub, "00000001.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := append(append([]byte(nil), hostConfig...), []byte("\ncompanion:\n  supportName: Example Support\n  supportRole: Example Agent\n")...)
	e.runWith(cfg, "case", "open", "00000001")
	out := e.runWith(cfg, "case", "comments", "00000001", "--exclude-support")
	// The kind column is as wide as the longest label, so the authors line up.
	for _, want := range []string{"models read 2 of 3 comments", "every comment by Example Support", "PRIVATE EXAMPLE AGENT Sam Synthetic", "PUBLIC  CUSTOMER      Jo Synthetic", "PUBLIC  PARTNER       Pat Synthetic"} {
		if !strings.Contains(out, want) {
			t.Errorf("--exclude-support output lacks %q:\n%s", want, out)
		}
	}
	if out := e.runWith(cfg, "case", "comments", "00000001", "--show"); !strings.Contains(out, "PRIVATE EXAMPLE AGENT Sam Synthetic") || !strings.Contains(out, "PUBLIC  CUSTOMER      Jo Synthetic") || !strings.Contains(out, "PUBLIC  PARTNER       Pat Synthetic") {
		t.Errorf("--show lost an author kind's label:\n%s", out)
	}
	out = e.runWith(cfg, "case", "comments", "00000001", "--exclude-kind", "partner")
	for _, want := range []string{"models read 1 of 3 comments", "every comment whose author kind is partner"} {
		if !strings.Contains(out, want) {
			t.Errorf("--exclude-kind partner output lacks %q:\n%s", want, out)
		}
	}
}
