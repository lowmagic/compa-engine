package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/model"
	modelfixture "github.com/lowmagic/compa-engine/internal/model/fixture"
)

// TestIngestServer: a session holding its store serves it to compa in
// over a socket only its owner can reach, in a directory of its own, its
// path named in the store; what is sent is kept as the engineer's
// evidence with its note, named for the conversation, the same content
// from the same place twice one item, and taken once; a request not
// compa in's is refused; closed, the socket and the pointer are gone.
func TestIngestServer(t *testing.T) {
	s, _ := companionSession(t, func(context.Context, model.Request) (*modelfixture.Answer, error) {
		return nil, errors.New("no model call is expected")
	})
	srv, err := s.ServeIngest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	socket := srv.Socket()
	for path, want := range map[string]fs.FileMode{socket: 0o600, filepath.Dir(socket): 0o700} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v; want %v", path, info.Mode().Perm(), want)
		}
	}
	// The fixture keeps its store apart from its artifacts; the source
	// points at the store's folder, as reopening it does.
	s.e.artifacts = &snapshotArtifacts{root: filepath.Dir(s.c.storeDir)}
	if got, err := s.e.IngestSocket(t.Context(), s.CaseID()); err != nil || got != socket {
		t.Errorf("the store names %q, %v; want %q", got, err, socket)
	}

	content := "NAME   READY\nnetwork  False\n"
	first, err := SendIngest(t.Context(), socket, strings.NewReader(content), "stdin", "what does this answer?")
	if err != nil || !first.New || !strings.HasPrefix(first.Name, "e") || first.Lines != 2 || first.Bytes != int64(len(content)) ||
		first.Note != "what does this answer?" || first.Location != "stdin" {
		t.Fatalf("sent: %+v, %v", first, err)
	}
	it, err := s.EvidenceNamed(first.Name)
	if err != nil || string(it.Content) != content || it.Ref.Provenance.Source != "engineer" {
		t.Errorf("%s holds %q from %q, %v", first.Name, it.Content, it.Ref.Provenance.Source, err)
	}
	again, err := SendIngest(t.Context(), socket, strings.NewReader(content), "stdin", "what does this answer?")
	if err != nil || again.New || again.ID != first.ID || again.Name != first.Name {
		t.Errorf("sent again: %+v, %v; want %s, not new", again, err, first.ID)
	}
	if taken := srv.Take(); len(taken) != 2 || taken[0].ID != first.ID || len(srv.Take()) != 0 {
		t.Errorf("taken: %+v", taken)
	}

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("not compa in\n")); err != nil {
		t.Fatal(err)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("dialled %T", conn)
	}
	if err := uc.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var reply ingestReply
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&reply); err != nil || reply.Error == "" {
		t.Errorf("a request not compa in's answered %+v, %v", reply, err)
	}
	conn.Close()

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(socket)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket's directory after Close: %v", err)
	}
	if got, err := s.e.IngestSocket(t.Context(), s.CaseID()); err != nil || got != "" {
		t.Errorf("the store names %q, %v after Close", got, err)
	}
	if _, err := SendIngest(t.Context(), socket, strings.NewReader(content), "stdin", ""); err == nil {
		t.Error("a closed server took an ingest")
	}
}
