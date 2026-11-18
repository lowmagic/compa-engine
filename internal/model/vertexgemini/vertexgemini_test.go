package vertexgemini_test

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
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/vertexgemini"
)

// server records each request and answers generation with the chunks of
// stream, as server-sent events, and cache creation with a new name, or
// with Vertex's refusal where the cache's contents end in function
// responses (found live, TestLiveOpeningCache).
type server struct {
	mu      sync.Mutex
	bodies  []map[string]any
	paths   []string
	creates int
	refuse  bool
	status  int
	stream  []string
}

func (s *server) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m) // a body that is not JSON is recorded as nil
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies = append(s.bodies, m)
	s.paths = append(s.paths, r.URL.Path)
	if strings.HasSuffix(r.URL.Path, "/cachedContents") {
		w.Header().Set("Content-Type", "application/json")
		s.creates++
		if s.refuse {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"The minimum token count to start explicit caching is 4096.","status":"INVALID_ARGUMENT"}}`)
			return
		}
		if endsInResponses(m) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"Requests ending with a model turn are not supported.","status":"INVALID_ARGUMENT"}}`)
			return
		}
		fmt.Fprintf(w, `{"name":"projects/proj/locations/global/cachedContents/c%d"}`, s.creates)
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		fmt.Fprint(w, `{"error":{"code":500,"message":"boom","status":"INTERNAL"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range s.stream {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
}

func newProvider(t *testing.T, s *server, now *time.Time) *vertexgemini.Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	o := vertexgemini.Options{Project: "proj", Location: "global", BaseURL: srv.URL, HTTPClient: srv.Client()}
	if now != nil {
		o.CacheTTL = 10 * time.Minute
		o.Now = func() time.Time { return *now }
	}
	p, err := vertexgemini.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const done = `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1},"modelVersion":"gemini-3.8-flash"}`

func request() model.Request {
	return model.Request{
		Model: "gemini-3.8-flash", Effort: "low", System: "You pair with the engineer.",
		Tools: []model.Tool{{Name: "grep", Description: "Search files.", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}}, "required": []any{"pattern"}}}},
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "the case block", Break: true},
			{Role: model.RoleUser, Text: "what failed?"},
		},
	}
}

// An answer streams: text and thought summaries reach the stream as they
// arrive, and the response holds the whole text, the thinking, the calls
// with their thought signatures, and the usage of the last chunk.
func TestStreamedAnswer(t *testing.T) {
	s := &server{stream: []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"weighing it","thought":true}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"The router "}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"is pending."},{"functionCall":{"id":"f1","name":"grep","args":{"pattern":"Pending"}},"thoughtSignature":"c2ln"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":120,"cachedContentTokenCount":100,"candidatesTokenCount":9,"thoughtsTokenCount":4},"modelVersion":"gemini-3.8-flash-001"}`,
	}}
	p := newProvider(t, s, nil)
	var got []model.Delta
	out, err := p.Call(context.Background(), request(), func(d model.Delta) { got = append(got, d) })
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Delta{{Thinking: "weighing it"}, {Text: "The router "}, {Text: "is pending."}}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("deltas (-want +got):\n%s", d)
	}
	if out.Text != "The router is pending." || out.Thinking != "weighing it" || out.ModelID != "gemini-3.8-flash-001" {
		t.Errorf("answer %q thinking %q model %q", out.Text, out.Thinking, out.ModelID)
	}
	wantCalls := []model.ToolCall{{ID: "f1", Name: "grep", Args: map[string]any{"pattern": "Pending"}, Signature: []byte("sig")}}
	if d := cmp.Diff(wantCalls, out.Calls); d != "" {
		t.Errorf("calls (-want +got):\n%s", d)
	}
	if u := out.Usage; u.InputTokens.Value != 20 || u.CacheReadTokens.Value != 100 || u.OutputTokens.Value != 9 || u.ReasoningTokens.Value != 4 {
		t.Errorf("usage %+v", u)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(out.Raw, &raw); err != nil || len(raw) != 3 {
		t.Errorf("raw: %d chunks, %v", len(raw), err)
	}
}

