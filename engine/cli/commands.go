package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lowmagic/compa-engine/engine"
	"github.com/lowmagic/compa-engine/engine/chat"
	"github.com/lowmagic/compa-engine/engine/text"
)

// errNoTerminal refuses the chat where its input or output is not a
// terminal, which it draws on and reads keys from.
var errNoTerminal = errors.New("the chat needs a terminal for its input and output; compa ask <case> <question> runs one turn without one")

// chatOn opens a case, its details and attachment list read, and runs the
// chat on it, which reads the rest.
func (l *commandLine) chatOn(ctx context.Context, id string, o chat.Options, in io.Reader, out io.Writer) error {
	if !interactive(in, out) {
		return errNoTerminal
	}
	s, err := l.e.StartSession(ctx, id, l.fetchOffer(in, out))
	if err != nil {
		return err
	}
	defer s.Close()
	return runChat(ctx, s, o, in, out)
}

// chatAlone runs the chat with no case: the chat named, or a new one, over
// dir's artifacts when given.
func (l *commandLine) chatAlone(ctx context.Context, name, dir string, in io.Reader, out io.Writer) error {
	if !interactive(in, out) {
		return errNoTerminal
	}
	s, err := l.e.StartChat(ctx, name, dir)
	if err != nil {
		return err
	}
	defer s.Close()
	return runChat(ctx, s, chat.Options{NoOpen: true}, in, out)
}

// runChat is the chat, engine/chat; tests replace it.
var runChat = chat.Run

// interactive reports whether in and out are the engineer's terminal,
// where a command may ask before it goes on; tests replace it.
var interactive = func(in io.Reader, out io.Writer) bool {
	i, ok := in.(*os.File)
	o, ok2 := out.(*os.File)
	return ok && ok2 && isTerminal(i) && isTerminal(o)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// fetchOffer is the offer to fetch the artifacts of a case that has none,
// made on the engineer's terminal where the artifact source can fetch them
// (engine.ArtifactFetcher); nil where it cannot or there is no terminal to
// ask on. A fetch that fails is said, and the case opens without its
// folder; only a cancelled one stops it.
func (l *commandLine) fetchOffer(in io.Reader, out io.Writer) func(context.Context, string) error {
	f, ok := l.artifacts.(engine.ArtifactFetcher)
	if !ok || !interactive(in, out) {
		return nil
	}
	return func(ctx context.Context, id string) error {
		_, err := l.artifacts.Root(ctx, id)
		if !errors.Is(err, engine.ErrNoArtifacts) {
			return nil
		}
		command := text.Safe(f.FetchCommand(id))
		fmt.Fprintf(out, "%s\nfetch the case's artifacts with %s? [Y/n] ", text.Safe(err.Error()), command)
		answer, err := readLine(ctx, in)
		if ctx.Err() != nil {
			fmt.Fprintln(out)
			return ctx.Err()
		}
		if err != nil && answer == "" {
			fmt.Fprintln(out)
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "y", "yes":
		default:
			return nil
		}
		if err := f.Fetch(ctx, id, in, out); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(out, "%s failed: %s; going on without the case folder\n", command, text.Safe(err.Error()))
		}
		return nil
	}
}

