package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/lowmagic/compa-engine/internal/companion"
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/redact"
)

// ResendTask is the task a resent call is recorded under, so the case's
// usage counts it apart from the calls of its conversations.
const ResendTask = "calls.resend"

// Resent is a stored call sent again and what came back, beside the
// original as its record and its conversation hold it.
type Resent struct {
	// Of is the original call's record, and OfText the answer its
	// conversation kept, empty where the call kept none.
	Of     invocations.Record
	OfText string
	// Record is the resent call's own record; Text and Calls are what it
	// answered.
	Record invocations.Record
	Text   string
	Calls  []model.ToolCall
}

// Resend sends the request a stored call of the case sent, as it was
// stored, once more: to the same provider, model and effort, or to the
// configured model named, at effort or the original's where that model
// takes it. The call passes the case's gate and the target provider's
// redaction as any call does, and is recorded as a call of its own whose
// record names the original; no conversation is read or written. A call
// sent to another model leaves out the provider data the original's
// conversation carried for its model, such as thought signatures.
func (s *Session) Resend(ctx context.Context, id, modelName, effort string, on func(TalkEvent)) (Resent, error) {
	e, c := s.e, s.c
	db := c.st.DB()
	rec, err := invocations.Get(db, id)
	if errors.Is(err, invocations.ErrNotFound) || err == nil && rec.InvestigationID != c.id {
		return Resent{}, fmt.Errorf("no model call %s in this case", id)
	}
	if err != nil {
		return Resent{}, err
	}
	if len(rec.Request) == 0 {
		return Resent{}, fmt.Errorf("call %s kept no request to send: it ended %s before one was sent", id, rec.Outcome)
	}
	var req model.Request
	if err := json.Unmarshal(rec.Request, &req); err != nil {
		return Resent{}, fmt.Errorf("call %s's request: %w", id, err)
	}
	pc := config.ProfileChoice{Provider: rec.Model.Provider, Model: rec.Model.ModelID, Effort: rec.Model.Effort, Source: "config", Defined: true}
	if rec.ModelProfile != nil {
		pc.Class, pc.Source = rec.ModelProfile.Class, rec.ModelProfile.Source
	}
	switch {
	case modelName != "":
		if pc, err = e.config.WithModel(pc, modelName, effort); err != nil {
			return Resent{}, err
		}
	case effort != "":
		return Resent{}, errors.New("an effort is given with the model it is for")
	}
	if pc.Provider != rec.Model.Provider || pc.Model != rec.Model.ModelID {
		for i := range req.Messages {
			for j := range req.Messages[i].Calls {
				req.Messages[i].Calls[j].Signature = nil
			}
		}
	}
	req.Model, req.Effort = pc.Model, pc.Effort
	p, err := e.modelProvider(pc.Provider)
	if err != nil {
		return Resent{}, err
	}
	d := p.Describe()
	l := &companion.Loop{
		Store:    companion.Store{DB: db},
		Provider: p,
		Gate:     e.companionGate(c, d.EgressClass),
		Policy:   redact.For(d.EgressClass),
		Record: func(inv companion.Invocation) error {
			return recordCall(c, ResendTask, pc, d, inv)
		},
		Now:   c.now,
		NewID: c.newID,
	}
	resp, inv, callErr := l.Resend(ctx, id, req, on)
	out := Resent{Of: rec, OfText: answerOf(ctx, l.Store, rec)}
	if out.Record, err = invocations.Get(db, inv.ID); err != nil {
		return out, errors.Join(callErr, err)
	}
	if resp != nil {
		out.Text = resp.Text
	}
	return out, callErr
}

// answerOf is the text the call's conversation kept as its answer, or
// none where it kept none.
func answerOf(ctx context.Context, st companion.Store, rec invocations.Record) string {
	if rec.ConversationID == "" {
		return ""
	}
	msgs, err := st.Messages(ctx, rec.ConversationID)
	if err != nil {
		return ""
	}
	for _, m := range msgs {
		if m.Invocation == rec.InvocationID {
			return m.Text
		}
	}
	return ""
}

// RenderResent is a resent call as compa case resend prints it: the
// original and the new call side by side; empty where nothing was sent.
func RenderResent(r Resent) string {
	if r.Record.InvocationID == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s, as sent before:\n", callLine(r.Of))
	writeAnswer(&b, r.OfText, r.Of.ToolCalls)
	fmt.Fprintf(&b, "\nresent as %s:\n", callLine(r.Record))
	writeAnswer(&b, r.Text, r.Record.ToolCalls)
	b.WriteString("\nA call sent again is not expected to answer as it did; nothing in the case's conversations changed.\n")
	return b.String()
}

// callLine names a call: its ID, task, model and effort, outcome and
// usage.
func callLine(rec invocations.Record) string {
	return fmt.Sprintf("%s · %s · %s %s (%s) · %s · %s", rec.InvocationID, rec.TaskType, rec.Model.Provider, rec.Model.ModelID,
		rec.Model.EffortShown(), rec.Outcome, invocations.Of(rec).Totals())
}

// writeAnswer prints a call's answer and the tool calls it made, each with
// its arguments.
func writeAnswer(out io.Writer, text string, calls []invocations.ToolCall) {
	if text = strings.TrimSpace(text); text != "" {
		fmt.Fprintln(out, text)
	}
	for _, call := range calls {
		args, _ := json.Marshal(call.Params) // decoded from JSON, so it encodes again
		fmt.Fprintf(out, "→ %s %s\n", call.Test, args)
	}
	if text == "" && len(calls) == 0 {
		fmt.Fprintln(out, "(no answer kept)")
	}
}
