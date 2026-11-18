package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/engine/text"
	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/redact"
	apischema "github.com/lowmagic/compa-engine/internal/schema"
)

// The editor: after
// every answer of the main conversation, before it is printed, the
// deterministic checks and then one call of a model of its own, given the
// answer, its statements, what the checks found and the lines it cites,
// never the conversation. What needs only those it corrects, and the
// correction is printed in the answer's place; what needs the conversation
// it leaves as anchors, and the main model is asked once, calling no tool,
// to write the answer again with each fulfilled, its revision printed. No
// second round. The answer stays as the model wrote it: a correction, the
// ask and the revision are messages of their own. A deployment with no
// editor profile runs none.

// EditorTask is the task type the editor's calls are recorded as, and its
// model profile resolved for (class editor).
const EditorTask = "answers.edit"

// editorConversation is the kind of an editor call's conversation.
const editorConversation = "editor"

// checkBudget is how many model calls one check, the editor's or the
// reviewer's, may make in all: the findings, and one retry of findings
// refused whole or of an answer that called no tool.
const checkBudget = 2

// checkRetry is the engine's word to a check's model that answered
// without calling check_findings: the check's one retry.
const checkRetry = "Answer by calling " + check.ToolName + " with every statement accounted for; write nothing else."

// editorBound is how long the editor's check may take, its retry or
// follow-up included, before the answer is printed as written, said
// unchecked. Measured live, one call of gemini-3.8-flash at low
// took 8–10 s over about 5,500 input tokens, so the 20 s first set left
// no room for a second: the bound is 45 s.
var editorBound = 45 * time.Second

// The steps of the editor's check a TalkStage event names.
const (
	StageChecking = "checking"
	StageRevising = "revising"
)

// How a check ended, as kept.
const (
	checkClean          = "clean"
	checkCorrected      = "corrected"
	checkFlagged        = "flagged"
	checkRevised        = "revised"
	checkRevisionFailed = "revision_failed"
	checkUnchecked      = "unchecked"
)

// CheckFinding is one of a check's findings, as validated.
type CheckFinding = check.Finding

// AnswerCheck is what the editor made of an answer, as it is shown beneath
// it; the zero value where no editor ran.
type AnswerCheck struct {
	// Ran says the editor was asked.
	Ran bool
	// Unchecked says why the answer went unchecked: the editor failed or
	// passed its bound.
	Unchecked string
	// Corrected are the statements the editor's corrected text changed,
	// by ID; Flagged its findings it wrote no correction for.
	Corrected []string
	Flagged   []CheckFinding
	// CorrectedCites are the citations the editor's corrected text gave
	// statements in place of theirs, [e4:17-18], each once
	// (check.CorrectedCites).
	CorrectedCites []string
	// Anchors are what the main model's revision was asked to fulfil;
	// Revised says it revised the answer, RevisionFailed why it did not.
	Anchors        []CheckFinding
	Revised        bool
	RevisionFailed string
	// Added are the citations naming lines that the revision wrote and
	// the answer as written did not have, [e4:13-14]: the revision read no
	// evidence, so nothing it read gave it those lines.
	Added []string
	// Statements is how many statements the check was given, Accounted
	// how many its findings account for: a finding, supported or skipped.
	Statements, Accounted int
	// FollowUp are why the editor's follow-up on the statements its first
	// findings left unaccounted, or its correction, was not taken.
	FollowUp []string
}

// Words are what an answer's footer says of its check, in order.
func (a AnswerCheck) Words() []string {
	var out []string
	if a.Unchecked != "" {
		out = append(out, "unchecked: "+a.Unchecked)
	}
	if a.Accounted < a.Statements {
		out = append(out, fmt.Sprintf("checked %d of %s", a.Accounted, text.Plural(a.Statements, "statement")))
	}
	if n := len(a.Corrected); n > 0 {
		words := "corrected by the editor: " + text.Plural(n, "statement")
		if m := len(a.CorrectedCites); m > 0 {
			words += ", " + text.Plural(m, "citation")
		}
		out = append(out, words)
	}
	if a.Revised {
		out = append(out, "revised for "+text.Plural(len(a.Anchors), "anchor"))
	}
	if n := len(a.Added); n > 0 {
		out = append(out, text.Plural(n, "citation")+" added by the revision")
	}
	return out
}

// Changed says the text shown is not the answer as the model wrote it.
func (a AnswerCheck) Changed() bool { return len(a.Corrected) > 0 || a.Revised }