// The request carries the system prompt, the tools, the effort as the
// thinking level, and the conversation: a model's calls with their thought
// signatures, and each result, a refusal as an error, before the text.
func TestThinkingLevelSent(t *testing.T) {
	s := &server{stream: []string{done}}
	p := newProvider(t, s, nil)
	r := request()
	r.Messages = append(r.Messages,
		model.Message{Role: model.RoleModel, Text: "Looking.", Calls: []model.ToolCall{{ID: "f1", Name: "grep", Args: map[string]any{"pattern": "x"}, Signature: []byte("sig")}}},
		model.Message{Role: model.RoleUser, Results: []model.ToolResult{{CallID: "f1", Name: "grep", Text: "refused: no such path", IsError: true}}, Text: "and now?"},
	)
	if _, err := p.Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	body := s.bodies[0]
	if !strings.HasSuffix(s.paths[0], ":streamGenerateContent") {
		t.Errorf("path %s", s.paths[0])
	}
	gc := as[map[string]any](t, body["generationConfig"])
	if tc := as[map[string]any](t, gc["thinkingConfig"]); tc["thinkingLevel"] != "LOW" || tc["includeThoughts"] != true {
		t.Errorf("thinking config %v", tc)
	}
	if body["systemInstruction"] == nil || body["tools"] == nil || body["toolConfig"] != nil {
		t.Errorf("system %v tools %v toolConfig %v", body["systemInstruction"], body["tools"], body["toolConfig"])
	}
	contents := as[[]any](t, body["contents"])
	if len(contents) != 5 {
		t.Fatalf("contents %d, want 5", len(contents))
	}
	parts := func(i int) []any { return as[[]any](t, as[map[string]any](t, contents[i])["parts"]) }
	call := as[map[string]any](t, parts(2)[1])
	if call["thoughtSignature"] != "c2ln" || as[map[string]any](t, call["functionCall"])["id"] != "f1" {
		t.Errorf("model's call %v", call)
	}
	user := parts(3)
	if len(user) != 1 {
		t.Errorf("the results' content holds %d parts, want the result alone", len(user))
	}
	resp := as[map[string]any](t, as[map[string]any](t, user[0])["functionResponse"])
	if resp["id"] != "f1" || resp["name"] != "grep" || as[map[string]any](t, resp["response"])["error"] != "refused: no such path" {
		t.Errorf("result %v", resp)
	}
	if text := as[map[string]any](t, parts(4)[0])["text"]; text != "and now?" {
		t.Errorf("text after results %v", text)
	}
}

// One HTTP attempt per call: a server error is returned, not retried.
func TestNoRetry(t *testing.T) {
	s := &server{status: http.StatusInternalServerError}
	p := newProvider(t, s, nil)
	if _, err := p.Call(context.Background(), request(), nil); err == nil {
		t.Fatal("no error")
	}
	if len(s.bodies) != 1 {
		t.Errorf("requests %d, want 1", len(s.bodies))
	}
}

