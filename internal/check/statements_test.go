package check_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/check"
)

// TestSplit: a text's statements are its list items whole, its table rows
// and its paragraphs' sentences, numbered in order; fenced blocks,
// headings and rules are none, and a sentence never ends inside a code
// span, a citation or after an abbreviation.
func TestSplit(t *testing.T) {
	text := strings.Join([]string{
		"## Where it stands",
		"",
		"The kubelet ranked pods for eviction [e3:2. 4]. It did so at 08:10, e.g. under disk pressure. Was it evicted? `a. B` stays whole.",
		"",
		"- The MONs ran on worker-1 [e7:2-4].",
		"  The listing shows their logs.",
		"1. Send the kubelet log.",
		"",
		"```",
		"oc get pods -n openshift-storage. Not a statement.",
		"```",
		"",
		"> A quoted line. Two sentences.",
		"",
		"| node | state |",
		"|---|---|",
		"| worker-1 | NotReady |",
		"",
		"---",
		"Last. /var filled.",
	}, "\n")
	var got []string
	for _, s := range check.Split(text) {
		got = append(got, s.ID+": "+s.Text)
	}
	want := []string{
		"s1: The kubelet ranked pods for eviction [e3:2. 4].",
		"s2: It did so at 08:10, e.g. under disk pressure.",
		"s3: Was it evicted?",
		"s4: `a. B` stays whole.",
		"s5: The MONs ran on worker-1 [e7:2-4]. The listing shows their logs.",
		"s6: Send the kubelet log.",
		"s7: A quoted line.",
		"s8: Two sentences.",
		"s9: | node | state |",
		"s10: | worker-1 | NotReady |",
		"s11: Last.",
		"s12: /var filled.",
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("statements (-want +got):\n%s", d)
	}
	if d := cmp.Diff(check.Split(text), check.Split(text)); d != "" {
		t.Errorf("the same text split twice differs:\n%s", d)
	}
}