// The kinds of CheckNote.
const (
	NoteFlag    = "flag"
	NoteWarning = "warning"
)

// CheckNote is a line beneath an answer saying what of the editor's check
// its text does not show: a finding it did not correct (NoteFlag, with
// Why and the most the lines justify, AtMost), a citation the revision
// added (NoteFlag), or a revision or a follow-up that failed
// (NoteWarning).
type CheckNote struct {
	Kind, Text, Why, AtMost string
}

// Notes are the lines beneath an answer of what its check found that its
// text does not show, as the chat and compa ask print them; the anchors
// asked of the revision are apart (Asked).
func (a AnswerCheck) Notes() []CheckNote {
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	var out []CheckNote
	for _, f := range a.Flagged {
		out = append(out, CheckNote{Kind: NoteFlag, Text: f.Statement + " " + f.Kind + " · " + strconv.Quote(flat(f.Quote)), Why: flat(f.Why), AtMost: flat(f.Suggested)})
	}
	for _, c := range a.Added {
		out = append(out, CheckNote{Kind: NoteFlag, Text: c, Why: revisionAddedWhy})
	}
	if a.RevisionFailed != "" {
		shown := "the answer as written is printed"
		if len(a.Corrected) > 0 {
			shown = "the editor's corrected text is printed"
		}
		out = append(out, CheckNote{Kind: NoteWarning, Text: "the revision failed: " + flat(a.RevisionFailed) + "; " + shown})
	}
	for _, why := range a.FollowUp {
		out = append(out, CheckNote{Kind: NoteWarning, Text: "the editor's follow-up on the statements left unaccounted was not taken: " + flat(why)})
	}
	return out
}

// Asked are the anchors a revision was asked to fulfil, where one was
// asked: revised, or failing. compa ask lists each beneath the answer
// (AnchorLine); the chat sums them up in one line and lists them in the
// checks tab.
func (a AnswerCheck) Asked() []CheckFinding {
	if a.Revised || a.RevisionFailed != "" {
		return a.Anchors
	}
	return nil
}

// AnchorLine is an anchor as the revision is asked to fulfil it and compa
// ask lists it: s4 [unsupported] "the words" → what fulfils it.
func AnchorLine(f CheckFinding) string {
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return fmt.Sprintf("%s [%s] %s → %s", f.Statement, f.Kind, strconv.Quote(flat(f.Quote)), flat(f.Suggested))
}

// EditsAnswers says whether the editor checks each answer before it is
// printed: where the deployment configures its profile.
func (s *Session) EditsAnswers() bool { return s.e.config.Profile(EditorTask).Defined }