// readLine reads a line from in, or returns the context's error once it is
// done. Interrupt cancels the context rather than ending the process
// (Main), and a read from a terminal goes on past it, so a prompt read
// without this waits for Enter after Ctrl+C. A read cut short is left
// running; the command it belongs to then ends with the context's error.
func readLine(ctx context.Context, in io.Reader) (string, error) {
	type line struct {
		s   string
		err error
	}
	got := make(chan line, 1)
	go func() {
		s, err := bufio.NewReader(in).ReadString('\n')
		got <- line{s, err}
	}()
	select {
	case l := <-got:
		return l.s, l.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// askOnce runs one turn of the case's conversation, the case prepared
// first as the chat's opening prepares it, and prints the answer.
func (l *commandLine) askOnce(ctx context.Context, id, question string, in io.Reader, out io.Writer) error {
	s, err := l.e.StartSession(ctx, id, l.fetchOffer(in, out))
	if err != nil {
		return err
	}
	defer s.Close()
	p, err := s.Prepare(ctx, engine.PrepareOptions{Call: true})
	if err != nil {
		return err
	}
	notes := p.Problems
	switch {
	case p.Ledger.Running:
		notes = append(notes, fmt.Errorf("the case ledger was still reading the comments after %s; this answer goes on without it", p.Ledger.Waited))
	case p.Ledger.Err != nil:
		notes = append(notes, p.Ledger.Err)
	}
	for _, n := range notes {
		if _, err := fmt.Fprintf(out, "note: %v\n", n); err != nil {
			return err
		}
	}
	if len(notes) > 0 {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	}
	reply, err := s.Talk(ctx, question, nil)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, strings.TrimSpace(reply.Text)); err != nil {
		return err
	}
	return writeCheck(out, reply.Check)
}

// writeCheck writes beneath an answer what the editor's check made of it:
// its findings left uncorrected, a revision that failed, the anchors a
// revision was asked to fulfil, and in one line whether it was corrected,
// revised or unchecked.
func writeCheck(out io.Writer, c engine.AnswerCheck) error {
	var b strings.Builder
	for _, n := range c.Notes() {
		if n.Kind == engine.NoteWarning {
			fmt.Fprintf(&b, "! %s\n", n.Text)
			continue
		}
		fmt.Fprintf(&b, "⚠ %s %s\n", n.Text, n.Why)
		if n.AtMost != "" {
			fmt.Fprintf(&b, "  at most: %s\n", n.AtMost)
		}
	}
	for _, f := range c.Asked() {
		fmt.Fprintf(&b, "asked of the revision: %s\n", engine.AnchorLine(f))
	}
	if words := c.Words(); len(words) > 0 {
		fmt.Fprintf(&b, "note: %s\n", strings.Join(words, "; "))
	}
	if b.Len() == 0 {
		return nil
	}
	_, err := fmt.Fprint(out, "\n"+b.String())
	return err
}

func (l *commandLine) caseOpen(ctx context.Context, id string, fetch func(context.Context, string) error, out io.Writer) error {
	o, err := l.e.OpenCase(ctx, id, fetch)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "case %s: %s\n", id, text.Safe(o.Case.Subject))
	describe(out, o.Store)
	if t := o.Attachments; t != nil {
		files := fmt.Sprintf("%s, %d bytes", text.Plural(int(t.Files), "file"), t.Bytes)
		if t.AtLeast {
			files = "at least " + files
		}
		fmt.Fprintf(out, "attachments: %d, holding %s\n", t.Attachments, files)
	}
	fmt.Fprintf(out, "evidence: %s, %d new\n", text.Plural(o.Evidence, "item"), o.New)
	fmt.Fprintln(out, o.Experts)
	for _, dir := range o.Removed {
		fmt.Fprintf(out, "removed an expired store: %s\n", dir)
	}
	if o.SweepErr != nil {
		fmt.Fprintf(out, "warning: sweeping expired stores: %v\n", o.SweepErr)
	}
	return nil
}

// describe says where a case's artifacts and store are.
func describe(out io.Writer, s engine.StoreInfo) {
	if s.ArtifactRoot == "" {
		why := ""
		if s.NoArtifacts != nil {
			why = " (" + text.Safe(s.NoArtifacts.Error()) + ")"
		}
		fmt.Fprintf(out, "artifacts: none found%s; the store is in your home directory\n", why)
	} else {
		fmt.Fprintf(out, "artifacts: %s\n", s.ArtifactRoot)
	}
	fmt.Fprintf(out, "store: %s\n", s.Dir)
	for _, n := range s.Notes {
		fmt.Fprintf(out, "store: %s\n", text.Safe(n))
	}
}