// An answer cut at its output budget, or stopped for another reason, is an
// error that names it, with what was written beside it.
func TestStopReasons(t *testing.T) {
	for reason, want := range map[string]error{"MAX_TOKENS": model.ErrOutputLimit, "SAFETY": model.ErrStopped} {
		s := &server{stream: []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"` + reason + `"}]}`}}
		out, err := newProvider(t, s, nil).Call(context.Background(), request(), nil)
		if !errors.Is(err, want) || out == nil || out.Text != "partial" {
			t.Errorf("%s: err %v, out %+v", reason, err, out)
		}
	}
}

// big is a request whose prefix through the case block is over Vertex's
// cache minimum by the estimate.
func big() model.Request {
	r := request()
	r.Messages[0].Text = strings.Repeat("node worker-1 reports NotReady since 10:12 ", 900)
	return r
}

// The prefix through the last break before the newest message is cached
// once and reused while the conversation grows a little at its end, the
// call sending only what lies past the cache, with neither system prompt
// nor tools; grown past a quarter of what the cache holds, the whole is
// cached again; a cache near its expiry is made again. Each call says
// what it did and which cache it read, holding how much.
func TestExplicitCacheGrows(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := &server{stream: []string{done}}
	p := newProvider(t, s, &now)
	r := big()
	out, err := p.Call(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantCache(t, out, model.CacheMade, 1, 1)
	sent := func(body map[string]any) int { return len(as[[]any](t, body["contents"])) }
	create, gen := s.bodies[0], s.bodies[1]
	if s.creates != 1 || sent(create) != 1 || create["ttl"] != "600s" || create["tools"] == nil || create["systemInstruction"] == nil {
		t.Fatalf("cache create: creates %d, body keys %v", s.creates, keys(create))
	}
	if gen["cachedContent"] != cacheName(1) || gen["systemInstruction"] != nil || gen["tools"] != nil || sent(gen) != 1 {
		t.Fatalf("generation reads the cache: cachedContent %v, keys %v", gen["cachedContent"], keys(gen))
	}

	// A turn later: the answer and the next message follow; the last
	// message of the first call is now a break.
	r.Messages[1].Break = true
	r.Messages = append(r.Messages,
		model.Message{Role: model.RoleModel, Text: "The node is NotReady."},
		model.Message{Role: model.RoleUser, Text: "why?"})
	r.Messages[2].Break = true
	if out, err = p.Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	wantCache(t, out, model.CacheReused, 1, 1)
	last := s.bodies[len(s.bodies)-1]
	if s.creates != 1 || last["cachedContent"] != cacheName(1) || sent(last) != 3 {
		t.Fatalf("small growth: creates %d, %d contents sent", s.creates, sent(last))
	}

	// A long answer later, the growth passes a quarter: cached again,
	// through the last break.
	r.Messages = append(r.Messages,
		model.Message{Role: model.RoleModel, Text: strings.Repeat("the kubelet stopped posting status ", 600), Break: true},
		model.Message{Role: model.RoleUser, Text: "and then?"})
	if out, err = p.Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	wantCache(t, out, model.CacheMade, 2, 5)
	last = s.bodies[len(s.bodies)-1]
	if s.creates != 2 || last["cachedContent"] != cacheName(2) || sent(last) != 1 {
		t.Fatalf("large growth: creates %d, cache %v", s.creates, last["cachedContent"])
	}

	now = now.Add(9*time.Minute + 30*time.Second)
	if out, err = p.Call(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	if s.creates != 3 {
		t.Errorf("a cache near its expiry was reused: creates %d", s.creates)
	}
	wantCache(t, out, model.CacheMade, 3, 5)
}

// wantCache fails t unless out says the call's cache outcome is outcome,
// reading the n-th cache made, which holds msgs messages, their tokens
// estimated.
func wantCache(t *testing.T, out *model.Response, outcome string, n, msgs int) {
	t.Helper()
	c := out.Cache
	if c == nil || c.Outcome != outcome || c.Name != cacheName(n) || c.Messages != msgs || c.Error != "" ||
		c.Tokens == nil || c.Tokens.Fidelity != model.FidelityEstimated || c.Tokens.EstimatorVersion != model.EstimatorVersion || c.Tokens.Value <= 0 {
		t.Errorf("cache %+v, want %s of %s holding %d messages", c, outcome, cacheName(n), msgs)
	}
}

// A prefix under the minimum is not cached, and one Vertex refuses is not
// tried again; either way the call goes uncached, says why with Vertex's
// error, and NoCalls then turns function calling off.
func TestExplicitCacheSkipped(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := &server{stream: []string{done}}
	r := request()
	r.NoCalls = true
	out, err := newProvider(t, s, &now).Call(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.creates != 0 || s.bodies[0]["toolConfig"] == nil {
		t.Errorf("small prefix: creates %d, toolConfig %v", s.creates, s.bodies[0]["toolConfig"])
	}
	if c := out.Cache; c == nil || *c != (model.Cache{Outcome: model.CacheUnderMinimum}) {
		t.Errorf("small prefix: cache %+v", c)
	}

	s = &server{stream: []string{done}, refuse: true}
	p := newProvider(t, s, &now)
	for i := range 2 {
		out, err := p.Call(context.Background(), big(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if c := out.Cache; c == nil || c.Outcome != model.CacheRefused || !strings.Contains(c.Error, "The minimum token count to start explicit caching is 4096.") || c.Name != "" || c.Tokens != nil {
			t.Errorf("refused prefix, call %d: cache %+v", i+1, c)
		}
	}
	if s.creates != 1 || s.bodies[len(s.bodies)-1]["cachedContent"] != nil {
		t.Errorf("refused prefix: creates %d", s.creates)
	}
}

// Within a tool loop the cache grows to the model's signed calls before
// the newest results but one, the results that end the break left to be
// sent: Vertex refuses a cache ending in function responses. Synthetic:
// the case block, the ask, then one call a response with its results, one
// result long.
func TestExplicitCacheInToolLoop(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := &server{stream: []string{done}}
	p := newProvider(t, s, &now)
	r := big()
	results := []string{strings.Repeat("e1: 10:12 worker-1 kubelet PLEG is not healthy ", 500), "e2: 3 lines", "e3: 2 lines"}
	var out *model.Response
	for n := range 4 {
		// The engine's break: the newest message ends the call.
		r.Messages[len(r.Messages)-1].Break = true
		var err error
		if out, err = p.Call(context.Background(), r, nil); err != nil {
			t.Fatal(err)
		}
		if out.Cache == nil || out.Cache.Outcome == model.CacheRefused {
			t.Fatalf("call %d: cache %+v", n+1, out.Cache)
		}
		if n == 3 {
			break
		}
		id := fmt.Sprintf("f%d", n+1)
		r.Messages = append(r.Messages,
			model.Message{Role: model.RoleModel, Calls: []model.ToolCall{{ID: id, Name: "grep", Args: map[string]any{"pattern": "PLEG"}, Signature: []byte("sig")}}},
			model.Message{Role: model.RoleUser, Results: []model.ToolResult{{CallID: id, Name: "grep", Text: results[n]}}})
	}
	// The fourth call's last break is the second results, which follow
	// the long first; the cache ends at the model's call before them.
	wantCache(t, out, model.CacheMade, 2, 5)
	create := s.bodies[len(s.bodies)-2]
	contents := as[[]any](t, create["contents"])
	lastMade := as[map[string]any](t, contents[len(contents)-1])
	call := as[map[string]any](t, as[[]any](t, lastMade["parts"])[0])
	if len(contents) != 5 || lastMade["role"] != "model" || call["functionCall"] == nil || call["thoughtSignature"] != "c2ln" {
		t.Errorf("the cache made holds %d contents, the last %v", len(contents), lastMade)
	}
	gen := s.bodies[len(s.bodies)-1]
	sent := as[[]any](t, gen["contents"])
	first := as[map[string]any](t, as[[]any](t, as[map[string]any](t, sent[0])["parts"])[0])
	if gen["cachedContent"] != cacheName(2) || len(sent) != 3 || first["functionResponse"] == nil {
		t.Errorf("the call read %v and sent %d contents, the first %v", gen["cachedContent"], len(sent), first)
	}
}

// endsInResponses says a cache create's body ends in function responses.
func endsInResponses(body map[string]any) bool {
	contents, _ := body["contents"].([]any)
	if len(contents) == 0 {
		return false
	}
	last, _ := contents[len(contents)-1].(map[string]any)
	parts, _ := last["parts"].([]any)
	for _, p := range parts {
		if part, _ := p.(map[string]any); part["functionResponse"] != nil {
			return true
		}
	}
	return false
}

// Usage is derived: input is the prompt less what was read from cache;
// tool-use prompt tokens leave input unavailable; absent metadata leaves
// every category unavailable, never zero.
func TestUsage(t *testing.T) {
	s := &server{stream: []string{`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":50,"toolUsePromptTokenCount":3,"candidatesTokenCount":1}}`}}
	out, err := newProvider(t, s, nil).Call(context.Background(), request(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Usage.InputTokens.Known || len(out.Usage.Ambiguous) != 1 {
		t.Errorf("tool-use prompt: %+v", out.Usage)
	}
	if u := vertexgemini.Usage(nil); u.InputTokens.Known || u.OutputTokens.Known || u.CacheReadTokens.Known {
		t.Errorf("no metadata: %+v", u)
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func cacheName(n int) string {
	return fmt.Sprintf("projects/proj/locations/global/cachedContents/c%d", n)
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