// edit has the editor check the answer reply ended with, in l's
// conversation, and returns what is to be printed with what the check
// made of it. With no editor configured, or no answer, it returns reply.
// An editor that fails or passes editorBound leaves the answer as
// written, said unchecked; only the engineer's cancelling fails the turn.
func (s *Session) edit(ctx context.Context, l *companion.Loop, reply companion.Reply, on func(TalkEvent)) (TalkReply, error) {
	out := TalkReply{Reply: reply}
	pc := s.e.config.Profile(EditorTask)
	if !pc.Defined || strings.TrimSpace(reply.Text) == "" {
		return out, nil
	}
	if on == nil {
		on = func(TalkEvent) {}
	}
	conv := reply.Conversation
	msgs, err := l.Store.Messages(ctx, conv)
	if err != nil {
		return out, err
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].Kind != companion.KindModel || len(msgs[len(msgs)-1].Calls) > 0 {
		return out, nil
	}
	checked := msgs[len(msgs)-1].Invocation
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return out, err
	}
	answers := answersOf(history)
	if len(answers) == 0 {
		return out, nil
	}
	on(TalkEvent{Kind: TalkStage, Conversation: conv, Text: StageChecking})
	answer := answers[len(answers)-1]
	run := checkRun{role: check.Editor, conv: conv, checked: checked, name: answer.label}
	bounded, cancel := context.WithTimeout(ctx, editorBound)
	err = s.runEditor(bounded, pc, answer, answers[:len(answers)-1], &run, on)
	cancel()
	// The answer is stored whatever the engineer does now: how it was
	// shown is kept with it, the engineer's cancelling included, each
	// write within a bound of its own from when it is made, however long
	// the revision took (finalContext).
	if ctx.Err() != nil {
		s.keepCheck(ctx, run, checkUnchecked, "cancelled", on)
		out.Check = AnswerCheck{Ran: true, Unchecked: "cancelled"}
		return out, ctx.Err()
	}
	// The editor failing degrades the answer to unchecked, never the turn.
	if why := editorFailed(err); why != "" {
		s.keepCheck(ctx, run, checkUnchecked, why, on)
		out.Check = AnswerCheck{Ran: true, Unchecked: why}
		return out, nil
	}
	var anchors []CheckFinding
	fixes := false
	for _, f := range run.findings.Findings {
		if f.Anchor {
			anchors = append(anchors, f)
		} else {
			fixes = true
		}
	}
	corrected := strings.TrimSpace(run.findings.Corrected)
	if corrected == strings.TrimSpace(reply.Text) {
		corrected = ""
	}
	run.findings.Corrected = corrected
	shown, outcome, why := reply.Text, checkClean, ""
	if corrected != "" {
		shown = corrected
	}
	switch {
	case len(anchors) > 0:
		on(TalkEvent{Kind: TalkStage, Conversation: conv, Text: StageRevising})
		revised, err := l.Ask(ctx, conv, companion.KindRevise, reviseAsk(corrected, anchors), on)
		if err == nil && strings.TrimSpace(revised) == "" {
			err = errors.New("the model's revision holds no text")
		}
		if err == nil {
			shown, outcome = revised, checkRevised
			break
		}
		outcome, why = checkRevisionFailed, err.Error()
		if ctx.Err() != nil {
			why = "cancelled"
		}
		// Whatever the failed call left in the conversation, what the
		// engineer was shown is kept after it, so the conversation, its
		// history and the model all read the answer as shown.
		fin, finished := finalContext(ctx)
		err = l.Show(fin, conv, shown)
		finished()
		if err != nil {
			return out, err
		}
	case corrected != "":
		fin, finished := finalContext(ctx)
		err := l.Correct(fin, conv, corrected)
		finished()
		if err != nil {
			return out, err
		}
		outcome = checkCorrected
	case fixes:
		outcome = checkFlagged
	}
	s.keepCheck(ctx, run, outcome, why, on)
	if shown != reply.Text {
		on(TalkEvent{Kind: TalkReplaced, Conversation: conv, Text: shown})
	}
	out.Text = shown
	out.Check = answerCheckOf(outcome, why, reply.Text, run.statements, run.findings, run.dropped)
	if outcome == checkRevised {
		// Read past the engineer's cancelling, as the answer is shown.
		fin, finished := finalContext(ctx)
		shown, err := shownLines(fin, l.Store, conv, msgs)
		finished()
		if err != nil {
			return out, err
		}
		out.Check.Added = revisionAdded(reply.Text, out.Text, out.Check.CorrectedCites, shown)
	}
	return out, ctx.Err()
}

// editorFinalBound bounds each write keeping how an answer was shown, or
// how a check ended, once the check has ended, the engineer's cancelling
// included: writes to the case's store.
var editorFinalBound = 5 * time.Second

// finalContext bounds one such write from when it is made: detached from
// ctx, so the engineer's cancelling does not stop it, and from any bound
// ctx carries, so a slow revision before it does not spend it.
func finalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), editorFinalBound)
}

// editorFailed says why the editor's check failed, err its error, or
// nothing where it did not.
func editorFailed(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "the editor passed its " + editorBound.String() + " bound"
	}
	return "the editor failed: " + err.Error()
}

// answerCheckOf is what a check that ended as outcome, why, with findings
// f and the elements validation dropped, made of answer, the text it
// checked, given statements statements: the same live and from the
// store. The statements corrected are those the corrected text changes,
// whatever finding called for it, and the citations corrected those it
// gives them in place of theirs; a finding whose statement it leaves as
// written is flagged. The statements accounted for are those a finding,
// supported or skipped names; a follow-up not taken, or its correction,
// is said.
func answerCheckOf(outcome, why, answer string, statements int, f check.Findings, dropped []check.Rejected) AnswerCheck {
	a := AnswerCheck{Ran: true}
	if outcome == checkUnchecked {
		a.Unchecked = why
		return a
	}
	accounted := map[string]bool{}
	for _, fi := range f.Findings {
		accounted[fi.Statement] = true
	}
	for _, id := range f.Supported {
		accounted[id] = true
	}
	for _, sk := range f.Skipped {
		accounted[sk.Statement] = true
	}
	a.Statements, a.Accounted = statements, len(accounted)
	for _, d := range dropped {
		if d.Element == followUpElement || d.Element == followUpElement+".corrected" {
			a.FollowUp = append(a.FollowUp, d.Reason)
		}
	}
	if f.Corrected != "" {
		a.Corrected = changedStatements(answer, f.Corrected)
		for _, c := range check.CorrectedCites(answer, f.Corrected, checkCites) {
			a.CorrectedCites = append(a.CorrectedCites, c.Place())
		}
	}
	for _, fi := range f.Findings {
		switch {
		case fi.Anchor:
			a.Anchors = append(a.Anchors, fi)
		case !slices.Contains(a.Corrected, fi.Statement):
			a.Flagged = append(a.Flagged, fi)
		}
	}
	a.Revised = outcome == checkRevised
	if outcome == checkRevisionFailed {
		a.RevisionFailed = why
	}
	return a
}