// caseAllowModels shows the override's notice and records the override
// when the account's name is typed on the next line.
func (l *commandLine) caseAllowModels(ctx context.Context, id string, in io.Reader, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	lines, err := s.OverrideNotice(ctx)
	if err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Fprintln(out, text.Safe(line))
	}
	fmt.Fprint(out, "> ")
	typed, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && typed == "" {
		return fmt.Errorf("nothing typed; nothing changed: %w", err)
	}
	o, err := s.AllowModels(ctx, strings.TrimSpace(typed), engine.LoginName())
	if err != nil {
		return err
	}
	fmt.Fprintln(out, o.Line())
	return nil
}

func (l *commandLine) caseRevokeModels(ctx context.Context, id string, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.RevokeModels(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "the override allowing model calls is withdrawn")
	return nil
}

func (l *commandLine) caseShow(ctx context.Context, id string, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(out, "case %s\n", id)
	describe(out, s.Store())
	if line := s.Barred(ctx).Line(); line != "" {
		fmt.Fprintln(out, line)
	}
	if from := l.e.ConfigSources(); len(from) > 0 {
		fmt.Fprintf(out, "configuration: %s\n", strings.Join(from, ", then "))
	}
	items, err := s.EvidenceList()
	if err != nil {
		return err
	}
	byType := map[string]int{}
	latestOf := map[string]string{}
	for _, it := range items {
		byType[it.Ref.Type]++
		latestOf[it.Ref.Type] = it.Ref.ID
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, fmt.Sprintf("%s %d", t, byType[t]))
	}
	sort.Strings(types)
	fmt.Fprintf(out, "evidence: %s (%s)\n", text.Plural(len(items), "item"), strings.Join(types, ", "))
	for _, typ := range []string{engine.TypeCaseMetadata, engine.TypeArtifactOverview} {
		id, ok := latestOf[typ]
		if !ok {
			continue
		}
		it, err := s.Evidence(id)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "\nlatest %s, %s:\n", strings.ReplaceAll(typ, "_", " "), id)
		writeBounded(out, it.Content, 16<<10)
	}
	return nil
}

// caseResend sends a stored call again and prints the original and the
// new call side by side.
func (l *commandLine) caseResend(ctx context.Context, caseID, id, modelName, effort string, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, caseID)
	if err != nil {
		return err
	}
	defer s.Close()
	r, err := s.Resend(ctx, id, modelName, effort, nil)
	fmt.Fprint(out, engine.RenderResent(r))
	return err
}

// caseTrajectory prints the case's trajectory, as text or as JSON.
func (l *commandLine) caseTrajectory(ctx context.Context, id string, asJSON bool, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	t, err := s.Trajectory(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		data, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return err
		}
		_, err = out.Write(append(data, '\n'))
		return err
	}
	_, err = fmt.Fprintf(out, "case %s\n%s", id, engine.RenderTrajectory(t))
	return err
}

// caseComments applies one rule change, if any, and prints the case's
// comments, which models read and which not, and the rules.
func (l *commandLine) caseComments(ctx context.Context, id, kind, value string, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	switch kind {
	case "":
	case "include":
		if err := s.IncludeComments(value); err != nil {
			return err
		}
	default:
		if _, err := s.ExcludeComments(kind, value); err != nil {
			return err
		}
	}
	rules, err := s.CommentRules()
	if err != nil {
		return err
	}
	all := s.Comments()
	role := s.SupportRole()
	read := 0
	for _, cm := range all {
		mark, why := "  ", ""
		if r := engine.ExcludedBy(rules, cm); r != nil {
			mark, why = "✕ ", "  (left out by "+r.ID+")"
		} else {
			read++
		}
		first, _, _ := strings.Cut(cm.Body, "\n")
		fmt.Fprintln(out, text.Safe(fmt.Sprintf("%s%s %s %-7s %-*s %s: %s%s", mark, cm.ID, cm.Created.UTC().Format("2006-01-02 15:04"), visLabel(cm), kindWidth(all, role), kindLabel(cm, role), cm.Author, first, why)))
	}
	fmt.Fprintf(out, "models read %d of %d comments\n", read, len(all))
	for _, r := range rules {
		fmt.Fprintln(out, text.Safe("rule "+r.ID+": "+r.Describe(s.SupportName())))
	}
	return nil
}

