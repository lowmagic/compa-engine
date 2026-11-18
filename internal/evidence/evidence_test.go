package evidence_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/collect"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/store"
)

func open(t *testing.T, dir string) (*store.Store, *evidence.Store) {
	t.Helper()
	s, err := store.Open(dir, "0420", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s, evidence.New(s)
}

var testRecord = collect.Record{
	ID:     "ev-7b6378b9a31095fe",
	What:   collect.What{TestID: "platform.degraded_operators", TestVersion: "0.1.0", Expert: "openshift.platform@0.1.0", Pipeline: []expert.Stage{{Primitive: "list"}}},
	Status: collect.StatusOK, Truncated: true,
	StartedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Duration: 3 * time.Millisecond,
	PathsRead: []string{"cluster-scoped-resources/config.openshift.io/clusteroperators/network.yaml", "cluster-scoped-resources/config.openshift.io/clusteroperators/ingress.yaml"},
	Output:    []byte("network\ningress\n"),
}

// Evidence stored and read back after the store is closed and reopened
// keeps its provenance — source and location — its size and its content,
// and reaches a request as the same input as when it was fresh.
func TestRoundTripAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), store.DirName)
	s, ev := open(t, dir)
	if err := ev.Put(testRecord); err != nil {
		t.Fatal(err)
	}
	if err := ev.Put(testRecord); err != nil {
		t.Fatalf("storing the same evidence again: %v", err)
	}
	s.Close()

	s, ev = open(t, dir)
	defer s.Close()
	it, err := ev.Get(testRecord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Ref.Provenance.Source != "platform.degraded_operators@0.1.0" || it.Ref.Type != evidence.TypeTestResult || it.Ref.Provenance.CollectedBy != "engine" {
		t.Errorf("source %+v", it.Ref)
	}
	if it.Location != testRecord.PathsRead[0]+"\n"+testRecord.PathsRead[1] {
		t.Errorf("location %q", it.Location)
	}
	if string(it.Content) != "network\ningress\n" || it.Size.Bytes != 16 || it.Size.Lines != 2 || !it.Size.LowerBound {
		t.Errorf("content %q size %+v", it.Content, it.Size)
	}
	// Compared as JSON: a round trip turns nil maps and slices empty.
	gotWhat, _ := json.Marshal(it.Collection.What)
	wantWhat, _ := json.Marshal(testRecord.What)
	if it.Collection == nil || it.Collection.Output != nil || string(gotWhat) != string(wantWhat) || it.Collection.Duration != testRecord.Duration {
		t.Errorf("collection record %+v", it.Collection)
	}
	if fresh := evidence.FromRecord(testRecord).Input(); !reflect.DeepEqual(it.Input(), fresh) {
		t.Errorf("stored evidence reaches a request differently:\n%+v\n%+v", it.Input(), fresh)
	}
	var rows int
	s.DB().QueryRow(`SELECT count(*) FROM evidence`).Scan(&rows)
	if rows != 1 || !ev.Exists(testRecord.ID) || ev.Exists("ev-none") {
		t.Errorf("rows %d", rows)
	}
	if _, err := ev.Get("ev-none"); !errors.Is(err, evidence.ErrNotFound) {
		t.Errorf("missing evidence: %v", err)
	}
}

// A collection that did not run says why in its note, which a model sees
// before the text; a command's output is typed as command output from the
// engine.
func TestInput(t *testing.T) {
	failed := collect.Record{ID: "ev-x", What: collect.What{Pipeline: []expert.Stage{{Argv: []string{"omc", "get", "co"}}}}, Status: collect.StatusUnavailable, Reason: "omc is not allowlisted"}
	in := evidence.FromRecord(failed).Input()
	if in.Text != "" || in.Ref.Note != "status: unavailable; reason: omc is not allowlisted" || in.Ref.Type != evidence.TypeCommandOutput || in.Ref.Provenance.Source != "command" || !in.Inline {
		t.Errorf("input %+v", in)
	}
	if in.Size == nil || in.Size.Bytes != 0 {
		t.Errorf("size %+v", in.Size)
	}
}

// A collection that stopped early says so in its note, which a model
// reads before what was found, whether it failed or not.
func TestInputSaysWhereItStopped(t *testing.T) {
	stop := &collect.Stop{By: "match_limit", At: "node-a/journal.log at line 30", FilesGiven: 3, FilesRead: 1, NotReached: []string{"node-b/journal.log", "node-c/journal.log"}}
	for _, status := range []string{collect.StatusOK, collect.StatusFailed} {
		in := evidence.FromRecord(collect.Record{ID: "ev-1", Status: status, Reason: "stage 0: timed out after 1m0s", Truncated: true, Stopped: stop, Output: []byte("node-a/journal.log:10:oom\n")}).Input()
		if !strings.HasSuffix(in.Ref.Note, "stopped: reached its match limit in node-a/journal.log at line 30; not reached: 2 of 3 files (node-b/journal.log, node-c/journal.log)") || in.Text != "node-a/journal.log:10:oom\n" {
			t.Errorf("%s: note %q text %q", status, in.Ref.Note, in.Text)
		}
	}
	if in := evidence.FromRecord(collect.Record{ID: "ev-2", Status: collect.StatusOK, Output: []byte("x\n")}).Input(); in.Text != "x\n" || in.Ref.Note != "" {
		t.Errorf("a complete run: %q %q", in.Ref.Note, in.Text)
	}
}
