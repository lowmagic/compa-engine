package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/openai"
)

// endpoint is a synthetic Chat Completions server answering every request
// with the event stream events, each a chunk's JSON, after keeping the
// request; it counts the requests it is sent.
type endpoint struct {
	srv    *httptest.Server
	body   map[string]any
	header http.Header
	calls  atomic.Int64
}

func serve(t *testing.T, status int, events ...string) *endpoint {
	t.Helper()
	e := &endpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		e.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &e.body); err != nil {
			t.Errorf("the request body: %v", err)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"message":"synthetic refusal"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\n")
		for _, ev := range events {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func provider(t *testing.T, e *endpoint, marks bool) *openai.Provider {
	t.Helper()
	p, err := openai.New(openai.Options{BaseURL: e.srv.URL + "/v1/", APIKey: "synthetic-key", EgressClass: "internal_network", CacheMarks: marks})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var twoTools = []model.Tool{{Name: "grep", Description: "Search.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}}}}

// TestStreamedAnswer: [C2] text and reasoning stream as they arrive; a tool
// call's arguments, sent in pieces, are put together by its index; the
// usage is made disjoint — input the prompt less its cache reads and
// writes, output the completion less its reasoning — and the cost the
// endpoint reports is kept with its currency. The request carries the
// conversation as the standard reads it: the system prompt, a model
// message's calls with their arguments as JSON, each result as a tool
// message before the engineer's text, the effort, no tool choice but where
// NoCalls asks, and the key as a bearer token.
func TestStreamedAnswer(t *testing.T) {
	e := serve(t, http.StatusOK,
		`{"model":"vendor/model-a","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"Thinking."}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Searching "}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"now."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c9","type":"function","function":{"name":"grep","arguments":"{\"pattern\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"NotReady\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0042,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":5}}}`,
	)
	p := provider(t, e, false)
	var streamed, thought strings.Builder
	r := model.Request{Model: "vendor/model-a", Effort: "low", System: "The method.", Tools: twoTools, MaxOutputTokens: 512, Messages: []model.Message{
		{Role: model.RoleUser, Text: "The case.", Break: true},
		{Role: model.RoleModel, Text: "Reading.", Calls: []model.ToolCall{{ID: "c1", Name: "grep", Args: map[string]any{"pattern": "x"}}}},
		{Role: model.RoleUser, Results: []model.ToolResult{{CallID: "c1", Name: "grep", Text: "e1: a line"}}, Text: "Go on."},
	}}
	out, err := p.Call(context.Background(), r, func(d model.Delta) { streamed.WriteString(d.Text); thought.WriteString(d.Thinking) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Searching now." || streamed.String() != out.Text || out.Thinking != "Thinking." || thought.String() != "Thinking." {
		t.Errorf("text %q streamed %q, thinking %q streamed %q", out.Text, streamed.String(), out.Thinking, thought.String())
	}
	if d := cmp.Diff([]model.ToolCall{{ID: "c9", Name: "grep", Args: map[string]any{"pattern": "NotReady"}}}, out.Calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
	want := model.Usage{
		InputTokens: model.Reported(10), CacheReadTokens: model.Reported(60), CacheWriteTokens: model.Reported(30),
		OutputTokens: model.Reported(15), ReasoningTokens: model.Reported(5), Normalization: model.NormalizationDerived,
	}
	if d := cmp.Diff(want, out.Usage); d != "" {
		t.Errorf("usage (-want +got):\n%s", d)
	}
	if out.Cost == nil || out.Cost.Amount != 0.0042 || out.Cost.Currency != "USD" || out.ModelID != "vendor/model-a" {
		t.Errorf("cost %+v, model %q", out.Cost, out.ModelID)
	}
	if got := e.header.Get("Authorization"); got != "Bearer synthetic-key" {
		t.Errorf("Authorization %q", got)
	}
	b := e.body
	if b["model"] != "vendor/model-a" || b["reasoning_effort"] != "low" || b["stream"] != true || b["max_tokens"] != float64(512) || b["tool_choice"] != nil {
		t.Errorf("request fields: %v", b)
	}
	msgs, _ := json.Marshal(b["messages"])
	wantMsgs := `[{"content":"The method.","role":"system"},{"content":"The case.","role":"user"},` +
		`{"content":"Reading.","role":"assistant","tool_calls":[{"function":{"arguments":"{\"pattern\":\"x\"}","name":"grep"},"id":"c1","type":"function"}]},` +
		`{"content":"e1: a line","role":"tool","tool_call_id":"c1"},{"content":"Go on.","role":"user"}]`
	if string(msgs) != wantMsgs {
		t.Errorf("messages:\n%s\nwant\n%s", msgs, wantMsgs)
	}

	r.NoCalls = true
	if _, err := p.Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if e.body["tool_choice"] != "none" {
		t.Errorf("NoCalls sent tool_choice %v", e.body["tool_choice"])
	}
}

// TestUsageUnreported: [C2] an endpoint that reports no prompt details
// leaves input and the cache categories unavailable, never zero, and its
// normalization ambiguous; one that reports no reasoning leaves it
// unavailable, output as reported; one that reports no usage, everything.
func TestUsageUnreported(t *testing.T) {
	for _, c := range []struct {
		name, usage string
		want        model.Usage
	}{{
		"no details", `,"usage":{"prompt_tokens":100,"completion_tokens":20}`,
		model.Usage{OutputTokens: model.Reported(20), Normalization: model.NormalizationAmbiguous, Ambiguous: []string{"input_tokens", "cache_read_tokens", "cache_write_tokens"}},
	}, {
		"reads, no writes", `,"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}`,
		model.Usage{InputTokens: model.Reported(60), CacheReadTokens: model.Reported(40), OutputTokens: model.Reported(20), Normalization: model.NormalizationDerived},
	}, {
		"none", "",
		model.Usage{Normalization: model.NormalizationAmbiguous, Ambiguous: []string{"input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens"}},
	}} {
		t.Run(c.name, func(t *testing.T) {
			e := serve(t, http.StatusOK, `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]`+c.usage+`}`)
			out, err := provider(t, e, false).Call(context.Background(), model.Request{Model: "m"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d := cmp.Diff(c.want, out.Usage); d != "" {
				t.Errorf("usage (-want +got):\n%s", d)
			}
			if out.Cost != nil {
				t.Errorf("a cost nobody reported: %+v", out.Cost)
			}
		})
	}
}

// TestStopReasons: [C2] an answer cut at its output budget is
// ErrOutputLimit with what was written, one stopped otherwise or ended
// with no reason ErrStopped, and an endpoint's refusal says its status
// and message; no request is tried again.
func TestStopReasons(t *testing.T) {
	for _, c := range []struct {
		finish string
		want   error
	}{{"length", model.ErrOutputLimit}, {"content_filter", model.ErrStopped}, {"", model.ErrStopped}} {
		e := serve(t, http.StatusOK, `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":"`+c.finish+`"}]}`)
		out, err := provider(t, e, false).Call(context.Background(), model.Request{Model: "m"}, nil)
		if !errors.Is(err, c.want) || out == nil || out.Text != "partial" {
			t.Errorf("finish %q: %v, %+v; want %v with the text", c.finish, err, out, c.want)
		}
	}
	e := serve(t, http.StatusTooManyRequests)
	_, err := provider(t, e, false).Call(context.Background(), model.Request{Model: "m"}, nil)
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "synthetic refusal") {
		t.Errorf("a refused request: %v", err)
	}
	if n := e.calls.Load(); n != 1 {
		t.Errorf("%d requests, want one: nothing is retried", n)
	}
}

// TestCacheMarks: with cache marks, the system prompt and the content
// ending each of the conversation's last three breaks carry
// cache_control, as OpenRouter passes it on; without, content is plain
// text and no mark is sent.
func TestCacheMarks(t *testing.T) {
	r := model.Request{Model: "m", System: "S", Messages: []model.Message{
		{Role: model.RoleUser, Text: "u1", Break: true},
		{Role: model.RoleUser, Text: "u2", Break: true},
		{Role: model.RoleModel, Calls: []model.ToolCall{{ID: "c1", Name: "grep"}}},
		{Role: model.RoleUser, Results: []model.ToolResult{{CallID: "c1", Text: "r1"}}, Break: true},
		{Role: model.RoleUser, Text: "u3", Break: true},
	}}
	finish := `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`
	e := serve(t, http.StatusOK, finish)
	if _, err := provider(t, e, true).Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(e.body["messages"])
	if err != nil {
		t.Fatal(err)
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &msgs); err != nil {
		t.Fatal(err)
	}
	var marked []string
	for _, m := range msgs {
		var parts []struct {
			Text         string         `json:"text"`
			CacheControl map[string]any `json:"cache_control"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue // plain text content, which carries no mark
		}
		for _, p := range parts {
			if p.CacheControl != nil {
				marked = append(marked, p.Text)
			}
		}
	}
	if d := cmp.Diff([]string{"S", "u2", "r1", "u3"}, marked); d != "" {
		t.Errorf("marked content (-want +got):\n%s", d)
	}
	e = serve(t, http.StatusOK, finish)
	if _, err := provider(t, e, false).Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(e.body); strings.Contains(string(b), "cache_control") {
		t.Errorf("a mark sent without cache marks: %s", b)
	}
}

// TestDescribe: the provider declares the egress class the deployment
// gave its endpoint, and refuses one it does not know.
func TestDescribe(t *testing.T) {
	if d := openai.Describe("internal_network"); d.ID != openai.ID || d.EgressClass != "internal_network" {
		t.Errorf("Describe: %+v", d)
	}
	if _, err := openai.New(openai.Options{BaseURL: "http://192.0.2.1/v1", EgressClass: "anywhere"}); err == nil {
		t.Error("an unknown egress class was taken")
	}
}
