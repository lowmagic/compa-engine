package collect

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/expert"
)

const mustGather = "../../testdata/artifacts/must-gather"

const ovnLog = "namespaces/openshift-ovn-kubernetes/pods/ovnkube-node-x7k2p/ovnkube-controller/ovnkube-controller/logs/current.log"

func prim(name string, params map[string]any) expert.Stage {
	return expert.Stage{Primitive: name, Params: params}
}

func run(t *testing.T, root string, stages ...expert.Stage) Record {
	t.Helper()
	r := &Runner{}
	return r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: stages}})
}

func mustOK(t *testing.T, rec Record) string {
	t.Helper()
	if rec.Status != StatusOK {
		t.Fatalf("status %s: %s", rec.Status, rec.Reason)
	}
	return string(rec.Output)
}

func TestFind(t *testing.T) {
	got := mustOK(t, run(t, mustGather, prim("find", map[string]any{"path": "cluster-scoped-resources", "glob": "*.yaml"})))
	want := "cluster-scoped-resources/config.openshift.io/clusteroperators.yaml\ncluster-scoped-resources/core/nodes/master-0.yaml\ncluster-scoped-resources/core/nodes/worker-1.yaml\n"
	if got != want {
		t.Errorf("got\n%s", got)
	}
	got = mustOK(t, run(t, mustGather, prim("find", map[string]any{"path": "cluster-scoped-resources", "type": "dir"})))
	if !strings.Contains(got, "cluster-scoped-resources/core/nodes\n") || strings.Contains(got, ".yaml") {
		t.Errorf("dirs: got\n%s", got)
	}
	rec := run(t, mustGather, prim("find", map[string]any{"max_results": 2}))
	if !rec.Truncated || strings.Count(string(rec.Output), "\n") != 2 {
		t.Errorf("max_results: truncated %v, output\n%s", rec.Truncated, rec.Output)
	}
	// details, max_depth and min_depth at their zero values print what
	// leaving them out prints, byte for byte.
	plain := mustOK(t, run(t, mustGather, prim("find", map[string]any{"type": "any"})))
	zero := mustOK(t, run(t, mustGather, prim("find", map[string]any{"type": "any", "details": false, "max_depth": 0, "min_depth": 0})))
	if zero != plain || strings.Contains(plain, "\t") {
		t.Errorf("zero values: got\n%s\nwant\n%s", zero, plain)
	}
}

func TestGrep(t *testing.T) {
	got := mustOK(t, run(t, mustGather, prim("grep", map[string]any{"pattern": "egress IP", "path": "namespaces"})))
	if got != ovnLog+":8:2026-10-19T12:01:00.000000Z W1019 12:01:00.000000 1 egress.go:88] egress IP 10.0.0.9 not assigned\n" {
		t.Errorf("got %q", got)
	}
	got = mustOK(t, run(t, mustGather, prim("grep", map[string]any{"pattern": "ready check", "path": ovnLog, "context_before": 1, "context_after": 1})))
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "log-5-main.sync()") || !strings.Contains(lines[1], "log:6:") || !strings.Contains(lines[2], "log-7-") {
		t.Errorf("context: got\n%s", got)
	}
	rec := run(t, mustGather, prim("grep", map[string]any{"pattern": "ERROR", "ignore_case": true, "path": "namespaces", "max_matches": 2}))
	if !rec.Truncated || strings.Count(string(rec.Output), "\n") != 2 {
		t.Errorf("max_matches: truncated %v\n%s", rec.Truncated, rec.Output)
	}
	// keep at its zero value prints what leaving it out prints, byte for byte.
	plain := mustOK(t, run(t, mustGather, prim("grep", map[string]any{"pattern": "ready check", "path": ovnLog, "context_before": 1})))
	zero := mustOK(t, run(t, mustGather, prim("grep", map[string]any{"pattern": "ready check", "path": ovnLog, "context_before": 1, "keep": 0})))
	if zero != plain {
		t.Errorf("keep 0: got\n%s\nwant\n%s", zero, plain)
	}
}

