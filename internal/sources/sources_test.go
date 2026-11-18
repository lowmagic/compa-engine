package sources

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/store"
)

func TestRecordKeepsFirstSighting(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), store.DirName), "00000001", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st := New(s)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	mg := Source{Origin: "local directory", Name: "0010-must-gather.tgz", Kind: KindAttachment, Path: "0010-must-gather.tgz", Size: 10, State: "not_extracted"}
	if err := st.Record([]Source{mg}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	mg.State, mg.Dir = "extracted", "0010-must-gather.tgz.d"
	log := Source{Origin: "local directory", Name: "0020-pods.log", Kind: KindAttachment, Path: "0020-pods.log", Size: -1, State: "present"}
	if err := st.Record([]Source{mg, log}); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if g := got[0]; g.State != "extracted" || g.Dir != mg.Dir || !g.FirstSeen.Equal(now.Add(-time.Hour)) || !g.LastSeen.Equal(now) {
		t.Errorf("must-gather after an update: %+v", g)
	}
	if g := got[1]; g.Size != -1 || !g.FirstSeen.Equal(now) {
		t.Errorf("log: %+v", g)
	}
}