// printComments prints every comment whole, newest first, each headed as
// the comments are listed: its text as stored, flush and unwrapped, in
// the terminal's own colours, so it copies as written. It marks nothing
// read.
func (l *commandLine) printComments(ctx context.Context, id string, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	all := s.Comments()
	role := s.SupportRole()
	for i, cm := range all {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, text.Safe(fmt.Sprintf("%s %s %-7s %-*s %s", cm.ID, cm.Created.UTC().Format("2006-01-02 15:04"), visLabel(cm), kindWidth(all, role), kindLabel(cm, role), cm.Author)))
		fmt.Fprintln(out, text.Safe(strings.ReplaceAll(cm.Body, "\r\n", "\n")))
	}
	return nil
}

// visLabel and kindLabel are a comment's visibility and author kind as
// the comments are listed.
func visLabel(cm engine.CaseComment) string {
	switch {
	case cm.Private():
		return "PRIVATE"
	case cm.Visibility == string(engine.VisibilityPartner):
		return "PARTNER"
	}
	return "PUBLIC"
}

// kindWidth is the width of the author kind's column: the widest label
// among the comments listed, and no narrower than the customer's.
func kindWidth(comments []engine.CaseComment, supportRole string) int {
	w := len("CUSTOMER")
	for _, cm := range comments {
		w = max(w, len([]rune(kindLabel(cm, supportRole))))
	}
	return w
}

// kindLabel is the author kind as listed: the deployment's label for a
// support author, the customer's, or any other kind the source reports
// under its own name.
func kindLabel(cm engine.CaseComment, supportRole string) string {
	switch cm.AuthorKind {
	case engine.AuthorSupport:
		return strings.ToUpper(supportRole)
	case engine.AuthorCustomer:
		return "CUSTOMER"
	}
	// Blank, never a guess, when the source does not say.
	return strings.ToUpper(cm.AuthorKind)
}

// evidenceNamed prints the case's named evidence, newest first, as the
// chat's evidence browser lists it, or one item's lines under their
// numbers, for a pager or grep.
func (l *commandLine) evidenceNamed(ctx context.Context, id, name string, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	if name == "" {
		named, err := s.Evidences()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tLINES\tBYTES\tWHAT")
		for _, n := range named {
			by, _, _ := strings.Cut(n.By, "\n")
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", n.Name, n.Item.Size.Lines, n.Item.Size.Bytes, text.Safe(by))
		}
		return tw.Flush()
	}
	it, err := s.EvidenceNamed(name)
	if err != nil {
		return fmt.Errorf("case %s: %w", id, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(it.Content), "\n"), "\n")
	width := len(strconv.Itoa(len(lines)))
	fmt.Fprintf(out, "%s · %s · %d lines\n", name, text.Safe(engine.ProducedBy(it)), len(lines))
	for i, line := range lines {
		fmt.Fprintf(out, "%*d  %s\n", width, i+1, text.Safe(line))
	}
	return nil
}

func (l *commandLine) evidenceList(ctx context.Context, id string, limit int, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	items, err := s.EvidenceList()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tBYTES\tLINES\tCREATED\tLOCATION")
	for i, it := range items {
		if i == limit {
			break
		}
		loc, _, _ := strings.Cut(it.Location, "\n")
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", it.Ref.ID, it.Ref.Type, it.Size.Bytes, it.Size.Lines, it.CreatedAt.Format(time.RFC3339), text.Safe(loc))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(items) > limit {
		fmt.Fprintf(out, "… %d more; use --limit\n", len(items)-limit)
	}
	return nil
}

