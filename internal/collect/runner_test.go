package collect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lowmagic/compa-engine/internal/expert"
)

func argv(a ...string) expert.Stage { return expert.Stage{Argv: a} }

// sandboxRunner returns a runner with a real sandbox and the named system
// binaries allowlisted, or skips the test where there is no sandbox
// (SkipWithoutSandbox).
func sandboxRunner(t *testing.T, binaries ...string) *Runner {
	t.Helper()
	sb, err := DefaultSandbox()
	if err != nil {
		SkipWithoutSandbox(t, err)
	}
	r := &Runner{Sandbox: sb, ScratchParent: t.TempDir()}
	for _, b := range binaries {
		p, err := exec.LookPath(b)
		if err != nil {
			t.Skipf("%s not installed", b)
		}
		r.Policy.Allow(b, p)
	}
	return r
}

// realRoot is the artifact root at p by its real path, absolute with its
// links resolved, as the engine gives one to the runner (engine/localdir)
// and engine/sandboxtest's contract tests take theirs.
func realRoot(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		r, err = filepath.Abs(r)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPolicyRefusals(t *testing.T) {
	var p Policy
	for _, b := range []string{"find", "awk", "sed", "grep", "xargs"} {
		p.Allow(b, "/usr/bin/"+b)
	}
	p.Allow("myawk", "/usr/bin/awk")
	refusedCases := [][]string{
		{"curl", "http://example.com"},
		{"find", ".", "-exec", "cat", "{}", ";"},
		{"find", ".", "-execdir", "rm", "{}", "+"},
		{"find", ".", "-delete"},
		{"find", ".", "-fprint", "out"},
		{"awk", `BEGIN { system("id") }`},
		{"awk", `{ print > "/tmp/x" }`},
		{"awk", `{ "date" | getline d }`},
		{"awk", "-f", "prog.awk"},
		{"sed", "e id"},
		{"sed", "1e id"},
		{"sed", "w /tmp/out"},
		{"sed", "s/a/b/w /tmp/out"},
		{"sed", "s/a/id/e"},
		{"sed", "-i", "s/a/b/", "f"},
		// Options as getopt reads them: in a cluster, with a value
		// attached, or abbreviated.
		{"sed", "-i.bak", "s/a/b/", "f"},
		{"sed", "-ni", "p", "f"},
		{"sed", "--in-pl", "s/a/b/", "f"},
		{"sed", "--in-place=.bak", "s/a/b/", "f"},
		{"awk", "-fprog.awk"},
		{"awk", "--fi", "prog.awk"},
		// A script attached to its option, or after "=".
		{"sed", "-ew /tmp/out"},
		{"sed", "-new /tmp/out", "f"},
		{"sed", "--expression=w /tmp/out"},
		{"awk", `-vx=1;system("id")`},
		// An alias runs its binary, under the binary's rules.
		{"myawk", `BEGIN { system("id") }`},
		{"xargs", "rm"},
		{},
	}
	for _, a := range refusedCases {
		_, err := p.Check(a)
		var re *RefusedError
		if !errors.As(err, &re) {
			t.Errorf("Check(%q) = %v, want refusal", a, err)
		}
	}
	allowed := [][]string{
		{"find", ".", "-name", "*.log", "-type", "f"},
		{"awk", `$3 > 5 { print $1 }`},
		{"sed", "-n", "s/error/ERR/p"},
		{"sed", "-n", "/start/,/end/p"},
		{"grep", "-r", "error; rm -rf /", "."},
		{"sed", "-n", "-E", "s/a+/b/p"},
		{"sed", "--quiet", "--expression=s/a/b/p"},
		{"awk", "-F:", "{ print $1 }"},
		{"awk", "-v", "OFS=,", "{ print $1, $2 }"},
		{"find", ".", "-name", "*.log", "-newer", "ref"},
	}
	for _, a := range allowed {
		if _, err := p.Check(a); err != nil {
			t.Errorf("Check(%q) = %v, want allowed", a, err)
		}
	}
	if got := strings.Join(p.Allowlist(), " "); got != "awk find grep myawk sed xargs" {
		t.Errorf("Allowlist() = %s", got)
	}
}

func TestUnallowlistedCommandIsUnavailableAndNothingRuns(t *testing.T) {
	r := &Runner{}
	rec := r.Run(context.Background(), Request{ArtifactRoot: mustGather, What: What{Pipeline: []expert.Stage{
		prim("find", map[string]any{}),
		argv("curl", "http://example.com"),
	}}})
	if rec.Status != StatusUnavailable || !strings.Contains(rec.Reason, `"curl" is not allowlisted`) {
		t.Fatalf("status %s: %s", rec.Status, rec.Reason)
	}
	if len(rec.Stages) != 0 || len(rec.Output) != 0 {
		t.Fatalf("a refused pipeline must not run partially: %+v", rec.Stages)
	}
}

func TestCommandWithoutSandboxIsUnavailable(t *testing.T) {
	r := &Runner{}
	r.Policy.Allow("cat", "/bin/cat")
	rec := r.Run(context.Background(), Request{ArtifactRoot: mustGather, What: What{Pipeline: []expert.Stage{argv("cat", "x")}}})
	if rec.Status != StatusUnavailable || !strings.Contains(rec.Reason, "need a sandbox") {
		t.Fatalf("status %s: %s", rec.Status, rec.Reason)
	}
}

func TestShellMetacharactersAreLiteral(t *testing.T) {
	r := sandboxRunner(t, "echo")
	root := realRoot(t, mustGather)
	args := []string{"a|b", "$(id)", "`id`", "*", "; rm -rf /", "$HOME", "x > y"}
	rec := r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{argv(append([]string{"echo"}, args...)...)}}})
	if got, want := mustOK(t, rec), strings.Join(args, " ")+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Ad-hoc commands get no templating either.
	rec = r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{argv("echo", "{{x}}")}}})
	if got := mustOK(t, rec); got != "{{x}}\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSandboxContract(t *testing.T) {
	r := sandboxRunner(t, "cat", "touch", "curl", "ls", "env", "pwd")
	root := realRoot(t, mustGather)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	os.WriteFile(secret, []byte("secret"), 0o644)
	home, _ := os.UserHomeDir()

	collect := func(a ...string) Record {
		return r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{argv(a...)}}})
	}

	t.Run("reads the artifact root", func(t *testing.T) {
		abs := filepath.Join(root, "cluster-scoped-resources/core/nodes/master-0.yaml")
		if got := mustOK(t, collect("cat", abs)); !strings.Contains(got, "master-0") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("cannot read outside the artifact root", func(t *testing.T) {
		for _, p := range []string{secret, filepath.Join(home, ".ssh"), filepath.Join(home, ".zshrc")} {
			rec := collect("cat", p)
			if rec.Status == StatusOK || strings.Contains(string(rec.Output), "secret") {
				t.Errorf("read %s: status %s output %q", p, rec.Status, rec.Output)
			}
		}
		// bubblewrap makes the directories leading to each path it mounts,
		// so where the artifact root, scratch or a binary lies under home,
		// home lists that path's next name and nothing else.
		given := []string{root, r.ScratchParent}
		if ls, err := exec.LookPath("ls"); err == nil {
			given = append(given, filepath.Dir(ls))
			if target, err := filepath.EvalSymlinks(ls); err == nil {
				given = append(given, filepath.Dir(target))
			}
		}
		if rec := collect("ls", "-A", home); rec.Status == StatusOK {
			for _, name := range strings.Fields(string(rec.Output)) {
				if !onPathTo(filepath.Join(home, name), given...) {
					t.Errorf("listed %s in the home directory, on the way to nothing it was given: %q", name, rec.Output)
				}
			}
		}
	})
	t.Run("can write only to scratch", func(t *testing.T) {
		if rec := collect("touch", "made-in-scratch"); rec.Status != StatusOK {
			t.Errorf("scratch write: %s %s %v", rec.Status, rec.Reason, rec.Stages)
		}
		target := filepath.Join(outside, "written")
		if rec := collect("touch", target); rec.Status == StatusOK {
			t.Error("wrote outside scratch")
		}
		if _, err := os.Stat(target); err == nil {
			t.Error("file outside scratch exists")
		}
		abs := filepath.Join(root, "planted")
		if rec := collect("touch", abs); rec.Status == StatusOK {
			t.Error("wrote into the artifact root")
		}
		if _, err := os.Stat(abs); err == nil {
			os.Remove(abs)
			t.Error("file in the artifact root exists")
		}
	})
	t.Run("scratch is removed afterwards", func(t *testing.T) {
		mustOK(t, collect("pwd"))
		entries, _ := os.ReadDir(r.ScratchParent)
		if len(entries) != 0 {
			t.Errorf("scratch left behind: %v", entries)
		}
	})
	t.Run("no network", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip(err)
		}
		defer ln.Close()
		accepted := make(chan struct{}, 1)
		go func() {
			if c, err := ln.Accept(); err == nil {
				accepted <- struct{}{}
				c.Close()
			}
		}()
		rec := collect("curl", "-s", "-m", "3", fmt.Sprintf("http://%s/", ln.Addr()))
		select {
		case <-accepted:
			t.Fatal("the sandboxed command reached the network")
		case <-time.After(200 * time.Millisecond):
		}
		if rec.Status == StatusOK {
			t.Errorf("curl succeeded: %q", rec.Output)
		}
	})
	t.Run("minimal fixed environment", func(t *testing.T) {
		t.Setenv("COMPA_TEST_SECRET", "leak")
		got := mustOK(t, collect("env"))
		if strings.Contains(got, "leak") || !strings.Contains(got, "LANG=C") {
			t.Errorf("env = %q", got)
		}
	})
}