// TestStatementParts: what a check looks at in a statement — its code
// spans, its qualifying words outside them, and whether it states a code
// span, a time, a date or a count, names such as worker-1 and e12 not
// counting.
func TestStatementParts(t *testing.T) {
	s := "The plugin `may restart` likely restarted, or either it MAY be gone; it appears so."
	if d := cmp.Diff([]string{"may restart"}, check.Spans(s)); d != "" {
		t.Errorf("spans (-want +got):\n%s", d)
	}
	if d := cmp.Diff([]string{"likely", "either", "may", "appears"}, check.Hedges(s)); d != "" {
		t.Errorf("hedges (-want +got):\n%s", d)
	}
	if h := check.Hedges("The socket was removed and the containers crashed."); len(h) != 0 {
		t.Errorf("hedges in a flat statement: %v", h)
	}
	for text, want := range map[string]bool{
		"The alert fired with a 300 s threshold.": true,
		"The OSD stopped at 08:16.":               true,
		"It began on 2026-10-20.":                 true,
		"/var reached 100% utilization.":          true,
		"The `ceph_osd_up` metric dropped.":       true,
		"worker-1 and worker-2 lost the CSI node": false,
		"See e12 for the listing.":                false,
		"The MONs ran on the node.":               false,
	} {
		if got := check.Specific(text); got != want {
			t.Errorf("Specific(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestCommand: a code span with whitespace in it whose first word is a
// program to run, the engine's own or one the deployment names, is a
// command line, no claim; a bare name or value, an expression whose first
// word is no program, and a product's command line no deployment names
// are claims still.
func TestCommand(t *testing.T) {
	named := check.Commands{"oc", "ceph"}
	for span, want := range map[string]bool{
		"oc -n openshift-dns get pods -o wide": true,
		"ceph health detail":                   true,
		"journalctl -u kubelet":                true,
		"dig  @172.30.0.10 kubernetes.default": true,
		"ceph_osd_up == 0":                     false,
		"172.30.0.10":                          false,
		"dns-default":                          false,
		"oc":                                   false,
		"the oc client":                        false,
	} {
		if got := named.Command(span); got != want {
			t.Errorf("Command(%q) = %v; want %v", span, got, want)
		}
	}
	for span, want := range map[string]bool{
		"oc -n openshift-dns get pods -o wide": false,
		"ceph health detail":                   false,
		"journalctl -u kubelet":                true,
	} {
		if got := check.Commands(nil).Command(span); got != want {
			t.Errorf("with no programs named, Command(%q) = %v; want %v", span, got, want)
		}
	}
}

// TestExcerpts: a fenced block with no info string or one naming text or a
// log is an excerpt, its lines with a tool's line numbers taken off and cut
// at an ellipsis, parts too short to tell apart left out, under the
// statement presenting it; a block of a shell or YAML, and one holding a
// command line, with or without a prompt, is none. Two blocks one after
// the other are two.
func TestExcerpts(t *testing.T) {
	text := strings.Join([]string{
		"Where it stands.",
		"",
		"The kubelet logged the eviction [e3:4]:",
		"",
		"```",
		"4: Oct 20 08:10:02 worker-1 kubelet[2211]: pods ranked for eviction",
		"  5 | E1020 08:12:40.502 csi_attacher.go:1022] rpc error … dial unix /var/lib/kubelet/plugins",
		"}",
		"```",
		"```log",
		"[...] OSD 3 shutting down",
		"```",
		"",
		"To check it:",
		"",
		"```",
		"$ oc -n openshift-storage get pods",
		"NAME   READY",
		"```",
		"",
		"```yaml",
		"evictionHard: imagefs.available<10%",
		"```",
		"",
		"```sh",
		"echo a value of no claim",
		"```",
	}, "\n")
	var got []string
	for _, ex := range (check.Commands{"oc"}).Excerpts(text) {
		got = append(got, ex.After.ID+" "+ex.After.Text)
		for _, l := range ex.Lines {
			got = append(got, "  "+l)
		}
		if ex.Bare {
			got = append(got, "  (bare)")
		}
	}
	want := []string{
		"s2 The kubelet logged the eviction [e3:4]:",
		"  Oct 20 08:10:02 worker-1 kubelet[2211]: pods ranked for eviction",
		"  E1020 08:12:40.502 csi_attacher.go:1022] rpc error",
		"  dial unix /var/lib/kubelet/plugins",
		"  (bare)",
		"s2 The kubelet logged the eviction [e3:4]:",
		"  OSD 3 shutting down",
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("excerpts (-want +got):\n%s", d)
	}
}

// TestCertaintyTerms: a statement's certainty and causal terms outside its
// code spans, in order and once each: own-voice proof and model-owned
// confidence, and causal phrasing even inside an attributed conclusion,
// whose voice only reading can tell; a named source's own verb, as in
// "the engineer confirmed", and a procedure's imperatives are none.
func TestCertaintyTerms(t *testing.T) {
	for text, want := range map[string][]string{
		"The investigation confirms that the initial FailedMount errors were caused by root filesystem exhaustion.": {"confirms", "caused"},
		"Engineering concluded the root cause is the full /var.":                                                    {"root cause"},
		"The customer reports it caused the outage.":                                                                {"caused"},
		"The engineer confirmed the node rebooted at 14:03.":                                                        nil,
		"The customer has confirmed the upgrade finished.":                                                          nil,
		"It is most likely the CSI plugin; clearly the socket was gone, which led to the crash.":                    {"most likely", "clearly", "led to"},
		"Verify the mount, then validate the config with `oc confirm`.":                                             nil,
		"The kubelet's eviction manager ranked the pod for eviction.":                                               nil,
	} {
		if d := cmp.Diff(want, check.CertaintyTerms(text)); d != "" {
			t.Errorf("CertaintyTerms(%q) (-want +got):\n%s", text, d)
		}
	}
}