func TestGrepKeep(t *testing.T) {
	root := t.TempDir()
	tableA := "+----+------+--------+\n" +
		"| ID | Name | Status |\n" +
		"+----+------+--------+\n" +
		"| 1  | web  | ACTIVE |\n" +
		"| 2  | api  | DOWN   |\n" +
		"| 3  | db   | DOWN   |\n" +
		"+----+------+--------+\n"
	tableB := "+----+------+--------+\n" +
		"| ID | Name | Status |\n" +
		"+----+------+--------+\n" +
		"| 9  | ok   | ACTIVE |\n" +
		"+----+------+--------+\n"
	tableC := "+----+------+--------+\n" +
		"| ID | Name | Status |\n" +
		"+----+------+--------+\n" +
		"| 4  | job  | DOWN   |\n" +
		"+----+------+--------+\n"
	for name, body := range map[string]string{
		"a.txt":     tableA,
		"b.txt":     tableB,
		"c.txt":     tableC,
		"short.txt": "header\nmatch\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Single file: header kept once with '-', not counting toward max_matches.
	rec := run(t, root, prim("grep", map[string]any{"pattern": "DOWN", "path": "a.txt", "keep": 3, "max_matches": 1}))
	want := "a.txt-1-+----+------+--------+\n" +
		"a.txt-2-| ID | Name | Status |\n" +
		"a.txt-3-+----+------+--------+\n" +
		"a.txt:5:| 2  | api  | DOWN   |\n"
	if got := mustOK(t, rec); got != want || !rec.Truncated || stopBy(rec) != "match_limit" {
		t.Errorf("single file max_matches 1: truncated %v stop %q got\n%s\nwant\n%s", rec.Truncated, stopBy(rec), got, want)
	}

	// Context_before overlapping kept lines: no duplicates, exact line order.
	got := mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "api", "path": "a.txt", "keep": 3, "context_before": 3})))
	want = "a.txt-1-+----+------+--------+\n" +
		"a.txt-2-| ID | Name | Status |\n" +
		"a.txt-3-+----+------+--------+\n" +
		"a.txt-4-| 1  | web  | ACTIVE |\n" +
		"a.txt:5:| 2  | api  | DOWN   |\n"
	if got != want {
		t.Errorf("overlap context_before: got\n%s\nwant\n%s", got, want)
	}

	// A kept line that also matches keeps ':', and remaining kept lines still emit with '-'.
	got = mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "Status|api", "path": "a.txt", "keep": 3, "context_after": 1})))
	want = "a.txt-1-+----+------+--------+\n" +
		"a.txt:2:| ID | Name | Status |\n" +
		"a.txt-3-+----+------+--------+\n" +
		"a.txt:5:| 2  | api  | DOWN   |\n" +
		"a.txt-6-| 3  | db   | DOWN   |\n"
	if got != want {
		t.Errorf("kept line matches: got\n%s\nwant\n%s", got, want)
	}

	// Multi-file glob: non-matching b.txt emits nothing; a.txt and c.txt keep their own headers.
	got = mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "DOWN", "glob": "*.txt", "keep": 3})))
	want = "a.txt-1-+----+------+--------+\n" +
		"a.txt-2-| ID | Name | Status |\n" +
		"a.txt-3-+----+------+--------+\n" +
		"a.txt:5:| 2  | api  | DOWN   |\n" +
		"a.txt:6:| 3  | db   | DOWN   |\n" +
		"c.txt-1-+----+------+--------+\n" +
		"c.txt-2-| ID | Name | Status |\n" +
		"c.txt-3-+----+------+--------+\n" +
		"c.txt:4:| 4  | job  | DOWN   |\n"
	if got != want {
		t.Errorf("multi-file: got\n%s\nwant\n%s", got, want)
	}

	// File with no matches emits nothing.
	if got := mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "DOWN", "path": "b.txt", "keep": 3}))); got != "" {
		t.Errorf("no match: got %q, want empty", got)
	}

	// keep larger than file length emits only existing lines.
	got = mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "match", "path": "short.txt", "keep": 10})))
	if got != "short.txt-1-header\nshort.txt:2:match\n" {
		t.Errorf("short file: got %q", got)
	}

	// Previous stage input (piped).
	got = mustOK(t, run(t, root,
		prim("read", map[string]any{"path": "a.txt"}),
		prim("grep", map[string]any{"pattern": "api", "keep": 3}),
	))
	want = "--1-+----+------+--------+\n" +
		"--2-| ID | Name | Status |\n" +
		"--3-+----+------+--------+\n" +
		"-:5:| 2  | api  | DOWN   |\n"
	if got != want {
		t.Errorf("piped keep: got\n%s\nwant\n%s", got, want)
	}
}

