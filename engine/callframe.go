package engine

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/lowmagic/compa-engine/internal/check"
	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/model"
)

// A call's frame is what the model says of a tool call beside its
// arguments (docs/MODEL_API.md §Tools): the question a call that returns
// evidence is meant to settle, and, on any call, the answers: what items
// earlier calls returned settled, each in one line. The engine takes them
// out of the arguments before the call is validated and run, so no
// runner, catalog tool or argument rule sees them, and the stored call
// keeps them as the model wrote them. A call without a question runs: a
// missing question is counted, never refused, as a refusal would cost a
// model call. An answer is the model's reading, never evidence, checked
// as a statement is (CallAnswers).
const (
	questionArg = "question"
	answersArg  = "answers"
	// questionMax bounds a question's length, and an answer's, in their
	// declarations.
	questionMax = 300
)

// TakesQuestion says whether a tool of the companion's table takes a
// question: every one that returns evidence, so all but load_expert,
// describe_tool and consult, whose own question is the consult's.
func TakesQuestion(name string) bool {
	return name != "load_expert" && name != "describe_tool" && name != consultTool
}

// framed is a tool of the companion's table with its call's frame
// declared and taken out of the arguments before it runs.
type framed struct {
	inner companion.Tool
	decl  model.Tool
}

func (t framed) Decl() model.Tool { return t.decl }

func (t framed) Run(ctx context.Context, c companion.Call) companion.Result {
	q := QuestionOf(c.Name, c.Args)
	c.Args = ranArgs(c.Name, c.Args)
	r := t.inner.Run(ctx, c)
	if q != "" && !r.IsError {
		r.Text = askedAgain(r.Text, q)
	}
	return r
}

// askedAgain is an excerpt with its call's question put back under its
// head, with the ask to say what the item settled: the model gave the
// optional answers in 1 live run of 3, so
// the ask comes where the result does. One line on every excerpt, so
// short; the head stays the first line, so ExcerptEvidence reads it, and
// the line is no numbered line, so a reading of the lines shown from the
// text skips it. A result that is no excerpt is left as it is.
func askedAgain(result, q string) string {
	name := ExcerptEvidence(result)
	if len(name) != 1 {
		return result
	}
	head, rest, _ := strings.Cut(result, "\n")
	return head + "\nAsked: " + q + " Put what " + name[0] + " settled in your next call's answers, or cite it if you answer now.\n" + rest
}

// answersDecl is the answers' declaration, on every tool of the table.
var answersDecl = map[string]any{"type": "array",
	"description": "Optional: what results of earlier calls settled, each by its item's name and in one line, a search that found nothing with how far it read.",
	"items": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"item", "answer"},
		"properties": map[string]any{
			"item":   map[string]any{"type": "string", "pattern": "^e[1-9][0-9]*$", "description": "The item's name, such as e12."},
			"answer": map[string]any{"type": "string", "maxLength": questionMax, "description": "What it settled, in one line."},
		}}}

// frame declares a tool's answers, and its question, required, where it
// takes one.
func frame(tl companion.Tool) companion.Tool {
	d := tl.Decl()
	params := maps.Clone(d.Parameters)
	props, _ := params["properties"].(map[string]any)
	props = maps.Clone(props)
	if props == nil {
		props = map[string]any{}
	}
	if TakesQuestion(d.Name) {
		props[questionArg] = map[string]any{"type": "string", "maxLength": questionMax,
			"description": "One line ending in ?, whose answer would change what you do or conclude next: not what the call does (\"Read df.txt\") but what it should settle (\"Is /var full on worker-1?\")."}
		required, _ := params["required"].([]any)
		params["required"] = append(slices.Clone(required), questionArg)
	}
	props[answersArg] = answersDecl
	params["properties"] = props
	d.Parameters = params
	return framed{inner: tl, decl: d}
}

// ranArgs are a call's arguments as its tool runs them: without its
// answers, nor its question where its tool takes one. args is not
// changed, as the stored call and the chat's view of it share it.
func ranArgs(name string, args map[string]any) map[string]any {
	_, asked := args[questionArg]
	asked = asked && TakesQuestion(name)
	if _, answered := args[answersArg]; !asked && !answered {
		return args
	}
	out := maps.Clone(args)
	if asked {
		delete(out, questionArg)
	}
	delete(out, answersArg)
	return out
}