// changedStatements are the statements of answer, by ID, that corrected
// does not hold as written, its whitespace aside: those it reworded or
// left out.
func changedStatements(answer, corrected string) []string {
	kept := map[string]bool{}
	for _, st := range check.Split(corrected) {
		kept[strings.Join(strings.Fields(st.Text), " ")] = true
	}
	var out []string
	for _, st := range check.Split(answer) {
		if !kept[strings.Join(strings.Fields(st.Text), " ")] {
			out = append(out, st.ID)
		}
	}
	return out
}

// reviseAsk is the engine's message asking the main model for its answer
// again with each anchor fulfilled, from the editor's corrected text where
// it corrected the wording, saying the call reads no evidence: citations
// kept as they are, only lines the conversation showed cited, and a claim
// none holds qualified or removed (revisionAdded flags what it adds).
func reviseAsk(corrected string, anchors []CheckFinding) string {
	var b strings.Builder
	b.WriteString("# Revise your answer\n\nThe engine's editor read your answer above against the lines it cites, without this conversation, and left what only the conversation can settle:\n\n")
	for _, a := range anchors {
		b.WriteString("- " + AnchorLine(a) + "\n")
	}
	if corrected != "" {
		b.WriteString("\nIt corrected the wording of the rest. Its corrected text, to revise from:\n\n" + corrected + "\n")
	}
	b.WriteString("\nWrite the whole answer again with each point fulfilled — the source named, the statement marked inferred or unresolved, or it removed — and the rest kept as it stands. " +
		"You read no evidence in this call: keep each citation as it is, cite only lines this conversation has already shown you, and never write line numbers you have not seen; " +
		"where no line shown holds a claim, qualify it or remove it. Call no tool, and write only the answer: it is printed in place of the one above.")
	return b.String()
}

// revisionAddedWhy is why a citation the revision added is flagged.
const revisionAddedWhy = "added by the revision, naming lines no tool result showed it"