func TestTimeout(t *testing.T) {
	r := sandboxRunner(t, "sleep")
	r.Limits.Timeout = 300 * time.Millisecond
	start := time.Now()
	rec := r.Run(context.Background(), Request{ArtifactRoot: realRoot(t, mustGather), What: What{Pipeline: []expert.Stage{argv("sleep", "10")}}})
	if rec.Status != StatusFailed || !strings.Contains(rec.Reason, "timed out") {
		t.Fatalf("status %s: %s", rec.Status, rec.Reason)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestCommandOutputCap(t *testing.T) {
	r := sandboxRunner(t, "cat")
	r.Limits.OutputBytes = 64
	root := realRoot(t, mustGather)
	rec := r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{argv("cat", filepath.Join(root, ovnLog))}}})
	if rec.Status != StatusOK || !rec.Truncated || len(rec.Output) != 64 {
		t.Fatalf("status %s truncated %v len %d", rec.Status, rec.Truncated, len(rec.Output))
	}
}

func TestPipelineConnectsStages(t *testing.T) {
	r := sandboxRunner(t, "cat", "sort")
	root := realRoot(t, mustGather)
	rec := r.Run(context.Background(), Request{ArtifactRoot: root, What: What{Pipeline: []expert.Stage{
		argv("cat", filepath.Join(root, "cluster-scoped-resources/config.openshift.io/clusteroperators.yaml")),
		prim("yq", map[string]any{"filter": ".items[].metadata.name", "raw": true}),
		argv("sort"),
	}}})
	if got := mustOK(t, rec); got != "dns\nmachine-config\nnetwork\n" {
		t.Fatalf("got %q", got)
	}
	if len(rec.Stages) != 3 || rec.Sandbox == "" {
		t.Errorf("stages %+v sandbox %q", rec.Stages, rec.Sandbox)
	}
}