// QuestionOf is the question of a call to name, as the model wrote it
// with its spacing collapsed to one line; empty where its tool takes none
// or the call gave none, or gave something other than text.
func QuestionOf(name string, args map[string]any) string {
	if !TakesQuestion(name) {
		return ""
	}
	q, _ := args[questionArg].(string)
	return strings.Join(strings.Fields(q), " ")
}

// questionKey is a question normalised to compare with another: its words
// lowercased, punctuation and spacing aside.
func questionKey(q string) string {
	return strings.Join(questionTerms(q), " ")
}

// questionTerms are a question's words, lowercased: runs of letters and
// digits.
func questionTerms(q string) []string {
	return strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// vagueWords is the fewest words a question names something in.
const vagueWords = 4

// genericWords are the words that name nothing of a case: English's
// function words and the investigation's generic nouns and verbs. A
// question none of whose words is outside them names nothing specific.
var genericWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the this that these those it its there here what which who whom whose when where why how
		is are was were be been being do does did done has have had can could should would will may might must
		and or not no nor but if of in on at to from for by with about into over under any all some each every other another
		more most much many anything something everything nothing else
		i we you me us my our your they them their
		file files log logs line lines error errors warning warnings issue issues problem problems output result results
		data info information content contents detail details status state thing things entry entries message messages
		check checks see look find show shows say says tell tells read contain contains happen happened wrong going on`) {
		m[w] = true
	}
	return m
}()

// vagueQuestion says whether a question is too short to name anything,
// under vagueWords words, or names nothing specific, every word of it a
// generic word (genericWords).
func vagueQuestion(q string) bool {
	words := questionTerms(q)
	if len(words) < vagueWords {
		return true
	}
	return !slices.ContainsFunc(words, func(w string) bool { return !genericWords[w] })
}

// actionVerbs open a description of a call rather than a question: an
// imperative such as "Read uptime output" or "Find ethtool outputs for
// ens2f0", which most questions in live use were.
var actionVerbs = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`find read check search list grep diff compare identify look get show
		verify confirm inspect examine review extract count locate fetch scan view open run query retrieve determine investigate`) {
		m[w] = true
	}
	return m
}()

// restatesAction says whether a question describes its call instead of
// asking: it has no question mark, or opens with an imperative verb of
// actionVerbs.
func restatesAction(q string) bool {
	if !strings.Contains(q, "?") {
		return true
	}
	words := questionTerms(q)
	return len(words) > 0 && actionVerbs[words[0]]
}

// ItemAnswer is one answer a call gave: the call's tool, the item it
// names and what it says the item settled, one line, as the model wrote
// them; and what checking it as a statement flagged (CallAnswers), the
// call still run.
type ItemAnswer struct {
	Tool   string   `json:"tool"`
	Item   string   `json:"item"`
	Answer string   `json:"answer"`
	Flags  []string `json:"flags,omitempty"`
}

// callAnswersOf are the answers of a call to name, as written, each one line;
// an element that is not an item and its answer, and answers that are no
// list, are flagged so.
func callAnswersOf(name string, args map[string]any) []ItemAnswer {
	v, ok := args[answersArg]
	if !ok || v == nil {
		return nil
	}
	l, ok := v.([]any)
	if !ok {
		return []ItemAnswer{{Tool: name, Flags: []string{"answers is not a list of an item and its answer"}}}
	}
	out := make([]ItemAnswer, 0, len(l))
	for _, e := range l {
		m, _ := e.(map[string]any)
		item, ok1 := m["item"].(string)
		answer, ok2 := m["answer"].(string)
		a := ItemAnswer{Tool: name, Item: strings.TrimSpace(item), Answer: strings.Join(strings.Fields(answer), " ")}
		if !ok1 || !ok2 {
			a.Flags = append(a.Flags, "not an item and its answer")
		}
		out = append(out, a)
	}
	return out
}

// returnedItems are the items a conversation's tools have returned so
// far, by short name: each a result's excerpt names, and each an
// evidence_read or evidence_grep read, a case comment among them.
type returnedItems map[string]bool

// evidenceGrepTool is evidence.grep as the companion offers it.
var evidenceGrepTool = strings.ReplaceAll(toolGrep, ".", "_")

// add counts the items results returned, asked the calls they answer by
// ID; short gives an item's short name from its name or ID.
func (r returnedItems) add(results []model.ToolResult, asked map[string]model.ToolCall, short func(string) string) {
	for _, res := range results {
		if res.IsError {
			continue
		}
		for _, n := range ExcerptEvidence(res.Text) {
			r[n] = true
		}
		var named string
		switch res.Name {
		case evidenceReadTool:
			named = strParam(asked[res.CallID].Args, "id")
		case evidenceGrepTool:
			named = strParam(asked[res.CallID].Args, "ids")
		}
		for _, n := range strings.Fields(named) {
			r[short(n)] = true
		}
	}
}

// itemFlag is why an answer names no item returned before it, or "".
func (r returnedItems) itemFlag(a ItemAnswer) string {
	switch {
	case len(a.Flags) > 0:
		return ""
	case !shortName.MatchString(a.Item):
		return strconv.Quote(a.Item) + " is not an item's name, such as e12"
	case !r[a.Item]:
		return a.Item + " was returned by no tool of this conversation before this call"
	}
	return ""
}

// checkAnswerSpans checks the inline code spans of answers whose item was
// returned before them as a statement's are: a command line is no claim;
// arithmetic is evaluated, flagged where it does not hold; any other span
// is searched for in the item's lines the conversation was shown, as the
// exposure record holds them, and flagged where none holds it.
func (s *Session) checkAnswerSpans(ctx context.Context, x exposure, answers []*ItemAnswer) error {
	for _, a := range answers {
		if len(a.Flags) > 0 {
			continue
		}
		var spans []string
		for _, sp := range check.Spans(a.Answer) {
			if check.Commands(s.e.config.Checks.Commands).Command(sp) || slices.Contains(spans, sp) {
				continue
			}
			if holds, ok := arithmetic(sp); ok {
				if !holds {
					a.Flags = append(a.Flags, "`"+sp+"`: "+arithmeticWrong)
				}
				continue
			}
			spans = append(spans, sp)
		}
		if len(spans) == 0 {
			continue
		}
		item := exposure{shown: map[string]bool{}, lines: map[string][]carriedRead{a.Item: x.lines[a.Item]}, named: map[string]bool{a.Item: x.named[a.Item]}}
		item.shown[a.Item] = x.shown[a.Item]
		found, err := s.unseenSpans(ctx, spans, nil, item, spanBudget)
		if err != nil {
			return err
		}
		for _, sp := range spans {
			if found.unseen[sp] {
				a.Flags = append(a.Flags, "`"+sp+"` is in none of "+a.Item+"'s lines this conversation was shown")
			}
		}
	}
	return nil
}

// CallAnswers are the answers call id of conversation conv gave, the main
// conversation, one it was compacted from or a consult, each checked as a
// statement (docs/MODEL_API.md §Tools): flagged where it names no item a
// tool of the conversation returned before the call, or a code span of it
// is in none of that item's lines the conversation was shown. It writes
// nothing and calls no model; a call not found has none.
func (s *Session) CallAnswers(ctx context.Context, conv, id string) ([]ItemAnswer, error) {
	history, err := s.ConversationHistory(ctx)
	if err != nil {
		return nil, err
	}
	var msgs []companion.Message
	for _, m := range history {
		msgs = append(msgs, m.Message)
	}
	if !slices.ContainsFunc(history, func(m ConversationMessage) bool { return m.ConversationID == conv }) {
		if msgs, err = (companion.Store{DB: s.c.st.DB()}).Messages(ctx, conv); err != nil {
			return nil, err
		}
	}
	short := func(n string) string {
		if !IsEvidenceID(n) {
			return n
		}
		if name, err := evidenceName(s.c, n); err == nil {
			return name
		}
		return n
	}
	returned := returnedItems{}
	asked := map[string]model.ToolCall{}
	for _, m := range msgs {
		switch m.Kind {
		case companion.KindModel:
			asked = map[string]model.ToolCall{}
			for _, c := range m.Calls {
				asked[c.ID] = c
			}
			c, ok := asked[id]
			if !ok {
				continue
			}
			answers := callAnswersOf(c.Name, c.Args)
			var checked []*ItemAnswer
			for i := range answers {
				a := &answers[i]
				if f := returned.itemFlag(*a); f != "" {
					a.Flags = append(a.Flags, f)
				}
				checked = append(checked, a)
			}
			x, err := s.exposure(ctx)
			if err != nil {
				return nil, err
			}
			return answers, s.checkAnswerSpans(ctx, x, checked)
		case companion.KindResults:
			returned.add(m.Results, asked, short)
		}
	}
	return nil, nil
}

// HasAnswers says whether a call's arguments give answers.
func HasAnswers(args map[string]any) bool {
	_, ok := args[answersArg]
	return ok
}