// revisionAdded are the citations naming lines that revised holds and
// neither answer, the answer as written, nor the editor's corrected
// citations do, each once, in revised's order as one place each:
// [e4:13-14], or an artifact's path:line. The revision is asked with no
// tool, so it read nothing that gave it those lines; one naming only
// lines the conversation was shown numbered (shown, by shownLines) cites
// what it saw, and is left to the checks every citation has.
func revisionAdded(answer, revised string, corrected []string, shown map[string][]carriedRead) []string {
	had := Cites(answer)
	for _, c := range corrected {
		had = append(had, Cites(c)...)
	}
	var out []string
	for _, c := range Cites(revised) {
		if c.From == 0 || slices.ContainsFunc(had, func(h Cite) bool {
			return h.Name == c.Name && h.Path == c.Path && h.From == c.From && max(h.To, h.From) == max(c.To, c.From)
		}) {
			continue
		}
		if c.Path == "" && spans(shown[c.Name], c.From, max(c.To, c.From)) {
			continue
		}
		label := citeLabel(c)
		if c.Path == "" {
			label = "[" + label + "]"
		}
		if !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

var (
	// shownNumbered is a line under its stored number, as an excerpt and
	// evidence_read give it; shownMatch one as evidence_grep does, a match
	// or its context.
	shownNumbered = regexp.MustCompile(`^ *([0-9]+)  `)
	shownMatch    = regexp.MustCompile(`^([0-9]+)[:-] `)
	// readHead and grepHead name the item an evidence_read result, or a
	// part of an evidence_grep one, shows lines of.
	readHead = regexp.MustCompile(`^(e[1-9][0-9]*) \([^)]*\): lines [0-9]+–[0-9]+`)
	grepHead = regexp.MustCompile(`^== (e[1-9][0-9]*) \([^)]*\): `)
)

// shownLines are the lines of each item, by short name, that msgs, the
// messages of the conversation conv, showed it under their stored
// numbers, as the record of what each engine-authored message gives holds
// them (companion.Store.Shows): a tool's result — an item's excerpt, the
// lines an evidence_read gave, the matches an evidence_grep gave with
// their context — the lines a restart carries and those a verdict quotes;
// a whole item, From 0, every line. The case block and case updates give
// items unnumbered, so they show none. A conversation stored before the
// record has them read from its tool results' text (resultLines).
func shownLines(ctx context.Context, st companion.Store, conv string, msgs []companion.Message) (map[string][]carriedRead, error) {
	c, err := st.Conversation(ctx, conv)
	if err != nil {
		return nil, err
	}
	if !c.Exposure {
		return resultLines(msgs), nil
	}
	shows, err := st.Shows(ctx, conv)
	if err != nil {
		return nil, err
	}
	out := map[string][]carriedRead{}
	for _, m := range msgs {
		if m.Kind == companion.KindCase || m.Kind == companion.KindUpdate {
			continue
		}
		for _, sh := range shows[m.Seq] {
			out[sh.Name] = append(out[sh.Name], carriedRead{from: sh.From, to: sh.To})
		}
	}
	return out, nil
}

// resultLines are the lines of each item, by short name, msgs' tool
// results showed under their stored numbers, read from their text: an
// item's excerpt as a tool returned it, the lines an evidence_read gave,
// and the matches an evidence_grep gave with their context.
func resultLines(msgs []companion.Message) map[string][]carriedRead {
	out := map[string][]carriedRead{}
	add := func(name, n string) {
		i, err := strconv.Atoi(n)
		if err != nil || name == "" {
			return
		}
		out[name] = append(out[name], carriedRead{from: i, to: i})
	}
	for _, m := range msgs {
		for _, r := range m.Results {
			if r.IsError {
				continue
			}
			head, body, _ := strings.Cut(r.Text, "\n")
			lines := strings.Split(body, "\n")
			switch {
			case len(ExcerptEvidence(r.Text)) == 1:
				name := ExcerptEvidence(r.Text)[0]
				for _, l := range lines {
					if mm := shownNumbered.FindStringSubmatch(l); mm != nil {
						add(name, mm[1])
					}
				}
			case readHead.MatchString(head):
				name := readHead.FindStringSubmatch(head)[1]
				for _, l := range lines {
					if mm := shownNumbered.FindStringSubmatch(l); mm != nil {
						add(name, mm[1])
					}
				}
			default:
				name := ""
				for _, l := range strings.Split(r.Text, "\n") {
					if mm := grepHead.FindStringSubmatch(l); mm != nil {
						name = mm[1]
					} else if mm := shownMatch.FindStringSubmatch(l); mm != nil {
						add(name, mm[1])
					}
				}
			}
		}
	}
	return out
}

// checkRun is one check as it is kept: the role that made it; the main
// conversation, the call whose answer it checked and the name the chat
// gives what it checked (a7, i3); its own conversation and the call whose
// findings it accepted; how many statements the text had; the findings,
// and what validation dropped.
type checkRun struct {
	role                check.Role
	conv, checked, name string
	checkConv, madeBy   string
	statements          int
	findings            check.Findings
	dropped             []check.Rejected
}

// checkTask is what is a check's own: the task type its calls are
// recorded as, the kind of its conversation, its system prompt, and
// whether a call left after valid findings asks about the statements they
// left unaccounted (followUp).
type checkTask struct {
	task, conversation, system string
	followUp                   bool
}

// editorCheck is the editor's.
var editorCheck = checkTask{task: EditorTask, conversation: editorConversation, system: check.EditorPrompt(), followUp: true}

// runEditor has the editor's model check answer, given earlier for the
// dropped qualifiers (runCheck).
func (s *Session) runEditor(ctx context.Context, pc config.ProfileChoice, answer answerAt, earlier []answerAt, run *checkRun, on func(TalkEvent)) error {
	req, err := s.editorRequest(ctx, answer, earlier)
	if err != nil {
		return err
	}
	return s.runCheck(ctx, editorCheck, pc, req, run, on)
}

// runCheck has a check's model answer req, in a conversation of its own
// through the companion's loop — the case's gate, the egress policy and
// the records of the check's task — and fills run with what it accepted.
// The check makes at most checkBudget model calls in all, whatever the
// model answers: findings refused whole are said to the model, which may
// call once more, and one answered in text is asked once to call the
// tool, while calls are left. Where the task follows up and a call is
// left after valid findings that leave statements unaccounted, it asks
// about those (followUp).
func (s *Session) runCheck(ctx context.Context, t checkTask, pc config.ProfileChoice, req CheckRequest, run *checkRun, on func(TalkEvent)) error {
	e, c := s.e, s.c
	run.statements = len(req.Context.Statements)
	p, err := e.modelProvider(pc.Provider)
	if err != nil {
		return err
	}
	d := p.Describe()
	tool := &checkTool{vc: req.Context}
	l := &companion.Loop{
		Store:    companion.Store{DB: c.st.DB()},
		Provider: p,
		Model:    pc.Model,
		Effort:   pc.Effort,
		System:   t.system,
		Tools:    []companion.Tool{tool},
		Gate:     e.companionGate(c, d.EgressClass),
		Policy:   redact.For(d.EgressClass),
		Record: func(inv companion.Invocation) error {
			return recordCall(c, t.task, pc, d, inv)
		},
		Budget:   checkBudget,
		MaxCalls: checkBudget,
		Now:      c.now,
		NewID:    c.newID,
	}
	// The chat is told of the check's calls, for the usage, and of its
	// warnings; its text and its tool are its own.
	calls := func(ev TalkEvent) {
		if ev.Kind == TalkCall || ev.Kind == TalkWarning {
			on(ev)
		}
	}
	reply, err := l.Task(ctx, t.conversation, req.Text, calls, quotedShows(req.Context.Quoted)...)
	run.checkConv = reply.Conversation
	if err == nil && !tool.called() {
		_, err = l.Turn(ctx, reply.Conversation, companion.KindEngine, checkRetry, calls)
	}
	f, callID, dropped, why := tool.result()
	if f == nil {
		switch {
		case err != nil && !errors.Is(err, companion.ErrCallsSpent):
			return err
		case why == "":
			why = "the model answered without calling " + check.ToolName
		}
		if err != nil {
			why += "; its " + strconv.Itoa(checkBudget) + " calls are spent"
		}
		return errors.New(why)
	}
	madeBy, err := callInvocation(ctx, l.Store, reply.Conversation, callID)
	if err != nil {
		return err
	}
	run.findings, run.dropped, run.madeBy = *f, dropped, madeBy
	if t.followUp && len(f.Unaccounted) > 0 && l.CallsLeft() > 0 {
		followUp(ctx, l, reply.Conversation, req, run, calls)
	}
	return nil
}

// quotedShows are the lines a check's request quotes, by item name, as the
// record of what its conversation was shown: a checker's own, which never
// shows the conversation it checks anything.
func quotedShows(quoted map[string][]check.Lines) []companion.Shown {
	var out []companion.Shown
	for _, name := range slices.Sorted(maps.Keys(quoted)) {
		for _, l := range quoted[name] {
			out = append(out, companion.Shown{Name: name, From: l.From, To: l.To})
		}
	}
	return out
}

// followUpElement names, among a check's dropped elements, why its
// follow-up was not taken, and prefixes the follow-up's own.
const followUpElement = "follow-up"

// followUp asks the check's model once more, in its conversation, about
// the statements its findings left unaccounted, naming only those, and
// validates the answer against a request holding only them (the same
// items quoted, the same role); what it accepts joins run
// (mergeFollowUp). Where the call fails, is refused, answers in text or
// passes the check's bound, run stands as it was, why kept as a dropped
// element, followUpElement.
func followUp(ctx context.Context, l *companion.Loop, conv string, req CheckRequest, run *checkRun, calls func(TalkEvent)) {
	vc := req.Context
	vc.Statements = nil
	var asked []string
	for _, st := range req.Context.Statements {
		if slices.Contains(run.findings.Unaccounted, st.ID) {
			vc.Statements = append(vc.Statements, st)
			asked = append(asked, st.ID)
		}
	}
	// One call is left: findings refused end the follow-up.
	tool := &checkTool{vc: vc, calls: checkBudget - 1}
	l.Tools = []companion.Tool{tool}
	_, err := l.Turn(ctx, conv, companion.KindEngine, followUpAsk(vc.Statements), calls)
	f, _, dropped, why := tool.result()
	if f != nil {
		mergeFollowUp(run, req.Context.Answer, asked, *f, dropped)
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		why = "it passed the editor's " + editorBound.String() + " bound"
	case why != "":
	case err != nil:
		why = err.Error()
	default:
		why = "the model answered without calling " + check.ToolName
	}
	run.dropped = append(run.dropped, check.Rejected{Element: followUpElement, Reason: why})
}

// followUpHeading begins the follow-up ask: how the trajectory tells a
// follow-up call from a retry.
const followUpHeading = "# Statements not accounted for"

// followUpAsk is the engine's message asking about the statements asked,
// by ID and as the request gave them, and only those.
func followUpAsk(asked []check.Statement) string {
	var b strings.Builder
	b.WriteString(followUpHeading + "\n\nYour findings said nothing of these statements:\n\n")
	for _, st := range asked {
		b.WriteString(st.ID + ": " + st.Text + "\n")
	}
	b.WriteString("\nThe other statements are settled. Call " + check.ToolName + " once for exactly these, each with a finding, as supported or as skipped, against the items quoted above; " +
		"where you correct one, write `corrected` as the whole answer as written with only these statements corrected.")
	return b.String()
}

// mergeFollowUp joins to run's findings the follow-up's, f, validated
// against the statements asked: its findings, supported and skipped on a
// statement the first call left unaccounted, the first call's account
// kept where both gave one; its corrected text, made of the answer as
// written, spliced statement by statement into the first call's corrected
// text, or into answer where the first corrected nothing (check.Splice) —
// where that fails, its correction is dropped with why and its findings
// are shown flagged. Its dropped elements are kept as the follow-up's.
func mergeFollowUp(run *checkRun, answer string, asked []string, f check.Findings, dropped []check.Rejected) {
	m := &run.findings
	open := func(id string) bool { return slices.Contains(asked, id) && slices.Contains(m.Unaccounted, id) }
	for _, fi := range f.Findings {
		if open(fi.Statement) {
			m.Findings = append(m.Findings, fi)
		}
	}
	for _, id := range f.Supported {
		if open(id) {
			m.Supported = append(m.Supported, id)
		}
	}
	for _, sk := range f.Skipped {
		if open(sk.Statement) {
			m.Skipped = append(m.Skipped, sk)
		}
	}
	m.Unaccounted = f.Unaccounted
	m.Notes = append(m.Notes, f.Notes...)
	if f.Corrected != "" {
		base := m.Corrected
		if base == "" {
			base = answer
		}
		spliced, err := check.Splice(answer, base, f.Corrected, asked)
		if err != nil {
			dropped = append(dropped, check.Rejected{Element: "corrected", Reason: err.Error() + "; its findings are shown flagged"})
		} else if spliced != answer {
			m.Corrected = spliced
		}
	}
	for _, d := range dropped {
		run.dropped = append(run.dropped, check.Rejected{Element: followUpElement + "." + d.Element, Reason: d.Reason})
	}
}

// keepCheck keeps a check as it ended, in the checks table, within a
// bound of its own from now (finalContext), whatever ctx's cancelling or
// bound; a failure to keep it is said as a warning, the answer or the
// review shown all the same.
func (s *Session) keepCheck(ctx context.Context, run checkRun, outcome, why string, on func(TalkEvent)) {
	ctx, cancel := finalContext(ctx)
	defer cancel()
	findings, err := json.Marshal(run.findings)
	if err == nil {
		var dropped []byte
		if dropped, err = json.Marshal(run.dropped); err == nil {
			_, err = s.c.st.DB().ExecContext(ctx, `INSERT INTO checks (id, investigation_id, role, conversation_id, checked_invocation_id, checked_name, statements, check_conversation_id, invocation_id, schema_id, findings, dropped, outcome, error, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				s.c.newID("check"), s.c.id, string(run.role), run.conv, run.checked, run.name, run.statements, run.checkConv, run.madeBy, string(apischema.CheckFindings),
				string(findings), string(dropped), outcome, why, s.c.now().UTC().Format(time.RFC3339Nano))
		}
	}
	if err != nil {
		on(TalkEvent{Kind: TalkWarning, Conversation: run.conv, Text: "the " + string(run.role) + "'s check was not kept: " + err.Error()})
	}
}

// keptAnswerCheck is an editor's check as kept: how it ended, how many
// statements it was given, its findings and what validation dropped.
type keptAnswerCheck struct {
	outcome, why string
	statements   int
	findings     check.Findings
	dropped      []check.Rejected
}

// of is what the check made of answer, the text it checked.
func (k keptAnswerCheck) of(answer string) AnswerCheck {
	return answerCheckOf(k.outcome, k.why, answer, k.statements, k.findings, k.dropped)
}

// answerChecks are the editor's checks of the case's answers, by the call
// that wrote each answer, as kept; the chat shows each beneath its answer.
func answerChecks(ctx context.Context, c *openCase) (map[string]keptAnswerCheck, error) {
	rows, err := c.st.DB().QueryContext(ctx, `SELECT checked_invocation_id, statements, findings, dropped, outcome, error FROM checks
		WHERE investigation_id = ? AND role = ? ORDER BY created_at, rowid`, c.id, string(check.Editor))
	if err != nil {
		return nil, fmt.Errorf("the editor's checks: %w", err)
	}
	defer rows.Close()
	out := map[string]keptAnswerCheck{}
	for rows.Next() {
		var checked, findings, dropped string
		var k keptAnswerCheck
		if err := rows.Scan(&checked, &k.statements, &findings, &dropped, &k.outcome, &k.why); err != nil {
			return nil, fmt.Errorf("the editor's checks: %w", err)
		}
		if err := json.Unmarshal([]byte(findings), &k.findings); err != nil {
			return nil, fmt.Errorf("the editor's check of %s: %w", checked, err)
		}
		if err := json.Unmarshal([]byte(dropped), &k.dropped); err != nil {
			return nil, fmt.Errorf("the editor's check of %s: %w", checked, err)
		}
		out[checked] = k
	}
	return out, rows.Err()
}

// checkTool is check_findings for one check: it validates the findings
// against what the request gave, keeps the first it accepts and ends the
// run; findings refused whole are said to the model, which may call again
// once. The editor's findings that name wording to correct and correct
// none (uncorrected) are sent back once too, while a call is left, and
// held: they stand where the second call gives nothing better.
type checkTool struct {
	vc check.Context

	mu       sync.Mutex
	calls    int
	accepted *check.Findings
	callID   string
	dropped  []check.Rejected
	why      string
	// held is whether accepted is findings sent back uncorrected, which a
	// later call's valid findings replace.
	held bool
}

// uncorrected says the editor's findings f name a fault its corrected
// text is to fix and give no corrected text: the answer would be shown
// as written, the fault flagged beneath it.
func uncorrected(vc check.Context, f check.Findings) bool {
	return vc.Role == check.Editor && strings.TrimSpace(f.Corrected) == "" &&
		slices.ContainsFunc(f.Findings, func(fi check.Finding) bool { return !fi.Anchor })
}

// findingsRefused begins check_findings's result for findings refused
// whole, the refusal following on its line: how the trajectory reads why
// a check's retry was asked.
const findingsRefused = "The findings were refused and nothing was recorded: "

func (*checkTool) Decl() model.Tool { return check.Decl() }

func (t *checkTool) Run(_ context.Context, c companion.Call) companion.Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.accepted != nil && !t.held {
		return companion.Result{Text: "The findings were already recorded; nothing more is taken.", IsError: true, End: true}
	}
	f, dropped, err := check.Validate(t.vc, c.Args)
	if err != nil {
		t.why = "the findings were refused: " + err.Error()
		return companion.Result{Text: findingsRefused + err.Error() + "\nCall " + check.ToolName + " again with the whole findings corrected.",
			IsError: true, End: t.calls >= checkBudget}
	}
	t.accepted, t.callID, t.dropped = &f, c.ID, dropped
	if t.held = t.calls < checkBudget && uncorrected(t.vc, f); t.held {
		return companion.Result{Text: findingsRefused + "they name wording to correct and give no `corrected` text, so the answer would be shown as written." +
			"\nCall " + check.ToolName + " again with the same findings and `corrected`, the whole answer with each finding that is not an anchor corrected.", IsError: true}
	}
	out := fmt.Sprintf("Recorded: %s, %s supported, %s skipped.", text.Plural(len(f.Findings), "finding"), text.Plural(len(f.Supported), "statement"), text.Plural(len(f.Skipped), "statement"))
	for _, d := range dropped {
		out += "\nDropped " + d.String()
	}
	return companion.Result{Text: out, End: true}
}

func (t *checkTool) called() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls > 0
}

func (t *checkTool) result() (*check.Findings, string, []check.Rejected, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.accepted, t.callID, t.dropped, t.why
}
