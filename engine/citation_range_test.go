package engine

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCitationEnDashRanges(t *testing.T) {
	const bracket = "[e12:1–2, 4–5, e14:6-7]"
	want := []Cite{
		{Text: bracket, Name: "e12", From: 1, To: 2},
		{Text: bracket, Name: "e12", From: 4, To: 5},
		{Text: bracket, Name: "e14", From: 6, To: 7},
		{Text: "nodes/worker-1/log.txt:10–12", Path: "nodes/worker-1/log.txt", From: 10, To: 12},
	}
	if d := cmp.Diff(want, Cites(bracket+" nodes/worker-1/log.txt:10–12")); d != "" {
		t.Errorf("typographic range citations (-want +got):\n%s", d)
	}
}

// Source bytes written from no case must reach both checkers for en-dash ranges.
func TestCheckerQuotesEnDashRange(t *testing.T) {
	const path = "capture/status.txt"
	const source = "2026-10-01T12:00:00Z fsid=fs-a HEALTH_OK pgs=active+clean\nosd.3 latency_ms=1\n"
	s, names := readFilesSession(t, []readFile{{path, source}}, func(n map[string]string) string {
		return "The capture reports healthy state at collection [" + n[path] + ":1–2]."
	})
	for _, role := range []string{"editor", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			var req CheckRequest
			var err error
			if role == "editor" {
				req, err = s.EditorRequest(context.Background())
			} else {
				req, err = s.ReviewRequest(context.Background(), ReviewAsk{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(req.Quotes) != 1 || req.Quotes[0].Name != names()[path] || len(req.Quotes[0].Parts) != 1 {
				t.Fatalf("%s packet quotes = %+v; want the captured source", role, req.Quotes)
			}
			if got := req.Quotes[0].Parts[0].Text; got != source {
				t.Errorf("%s received source %q, want %q", role, got, source)
			}
		})
	}
}