func TestNonZeroExitIsRecorded(t *testing.T) {
	r := sandboxRunner(t, "cat")
	rec := r.Run(context.Background(), Request{ArtifactRoot: realRoot(t, mustGather), What: What{Pipeline: []expert.Stage{argv("cat", "/nonexistent/file")}}})
	if rec.Status != StatusFailed || rec.ExitStatus == 0 || rec.Stages[0].Stderr == "" {
		t.Fatalf("status %s exit %d stderr %q", rec.Status, rec.ExitStatus, rec.Stages[0].Stderr)
	}
}

type memSink struct{ records []Record }

func (m *memSink) Put(r Record) error { m.records = append(m.records, r); return nil }

func loadFixture(t *testing.T, name string) *expert.Package {
	t.Helper()
	pkg, err := expert.Loader{CheckStage: CheckStage}.Load(filepath.Join("../../testdata/experts", name))
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func TestRunExpertTests(t *testing.T) {
	platform, networking := loadFixture(t, "platform"), loadFixture(t, "networking")
	r := &Runner{}
	sink := &memSink{}
	ctx := context.Background()

	rec := r.RunTest(ctx, mustGather, platform, "platform.node_conditions", nil)
	if got := mustOK(t, rec); got != "{\"name\":\"master-0\",\"ready\":\"True\"}\n{\"name\":\"worker-1\",\"ready\":\"False\"}\n" {
		t.Errorf("node_conditions: %q", got)
	}
	sink.Put(rec)
	rec = r.RunTest(ctx, mustGather, platform, "platform.node_conditions", map[string]any{"node": "worker-1"})
	if got := mustOK(t, rec); got != "{\"name\":\"worker-1\",\"ready\":\"False\"}\n" {
		t.Errorf("node_conditions(worker-1): %q", got)
	}
	sink.Put(rec)
	rec = r.RunTest(ctx, mustGather, platform, "platform.degraded_operators", nil)
	if got := mustOK(t, rec); got != "network\nmachine-config\n" {
		t.Errorf("degraded_operators: %q", got)
	}
	sink.Put(rec)
	rec = r.RunTest(ctx, mustGather, networking, "networking.ovn_errors", map[string]any{"max": 50})
	if got := mustOK(t, rec); !strings.Contains(got, `"count":3`) {
		t.Errorf("ovn_errors: %q", got)
	}
	sink.Put(rec)

	// The record says what ran, what it read, how it ended and how long it took.
	last := sink.records[len(sink.records)-1]
	if last.What.TestID != "networking.ovn_errors" || last.What.TestVersion != "0.1.0" || last.What.Expert != "openshift.networking@0.1.0" || last.What.Params["max"] != 50 {
		t.Errorf("what = %+v", last.What)
	}
	if last.What.Pipeline[0].Params["max_matches"] != 50 {
		t.Errorf("parameters must be substituted into the recorded pipeline: %+v", last.What.Pipeline[0])
	}
	if len(last.PathsRead) != 1 || last.PathsRead[0] != ovnLog || last.Duration <= 0 || !strings.HasPrefix(last.ID, "ev-") || last.Truncated {
		t.Errorf("record = %+v", last)
	}
	if sink.records[0].ID == sink.records[1].ID {
		t.Error("different results must have different evidence IDs")
	}

	// omc is declared by the expert but not allowlisted here.
	rec = r.RunTest(ctx, mustGather, networking, "networking.ovn_pods", nil)
	if rec.Status != StatusUnavailable || !strings.Contains(rec.Reason, `"omc" is not allowlisted`) || len(rec.Stages) != 0 {
		t.Errorf("ovn_pods: %s %s", rec.Status, rec.Reason)
	}
}

func TestRunTestUnavailable(t *testing.T) {
	platform, networking := loadFixture(t, "platform"), loadFixture(t, "networking")
	r := &Runner{}
	ctx := context.Background()
	cases := map[string]Record{
		"unknown test":      r.RunTest(ctx, mustGather, platform, "platform.nope", nil),
		"unknown parameter": r.RunTest(ctx, mustGather, platform, "platform.node_conditions", map[string]any{"nodes": "x"}),
		"bad parameter":     r.RunTest(ctx, mustGather, platform, "platform.node_conditions", map[string]any{"node": "Bad Name!"}),
		"out of range":      r.RunTest(ctx, mustGather, networking, "networking.ovn_errors", map[string]any{"max": 5000}),
		"precondition":      r.RunTest(ctx, t.TempDir(), platform, "platform.node_conditions", nil),
	}
	for name, rec := range cases {
		if rec.Status != StatusUnavailable || rec.Reason == "" || len(rec.Stages) != 0 {
			t.Errorf("%s: status %s reason %q", name, rec.Status, rec.Reason)
		}
	}
}

func TestLoaderUsesCheckStage(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"expert.yaml": "apiVersion: compa.lowmag.cc/expert/v1alpha3\nkind: Expert\nmetadata: {id: a.b, version: 0.1.0, summary: A test expert}\nspec: {engineApi: '>=0.2.0 <0.3.0', analyst: a.md, tests: [a.t]}\n",
		"a.md":        "x",
		"tests.yaml":  "apiVersion: compa.lowmag.cc/expert/v1alpha2\nkind: Tests\ntests: [{id: a.t, version: 0.1.0, run: [{grep: {pattern: x, recurse: true}}]}]\n",
	}
	for n, c := range files {
		os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
	}
	_, err := expert.Loader{CheckStage: CheckStage}.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "recurse") {
		t.Fatalf("got %v", err)
	}
}