func (l *commandLine) evidenceShow(ctx context.Context, id, evID string, maxBytes int, out io.Writer) error {
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	it, err := s.DescribeEvidence(evID)
	if errors.Is(err, engine.ErrEvidenceNotFound) {
		return fmt.Errorf("case %s has no evidence %s", id, evID)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "id: %s\ntype: %s\nsource: %s\ncollected by: %s\n", it.Ref.ID, it.Ref.Type, text.Safe(it.Ref.Provenance.Source), it.Ref.Provenance.CollectedBy)
	for _, loc := range strings.Split(it.Location, "\n") {
		if loc != "" {
			fmt.Fprintf(out, "location: %s\n", text.Safe(loc))
		}
	}
	size := fmt.Sprintf("%d bytes, %d lines", it.Size.Bytes, it.Size.Lines)
	if it.Size.LowerBound {
		size = "at least " + size
	}
	fmt.Fprintf(out, "size: %s\ncreated: %s\n", size, it.CreatedAt.Format(time.RFC3339))
	if it.Note != "" {
		fmt.Fprintf(out, "note: %s\n", text.Safe(strings.ReplaceAll(it.Note, "\n", " ")))
	}
	fmt.Fprintln(out)
	// Streamed, so showing the start of a large item reads only that much.
	r, err := s.OpenEvidence(evID)
	if err != nil {
		return err
	}
	defer r.Close()
	head, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)))
	if err != nil {
		return err
	}
	writeBounded(out, head, maxBytes)
	if rest := it.Size.Bytes - int64(len(head)); rest > 0 {
		fmt.Fprintf(out, "[… %d more bytes; use --max-bytes]\n", rest)
	}
	return nil
}

func (l *commandLine) ingest(ctx context.Context, id string, files []string, note string, in io.Reader, out io.Writer) error {
	if len(files) == 0 {
		if f, ok := in.(*os.File); ok && isTerminal(f) {
			return fmt.Errorf("%w: give files, or pipe what to ingest into compa", errUsage)
		}
		files = []string{"-"}
	}
	s, err := l.e.OpenCaseStore(ctx, id)
	if err != nil {
		return err
	}
	defer s.Close()
	var failed int
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		it, isNew, err := ingestOne(s, name, in, note)
		if err != nil {
			fmt.Fprintf(out, "%s: %v\n", text.Safe(name), err)
			failed++
			continue
		}
		state := "already stored"
		if isNew {
			state = "new"
		}
		fmt.Fprintf(out, "%s  %s  %d bytes, %s (%s)\n", it.Ref.ID, text.Safe(it.Location), it.Size.Bytes, text.Plural(int(it.Size.Lines), "line"), state)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d inputs were not ingested", failed, len(files))
	}
	return nil
}

// ingestOne ingests a file, or standard input for "-", recording a file
// by its absolute path.
func ingestOne(s *engine.CaseStore, name string, in io.Reader, note string) (engine.EvidenceItem, bool, error) {
	if name == "-" {
		return s.Ingest(in, "stdin", note)
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return engine.EvidenceItem{}, false, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return engine.EvidenceItem{}, false, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return engine.EvidenceItem{}, false, fmt.Errorf("%s is a directory", abs)
	}
	return s.Ingest(f, abs, note)
}

// writeBounded writes at most limit bytes of content, saying how much it
// left out.
func writeBounded(out io.Writer, content []byte, limit int) {
	shown := content
	if len(shown) > limit {
		shown = shown[:limit]
	}
	fmt.Fprint(out, text.Safe(string(shown)))
	if len(shown) > 0 && shown[len(shown)-1] != '\n' {
		fmt.Fprintln(out)
	}
	if len(content) > limit {
		fmt.Fprintf(out, "[… %d more bytes; use --max-bytes]\n", len(content)-limit)
	}
}
