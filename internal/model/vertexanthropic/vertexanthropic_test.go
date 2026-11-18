package vertexanthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/vertexanthropic"
)

// server records each request and answers with events as server-sent
// events, or with status.
type server struct {
	mu     sync.Mutex
	bodies []map[string]any
	paths  []string
	status int
	events []string
}

func newProvider(t *testing.T, s *server) *vertexanthropic.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m) // a body that is not JSON is recorded as nil
		s.mu.Lock()
		defer s.mu.Unlock()
		s.bodies = append(s.bodies, m)
		s.paths = append(s.paths, r.URL.Path)
		if s.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(s.status)
			fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range s.events {
			var head struct{ Type string }
			_ = json.Unmarshal([]byte(e), &head) // the fixtures are valid JSON
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
		}
	}))
	t.Cleanup(srv.Close)
	creds := &google.Credentials{ProjectID: "proj", TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token"})}
	p, err := vertexanthropic.New(context.Background(), vertexanthropic.Options{Project: "proj", Location: "global", Credentials: creds, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// answer is a streamed message: text in two deltas, a tool call whose
// input arrives in two pieces, then the stop reason and usage.
func answer(stop string) []string {
	return []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"usage":{"input_tokens":40,"cache_creation_input_tokens":5,"cache_read_input_tokens":3000,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The router "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"is pending."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"grep","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pattern\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Pending\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"` + stop + `","stop_sequence":null},"usage":{"output_tokens":25}}`,
		`{"type":"message_stop"}`,
	}
}

func request() model.Request {
	return model.Request{
		Model: "claude-sonnet-4-6", System: "You pair with the engineer.",
		Tools: []model.Tool{{Name: "grep", Description: "Search files.", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}},
			"required": []any{"pattern"}, "additionalProperties": false}}},
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "the case block", Break: true},
			{Role: model.RoleUser, Text: "what failed?", Break: true},
		},
	}
}

// An answer streams its text as it arrives; the response holds the whole
// text, the tool call with its input assembled, and the usage, already
// disjoint.
func TestStreamedAnswer(t *testing.T) {
	s := &server{events: answer("tool_use")}
	var got []model.Delta
	out, err := newProvider(t, s).Call(context.Background(), request(), func(d model.Delta) { got = append(got, d) })
	if err != nil {
		t.Fatal(err)
	}
	if d := cmp.Diff([]model.Delta{{Text: "The router "}, {Text: "is pending."}}, got); d != "" {
		t.Errorf("deltas (-want +got):\n%s", d)
	}
	wantCalls := []model.ToolCall{{ID: "toolu_1", Name: "grep", Args: map[string]any{"pattern": "Pending"}}}
	if out.Text != "The router is pending." || out.ModelID != "claude-sonnet-4-6" {
		t.Errorf("answer %q model %q", out.Text, out.ModelID)
	}
	if d := cmp.Diff(wantCalls, out.Calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
	u := out.Usage
	if u.InputTokens.Value != 40 || u.CacheReadTokens.Value != 3000 || u.CacheWriteTokens.Value != 5 || u.OutputTokens.Value != 25 || u.ReasoningTokens.Known {
		t.Errorf("usage %+v", u)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(out.Raw, &raw); err != nil || len(raw) != len(s.events) {
		t.Errorf("raw: %d events, %v", len(raw), err)
	}
}

// The request streams, takes no structured output and no tool choice, and
// declares each tool's schema whole. The system prompt carries a
// breakpoint, and so does each of the conversation's last three breaks;
// consecutive messages of one role are one message, results before text.
func TestCacheBreakpoints(t *testing.T) {
	s := &server{events: answer("end_turn")}
	r := request()
	r.NoCalls = true
	r.Messages = append(r.Messages,
		model.Message{Role: model.RoleModel, Text: "Looking.", Calls: []model.ToolCall{{ID: "toolu_1", Name: "grep", Args: map[string]any{"pattern": "x"}}}, Break: true},
		model.Message{Role: model.RoleUser, Results: []model.ToolResult{{CallID: "toolu_1", Name: "grep", Text: "refused: no such path", IsError: true}}, Break: true},
		model.Message{Role: model.RoleUser, Text: "and now?"},
	)
	if _, err := newProvider(t, s).Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	body := s.bodies[0]
	if body["stream"] != true || body["output_config"] != nil || body["tool_choice"] != nil {
		t.Errorf("stream %v output_config %v tool_choice %v", body["stream"], body["output_config"], body["tool_choice"])
	}
	if !strings.HasSuffix(s.paths[0], ":streamRawPredict") {
		t.Errorf("path %s", s.paths[0])
	}
	tool := as[map[string]any](t, as[[]any](t, body["tools"])[0])
	schema := as[map[string]any](t, tool["input_schema"])
	if schema["additionalProperties"] != false || as[[]any](t, schema["required"])[0] != "pattern" {
		t.Errorf("tool schema %v", schema)
	}
	if sys := as[map[string]any](t, as[[]any](t, body["system"])[0]); sys["cache_control"] == nil {
		t.Errorf("system has no breakpoint")
	}

	var got []string
	messages := as[[]any](t, body["messages"])
	for _, m := range messages {
		msg := as[map[string]any](t, m)
		var blocks []string
		for _, b := range as[[]any](t, msg["content"]) {
			block := as[map[string]any](t, b)
			kind := as[string](t, block["type"])
			if block["cache_control"] != nil {
				kind += "*"
			}
			blocks = append(blocks, kind)
		}
		got = append(got, as[string](t, msg["role"])+":"+strings.Join(blocks, ","))
	}
	// Four breaks: the case block's, the oldest, is the one left unmarked.
	want := []string{"user:text,text*", "assistant:text,tool_use*", "user:tool_result*,text"}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("messages (-want +got):\n%s", d)
	}
	content := as[[]any](t, as[map[string]any](t, messages[2])["content"])
	result := as[map[string]any](t, content[0])
	if result["tool_use_id"] != "toolu_1" || result["is_error"] != true {
		t.Errorf("tool result %v", result)
	}
}

// One HTTP attempt per call: a server error is returned, not retried.
func TestNoRetry(t *testing.T) {
	s := &server{status: http.StatusInternalServerError}
	if _, err := newProvider(t, s).Call(context.Background(), request(), nil); err == nil {
		t.Fatal("no error")
	}
	if len(s.bodies) != 1 {
		t.Errorf("requests %d, want 1", len(s.bodies))
	}
}

// An answer cut at its output budget, or stopped for another reason, is an
// error that names it, with what was written beside it.
func TestStopReasons(t *testing.T) {
	for stop, want := range map[string]error{"max_tokens": model.ErrOutputLimit, "refusal": model.ErrStopped} {
		out, err := newProvider(t, &server{events: answer(stop)}).Call(context.Background(), request(), nil)
		if !errors.Is(err, want) || out == nil || out.Text != "The router is pending." {
			t.Errorf("%s: err %v, out %+v", stop, err, out)
		}
	}
}

// as is v as a T, or the test's end: a request body that is not shaped as
// the test expects.
func as[T any](t *testing.T, v any) T {
	t.Helper()
	x, ok := v.(T)
	if !ok {
		t.Fatalf("%v is a %T", v, v)
	}
	return x
}