func TestGrepRefusesBadKeep(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []int{-1, maxGrepKeep + 1} {
		s := prim("grep", map[string]any{"pattern": "line", "path": "a.txt", "keep": k})
		if err := CheckStage(s); err == nil {
			t.Errorf("CheckStage accepted keep=%d", k)
		}
		if err := ValidateStage(s); err == nil {
			t.Errorf("ValidateStage accepted keep=%d", k)
		}
		if rec := run(t, root, s); rec.Status == StatusOK {
			t.Errorf("Run accepted keep=%d", k)
		}
	}
}

func TestRead(t *testing.T) {
	co := "cluster-scoped-resources/config.openshift.io/clusteroperators.yaml"
	if got := mustOK(t, run(t, mustGather, prim("read", map[string]any{"path": co, "head": 2}))); got != "apiVersion: config.openshift.io/v1\nkind: ClusterOperatorList\n" {
		t.Errorf("head: %q", got)
	}
	if got := mustOK(t, run(t, mustGather, prim("read", map[string]any{"path": ovnLog, "tail": 1}))); !strings.Contains(got, "egress IP") || strings.Count(got, "\n") != 1 {
		t.Errorf("tail: %q", got)
	}
	if got := mustOK(t, run(t, mustGather, prim("read", map[string]any{"path": ovnLog, "start_line": 4, "end_line": 5}))); got != "goroutine 412 [running]:\nmain.sync()\n" {
		t.Errorf("lines: %q", got)
	}
	if got := mustOK(t, run(t, mustGather, prim("read", map[string]any{"path": co, "start_byte": 5, "end_byte": 12}))); got != "rsion: " {
		t.Errorf("bytes: %q", got)
	}
	if rec := run(t, mustGather, prim("read", map[string]any{"path": co, "head": 1, "tail": 1})); rec.Status != StatusFailed {
		t.Errorf("head and tail together: %s", rec.Status)
	}
}

func TestQuery(t *testing.T) {
	got := mustOK(t, run(t, mustGather, prim("yq", map[string]any{
		"path": "cluster-scoped-resources/core/nodes", "glob": "*.yaml",
		"filter": `{name: .metadata.name, ready: (.status.conditions[] | select(.type == "Ready") | .status)}`,
	})))
	if got != "{\"name\":\"master-0\",\"ready\":\"True\"}\n{\"name\":\"worker-1\",\"ready\":\"False\"}\n" {
		t.Errorf("yq: %q", got)
	}
	// jq over the previous stage, with a parameter bound as a variable.
	r := &Runner{}
	rec := r.Run(context.Background(), Request{
		ArtifactRoot: mustGather,
		Vars:         map[string]any{"name": "worker-1"},
		What: What{Pipeline: []expert.Stage{
			prim("yq", map[string]any{"path": "cluster-scoped-resources/core/nodes", "filter": "."}),
			prim("jq", map[string]any{"filter": `select(.metadata.name == $name) | .metadata.name`, "raw": true}),
		}},
	})
	if got := mustOK(t, rec); got != "worker-1\n" {
		t.Errorf("jq with $name: %q", got)
	}
	if rec := run(t, mustGather, prim("jq", map[string]any{"filter": ".["})); rec.Status != StatusFailed || !strings.Contains(rec.Reason, "filter") {
		t.Errorf("bad filter: %s %s", rec.Status, rec.Reason)
	}
}

func TestLogsWindowAndCount(t *testing.T) {
	got := mustOK(t, run(t, mustGather, prim("logs.window", map[string]any{"path": ovnLog, "start": "2026-10-19T11:57:31Z", "end": "2026-10-19T11:58:03Z"})))
	want := []string{"11:57:31.223456Z", "goroutine 412", "main.sync()", "11:58:02.000000Z"}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != len(want) {
		t.Fatalf("window: got\n%s", got)
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], w)
		}
	}
	got = mustOK(t, run(t, mustGather, prim("logs.count", map[string]any{"path": ovnLog, "pattern": "error"})))
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, `"count":3`) || !strings.Contains(got, `"first":"`+ovnLog+`:2"`) || !strings.Contains(got, `"last":"`+ovnLog+`:7"`) {
		t.Errorf("count: %s", got)
	}
}