// TestSandboxNetworkControl is the positive control for the no-network
// case: the same curl, with network allowed, reaches the listener. Without
// it, the no-network test would pass if curl failed for any other reason.
func TestSandboxNetworkControl(t *testing.T) {
	sb, err := DefaultSandbox()
	if err != nil {
		SkipWithoutSandbox(t, err)
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	scratch := t.TempDir()
	cmd, err := sb.Command(context.Background(), SandboxSpec{
		Argv:    []string{curl, "-s", "-m", "3", fmt.Sprintf("http://%s/", ln.Addr())},
		Scratch: scratch, Network: true, Env: []string{"PATH=/usr/bin:/bin", "HOME=" + scratch},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Run()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("with network allowed, curl did not connect; the no-network test proves nothing")
	}
}

// onPathTo reports whether p is one of the given paths or a directory on
// the way to one: what a sandbox shows of the host when it mounts them.
// Each path is taken as given and with its links resolved. The contract
// package's OnPathTo, which this package's tests cannot import.
func onPathTo(p string, given ...string) bool {
	var all []string
	for _, g := range given {
		all = append(all, g)
		if target, err := filepath.EvalSymlinks(g); err == nil {
			all = append(all, target)
		}
	}
	ps := []string{p}
	if target, err := filepath.EvalSymlinks(p); err == nil {
		ps = append(ps, target)
	}
	for _, q := range ps {
		for _, g := range all {
			if g == q || strings.HasPrefix(g, q+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
