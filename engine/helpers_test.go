package engine

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
)

// actionCase is a case over the fixture must-gather, with fixture.storage
// and catalog fixture.core 0.1.1 installed and deterministic IDs and times.
// extra is added to the engine's configuration.
func actionCase(t *testing.T, extra string) (*Engine, *openCase) {
	t.Helper()
	return actionCaseWith(t, extra, "core-0.1.1", "storage")
}

// actionCaseWith is actionCase with the given fixture packages installed.
func actionCaseWith(t *testing.T, extra string, packages ...string) (*Engine, *openCase) {
	t.Helper()
	root := t.TempDir()
	install(t, root, packages...)
	cfg := "apiVersion: compa.lowmag.cc/config/v1alpha1\nkind: Config\nexperts: {local: [" + root + "]}\n" + extra
	c, err := config.Parse([]byte(cfg), "")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{config: c, home: t.TempDir()}
	oc := testCase(t, 1<<20)
	if oc.root, err = filepath.Abs("../testdata/artifacts/must-gather"); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	oc.newID = func(p string) string { return fmt.Sprintf("%s-%d", p, n.Add(1)) }
	oc.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	return e, oc
}

// summaryCase is an action case holding a case's metadata, two comments
// stored a moment apart as acquisitions are, and its attachments'
// overview; ids are the items in that order.
func summaryCase(t *testing.T) (e *Engine, c *openCase, ids []string) {
	t.Helper()
	e, c = actionCase(t, "")
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, it := range []evidence.Item{
		caseItem("stub", Case{ID: "00000001", Subject: "Volumes will not attach", Description: "Since the upgrade, PVCs stay Pending."}),
		commentItem("stub", "00000001", Comment{ID: "c1", Visibility: VisibilityPublic, Created: at, Body: "Started after the upgrade."}),
		commentItem("stub", "00000001", Comment{ID: "c2", Visibility: VisibilityPrivate, Created: at.Add(time.Hour), Body: "Asked for a must-gather."}),
	} {
		if err := c.ev.Add(it); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.Ref.ID)
		time.Sleep(2 * time.Millisecond)
	}
	atts := []Attachment{
		{Name: "0010-must-gather.local.1.tgz", Dir: "0010-must-gather.local.1.tgz", Size: -1, State: AttachmentExtracted},
		{Name: "0020-must-gather.local.2.tgz", Path: "0020-must-gather.local.2.tgz", Size: 3 << 30, State: AttachmentNotExtracted},
	}
	if err := recordAttachments(c.src, "local directory", atts); err != nil {
		t.Fatal(err)
	}
	ov := overview("local directory", "00000001", atts)
	if err := c.ev.Add(ov); err != nil {
		t.Fatal(err)
	}
	return e, c, append(ids, ov.Ref.ID)
}