func TestAggregate(t *testing.T) {
	grep := prim("grep", map[string]any{"pattern": "Degraded|Available", "path": "cluster-scoped-resources/config.openshift.io"})
	if got := mustOK(t, run(t, mustGather, grep, prim("aggregate", map[string]any{"op": "count"}))); got != "6\n" {
		t.Errorf("count: %q", got)
	}
	nodes := prim("yq", map[string]any{"path": "cluster-scoped-resources/core/nodes", "filter": `.status.conditions[]`})
	if got := mustOK(t, run(t, mustGather, nodes, prim("aggregate", map[string]any{"op": "count_by", "field": ".type"}))); got != "{\"count\":2,\"value\":\"MemoryPressure\"}\n{\"count\":2,\"value\":\"Ready\"}\n" {
		t.Errorf("count_by: %q", got)
	}
	if got := mustOK(t, run(t, mustGather, nodes, prim("aggregate", map[string]any{"op": "unique", "field": ".status"}))); got != "False\nTrue\n" {
		t.Errorf("unique: %q", got)
	}
	if rec := run(t, mustGather, prim("aggregate", map[string]any{"op": "count"})); rec.Status != StatusFailed {
		t.Error("aggregate as a first stage must fail")
	}
}

func TestDiff(t *testing.T) {
	got := mustOK(t, run(t, mustGather, prim("diff", map[string]any{"a": "cluster-scoped-resources/core/nodes/master-0.yaml", "b": "cluster-scoped-resources/core/nodes/worker-1.yaml"})))
	for _, want := range []string{"-  name: master-0\n", "+  name: worker-1\n", " apiVersion: v1\n", "--- cluster-scoped-resources/core/nodes/master-0.yaml\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	for _, c := range []struct{ a, b string }{{"", ""}, {"a\nb\nc", "a\nb\nc"}, {"a\nb", "b\nc"}, {"", "x\ny"}, {"x\ny\nz", ""}} {
		a, b := splitNonEmpty(c.a), splitNonEmpty(c.b)
		var gotA, gotB []string
		for _, op := range diffLines(a, b) {
			switch op[0] {
			case ' ':
				gotA, gotB = append(gotA, op[1:]), append(gotB, op[1:])
			case '-':
				gotA = append(gotA, op[1:])
			case '+':
				gotB = append(gotB, op[1:])
			}
		}
		if strings.Join(gotA, "\n") != c.a || strings.Join(gotB, "\n") != c.b {
			t.Errorf("diff(%q, %q) does not reconstruct its inputs", c.a, c.b)
		}
	}
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestArchiveList(t *testing.T) {
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "mg.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"b/file.txt", "a/file.txt"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
		tw.Write([]byte("hi"))
	}
	tw.Close()
	gz.Close()
	f.Close()
	zf, _ := os.Create(filepath.Join(root, "x.zip"))
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("only.txt")
	w.Write([]byte("abc"))
	zw.Close()
	zf.Close()

	if got := mustOK(t, run(t, root, prim("archive.list", map[string]any{"path": "mg.tar.gz"}))); got != "file\t2\ta/file.txt\nfile\t2\tb/file.txt\n" {
		t.Errorf("tar.gz: %q", got)
	}
	if got := mustOK(t, run(t, root, prim("archive.list", map[string]any{"path": "x.zip"}))); got != "file\t3\tonly.txt\n" {
		t.Errorf("zip: %q", got)
	}
	if rec := run(t, root, prim("archive.list", map[string]any{"path": "mg.tar.gz", "max_entries": 1})); !rec.Truncated {
		t.Error("max_entries must mark truncation")
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Error("archive.list must not extract")
	}
}

func TestPrimitivesStayInsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o644)
	if err := os.Symlink(filepath.Join(parent, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []expert.Stage{
		prim("read", map[string]any{"path": "../secret.txt"}),
		prim("read", map[string]any{"path": filepath.Join(parent, "secret.txt")}),
		prim("read", map[string]any{"path": "link.txt"}),
		prim("grep", map[string]any{"pattern": "secret", "path": ".."}),
		prim("find", map[string]any{"path": "../"}),
		prim("jq", map[string]any{"filter": ".", "path": "../secret.txt"}),
		prim("diff", map[string]any{"a": "ok.txt", "b": "../secret.txt"}),
	} {
		rec := run(t, root, stage)
		if rec.Status == StatusOK || strings.Contains(string(rec.Output), "secret") {
			t.Errorf("%s %v: status %s, output %q", stage.Primitive, stage.Params, rec.Status, rec.Output)
		}
	}
	// Walking never follows the symlink out.
	if got := mustOK(t, run(t, root, prim("grep", map[string]any{"pattern": "."}))); strings.Contains(got, "secret") {
		t.Errorf("grep followed a symlink: %q", got)
	}
}

func TestOutputCapMarksTruncation(t *testing.T) {
	r := &Runner{Limits: Limits{OutputBytes: 40}}
	rec := r.Run(context.Background(), Request{ArtifactRoot: mustGather, What: What{Pipeline: []expert.Stage{prim("read", map[string]any{"path": ovnLog})}}})
	if rec.Status != StatusOK || !rec.Truncated || len(rec.Output) != 40 || !rec.Stages[0].Truncated {
		t.Fatalf("status %s truncated %v len %d", rec.Status, rec.Truncated, len(rec.Output))
	}
}

func TestCheckStage(t *testing.T) {
	short := "*.yaml"
	ok := []expert.Stage{
		{Primitive: "find", Shorthand: &short},
		prim("grep", map[string]any{"pattern": "x", "max_matches": "{{max}}", "keep": "{{k}}"}),
		prim("grep", map[string]any{"pattern": "x", "keep": 3}),
		prim("logs.window", map[string]any{"start": "{{from}}", "end": "{{to}}"}),
	}
	for _, s := range ok {
		if err := CheckStage(s); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
	bad := map[string]expert.Stage{
		"unknown parameter": prim("grep", map[string]any{"pattern": "x", "recursive": true}),
		"wrong type":        prim("grep", map[string]any{"pattern": "x", "max_matches": "many"}),
		"missing required":  prim("grep", map[string]any{"path": "x"}),
		"no shorthand":      {Primitive: "diff", Shorthand: &short},
		"templated unknown": prim("grep", map[string]any{"pattern": "x", "bogus": "{{v}}"}),
		// An engine without a parameter refuses a package using it.
		"find unknown parameter": prim("find", map[string]any{"maxdepth": 2}),
		"grep negative keep":     prim("grep", map[string]any{"pattern": "x", "keep": -1}),
		"grep huge keep":         prim("grep", map[string]any{"pattern": "x", "keep": maxGrepKeep + 1}),
	}
	for name, s := range bad {
		if err := CheckStage(s); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestPrimitivesList(t *testing.T) {
	if got := strings.Join(Primitives(), " "); got != "aggregate archive.list diff find grep jq logs.count logs.window read yq" {
		t.Errorf("Primitives() = %s", got)
	}
}

// A gzipped file — a node's kubelet journal in a must-gather — is read
// decompressed by the line primitives: a line range, head and tail,
// grep. A byte range is refused, since it would mean compressed bytes. The
// read bound counts decompressed bytes, so a file that decompresses
// without end is cut where a plain one would be.
func TestGzippedFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nodes", "worker-0")
	os.MkdirAll(dir, 0o700)
	var plain strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&plain, "Oct 21 10:%02d:00 worker-0 kubelet[1]: line %d\n", i, i)
	}
	plain.WriteString("Oct 21 11:00:00 worker-0 kubelet[1]: E1021 MountVolume.SetUp failed\n")
	write := func(name string, data []byte) {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		gz.Write(data)
		gz.Close()
		if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("worker-0_logs_kubelet.gz", []byte(plain.String()))
	r := &Runner{}
	at := "nodes/worker-0/worker-0_logs_kubelet.gz"
	if out := mustOK(t, runIn(r, root, prim("read", map[string]any{"path": at, "head": 2}))); out != "Oct 21 10:01:00 worker-0 kubelet[1]: line 1\nOct 21 10:02:00 worker-0 kubelet[1]: line 2\n" {
		t.Errorf("head: %q", out)
	}
	if out := mustOK(t, runIn(r, root, prim("read", map[string]any{"path": at, "tail": 1}))); !strings.Contains(out, "MountVolume.SetUp failed") {
		t.Errorf("tail: %q", out)
	}
	if out := mustOK(t, runIn(r, root, prim("grep", map[string]any{"pattern": "MountVolume", "path": "nodes"}))); !strings.Contains(out, "E1021 MountVolume.SetUp failed") {
		t.Errorf("grep across the directory: %q", out)
	}
	if rec := runIn(r, root, prim("read", map[string]any{"path": at, "start_byte": 1, "end_byte": 10})); rec.Status == StatusOK || !strings.Contains(rec.Reason, "gzipped") {
		t.Errorf("a byte range: %s %q", rec.Status, rec.Reason)
	}

	// A bomb: far more decompressed than the read bound.
	write("bomb.gz", bytes.Repeat([]byte("a\n"), 1<<20))
	small := &Runner{Limits: Limits{MaxFileBytes: 1000}}
	out := mustOK(t, runIn(small, root, prim("logs.count", map[string]any{"path": "nodes/worker-0/bomb.gz"})))
	if !strings.Contains(out, "500") {
		t.Errorf("the bomb was not cut at the read bound: %q", out)
	}
}

// A file read only to the read bound is marked truncated, plain or
// gzipped, never presented as complete; a gzipped one of empty members,
// which decompress to nothing, is bounded by its compressed bytes.
func TestReadBoundMarksTruncation(t *testing.T) {
	root := t.TempDir()
	gzOf := func(data []byte, members int) []byte {
		var b bytes.Buffer
		for i := 0; i < members; i++ {
			zw := gzip.NewWriter(&b)
			zw.Write(data)
			zw.Close()
		}
		return b.Bytes()
	}
	os.WriteFile(filepath.Join(root, "plain.log"), bytes.Repeat([]byte("a\n"), 1000), 0o600)
	os.WriteFile(filepath.Join(root, "lines.gz"), gzOf(bytes.Repeat([]byte("a\n"), 1000), 1), 0o600)
	os.WriteFile(filepath.Join(root, "empty.gz"), gzOf(nil, 100000), 0o600)
	small := &Runner{Limits: Limits{MaxFileBytes: 1000}}
	for _, name := range []string{"plain.log", "lines.gz"} {
		rec := runIn(small, root, prim("logs.count", map[string]any{"path": name}))
		if rec.Status != StatusOK || !rec.Truncated {
			t.Errorf("%s cut at the bound: status %s, truncated %v", name, rec.Status, rec.Truncated)
		}
	}
	if rec := runIn(small, root, prim("read", map[string]any{"path": "empty.gz", "head": 1})); !rec.Truncated {
		t.Errorf("empty members past the bound: status %s %s, truncated %v", rec.Status, rec.Reason, rec.Truncated)
	}
	if rec := runIn(&Runner{}, root, prim("logs.count", map[string]any{"path": "plain.log"})); rec.Truncated {
		t.Error("a file within the bound is marked truncated")
	}
}

// archive.list stops at its entry cap without reading on into the last
// member's content, which may be large; a full list is marked truncated.
// The archive here breaks inside its first member, so reading on fails.
func TestArchiveListStopsAtItsCap(t *testing.T) {
	root := t.TempDir()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "big.bin", Mode: 0o600, Size: 1 << 20})
	tw.Write(bytes.Repeat([]byte{0}, 1000))
	zw.Flush()
	// No more of the member, no next header: the stream ends here.
	os.WriteFile(filepath.Join(root, "a.tar.gz"), b.Bytes(), 0o600)
	rec := runIn(&Runner{}, root, prim("archive.list", map[string]any{"path": "a.tar.gz", "max_entries": 1}))
	if rec.Status != StatusOK || !strings.Contains(string(rec.Output), "big.bin") || !rec.Truncated {
		t.Errorf("listing one entry: status %s %q, truncated %v, output %q", rec.Status, rec.Reason, rec.Truncated, rec.Output)
	}
}
