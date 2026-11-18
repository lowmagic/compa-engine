// Package vertexgemini is the model provider for Gemini models on Vertex
// AI, through Google's genai SDK. It authenticates with Application Default
// Credentials, streams each answer as free text and function calls, keeps
// an explicit cache of the conversation's stable prefix, and makes exactly
// one HTTP attempt per call.
package vertexgemini

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"

	"github.com/lowmagic/compa-engine/internal/model"
)

// Keywords are the JSON Schema keywords a tool's parameters may use: the
// ones a live call accepted (TestLiveTwoToolTurn). Vertex refuses a whole
// request over one keyword it does not take, as it did maxItems in a
// response schema, so a keyword new to the tools is probed live first.
var Keywords = []string{"additionalProperties", "default", "description", "enum", "items", "maxLength", "maximum", "minimum", "pattern", "properties", "required", "type"}

// ID names the provider in configuration and in invocation records.
const ID = "vertex-gemini"

// Options configure a provider.
type Options struct {
	Project  string
	Location string
	// BaseURL and HTTPClient replace the endpoint and transport, for
	// tests. An HTTPClient given here is used as is, without credentials.
	BaseURL    string
	HTTPClient *http.Client
	// CacheTTL is how long an explicit cache of a conversation's stable
	// prefix lives; zero caches nothing.
	CacheTTL time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// Provider calls Gemini models on Vertex and keeps the explicit caches it
// made.
type Provider struct {
	client *genai.Client
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	caches  []cache
	refused map[string]string // Vertex's error for each prefix it would not cache, by key
}

// cache is an explicit cache the provider made: the model, system prompt
// and tool declarations it holds, by base, and the first n contents of the
// conversation, by their digest, which are its first msgs messages.
type cache struct {
	base, sum string
	n, msgs   int
	tokens    int64 // estimated, of the messages it holds
	name      string
	expires   time.Time
}

// regrow is how much a conversation's cacheable part may outgrow the cache
// it reuses, as a share of what the cache holds, before a cache of the
// whole is made: past it, the uncached growth costs more over a few calls
// than caching it again once.
const regrow = 4

// minCacheTokens is Vertex's minimum for an explicit cache (found live:
// a smaller one is refused).
const minCacheTokens = 4096

// New builds a provider. Project and location are required: the SDK would
// otherwise take them from the environment, which is configuration the
// engine did not choose.
func New(ctx context.Context, o Options) (*Provider, error) {
	if o.Project == "" || o.Location == "" {
		return nil, errors.New("vertex-gemini: project and location are required")
	}
	one := int32(1)
	cc := &genai.ClientConfig{
		Backend:  genai.BackendVertexAI,
		Project:  o.Project,
		Location: o.Location,
		HTTPOptions: genai.HTTPOptions{
			BaseURL: o.BaseURL,
			// The SDK retries only when retry options are set; one attempt
			// is set explicitly so a later default cannot add retries.
			RetryOptions: &genai.HTTPRetryOptions{Attempts: &one},
		},
		HTTPClient: o.HTTPClient,
	}
	c, err := genai.NewClient(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("vertex-gemini: %w", err)
	}
	p := &Provider{client: c, ttl: o.CacheTTL, now: o.Now, refused: map[string]string{}}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

// Describe says where the provider's calls go.
func (*Provider) Describe() model.Descriptor {
	return model.Descriptor{ID: ID, EgressClass: "external_provider"}
}

// Call streams one generateContent request. The system prompt, the tool
// declarations and the conversation through its last break before the
// newest message are read from an explicit cache where the prefix is large
// enough (cached); Vertex refuses tools, tool config or a system
// instruction beside a cache (found live), so with a cache
// NoCalls is left to the engine, and without one it switches function
// calling off.
func (p *Provider) Call(ctx context.Context, r model.Request, stream func(model.Delta)) (*model.Response, error) {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(r.System, genai.RoleUser),
		CandidateCount:    1,
		// Thought summaries come back beside the answer, debug output for
		// the engineer. A level is sent only where the profile sets one:
		// without it, gemini-3.5-flash-lite does not think at all and
		// gemini-3.8-flash thinks some, and the levels each model accepts
		// differ (found live).
		ThinkingConfig: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingLevel: genai.ThinkingLevel(strings.ToUpper(r.Effort))},
		Tools:          tools(r.Tools),
	}
	if r.MaxOutputTokens > 0 {
		cfg.MaxOutputTokens = int32(r.MaxOutputTokens)
	}
	contents, ends := contents(r.Messages)
	rest, use := p.cached(ctx, r, cfg.SystemInstruction, cfg.Tools, contents, ends)
	if use != nil && use.Name != "" {
		cfg.CachedContent, cfg.SystemInstruction, cfg.Tools, contents = use.Name, nil, nil, rest
	} else if r.NoCalls && len(cfg.Tools) > 0 {
		cfg.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}
	}

	var (
		out    model.Response
		text   strings.Builder
		think  []string
		raw    []json.RawMessage
		finish genai.FinishReason
		usage  *genai.GenerateContentResponseUsageMetadata
	)
	for resp, err := range p.client.Models.GenerateContentStream(ctx, r.Model, contents, cfg) {
		if err != nil {
			return nil, fmt.Errorf("vertex-gemini: %w%s", err, refusal(use))
		}
		if b, err := json.Marshal(resp); err == nil {
			raw = append(raw, b)
		}
		if resp.UsageMetadata != nil {
			usage = resp.UsageMetadata
		}
		if resp.ModelVersion != "" {
			out.ModelID = resp.ModelVersion
		}
		if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
			if len(resp.Candidates) > 0 && resp.Candidates[0].FinishReason != "" {
				finish = resp.Candidates[0].FinishReason
			}
			continue
		}
		cand := resp.Candidates[0]
		if cand.FinishReason != "" {
			finish = cand.FinishReason
		}
		for _, part := range cand.Content.Parts {
			switch {
			case part.FunctionCall != nil:
				f := part.FunctionCall
				id := f.ID
				if id == "" {
					// A call without an ID is given one, so its result
					// can name it.
					id = fmt.Sprintf("call-%d", len(out.Calls)+1)
				}
				out.Calls = append(out.Calls, model.ToolCall{ID: id, Name: f.Name, Args: f.Args, Signature: part.ThoughtSignature})
			case part.Thought && part.Text != "":
				think = append(think, part.Text)
				if stream != nil {
					stream(model.Delta{Thinking: part.Text})
				}
			case part.Text != "":
				text.WriteString(part.Text)
				if stream != nil {
					stream(model.Delta{Text: part.Text})
				}
			}
		}
	}
	out.Text = text.String()
	out.Thinking = strings.Join(think, "")
	out.Usage, out.Cache = Usage(usage), use
	// Every element is a chunk the SDK marshalled, so marshalling cannot
	// fail.
	out.Raw, _ = json.Marshal(raw)

	switch finish {
	case genai.FinishReasonStop, "":
	case genai.FinishReasonMaxTokens:
		return &out, fmt.Errorf("vertex-gemini: max_output_tokens %d: %w", r.MaxOutputTokens, model.ErrOutputLimit)
	default:
		return &out, fmt.Errorf("vertex-gemini: %s: %w", finish, model.ErrStopped)
	}
	return &out, nil
}

// cached finds or makes the explicit cache of a call's stable prefix — the
// system prompt, the tools and the messages through the last break before
// the newest message, short of results that end it — and returns the
// contents still to send past the cache it read and what it did, nil where
// the provider caches nothing. Gemini caches nothing implicitly on our
// project, so without this every call pays for the whole conversation in
// full. A conversation grows at its end, so the longest live cache whose
// messages the call still begins with is reused, and the whole is cached
// again only once what lies past it outgrows a quarter of what it holds. A
// prefix the estimate puts under the minimum is not tried, and one Vertex
// refuses is not tried again; either way the call reads the cache it has,
// or none. A cache is reused until shortly before it expires, and expires
// by itself.
func (p *Provider) cached(ctx context.Context, r model.Request, sys *genai.Content, ts []*genai.Tool, contents []*genai.Content, ends []int) ([]*genai.Content, *model.Cache) {
	if p.ttl <= 0 {
		return nil, nil
	}
	end := -1
	for i := len(r.Messages) - 2; i >= 0; i-- {
		if r.Messages[i].Break {
			end = i
			break
		}
	}
	// Vertex refuses a cache whose contents end in function responses,
	// saying "Requests ending with a model turn are not supported", and
	// takes one ending in the model's signed function calls before them
	// (found live, TestLiveOpeningCache): within a tool loop every break
	// but the turn's first is such results, so the cache ends at the calls.
	if end > 0 && len(r.Messages[end].Results) > 0 && r.Messages[end].Text == "" && r.Messages[end-1].Role == model.RoleModel {
		end--
	}
	if end < 0 {
		return nil, &model.Cache{Outcome: model.CacheNoBreak}
	}
	sysTokens := model.EstimateTokens(r.System)
	var regionTokens int64
	for _, m := range r.Messages[:end+1] {
		regionTokens += estimate(m)
	}
	if sysTokens+regionTokens < minCacheTokens {
		return nil, &model.Cache{Outcome: model.CacheUnderMinimum}
	}
	// The declarations and contents are the SDK's own types, built from
	// JSON values, so marshalling them cannot fail.
	decls, _ := json.Marshal(ts)
	base := digest([]byte(r.Model + "\x00" + r.System + "\x00" + string(decls)))
	sumOf := func(n int) string {
		b, _ := json.Marshal(contents[:n])
		return digest(b)
	}
	now := p.now()

	p.mu.Lock()
	defer p.mu.Unlock()
	live := p.caches[:0]
	best := -1
	for _, ch := range p.caches {
		if !now.Add(time.Minute).Before(ch.expires) {
			continue
		}
		live = append(live, ch)
		if ch.base == base && ch.n <= ends[end] && (best < 0 || ch.n > live[best].n) && sumOf(ch.n) == ch.sum {
			best = len(live) - 1
		}
	}
	p.caches = live
	var read cache
	if best >= 0 {
		read = p.caches[best]
	}
	use := &model.Cache{Outcome: model.CacheReused}
	if read.name == "" || (regionTokens-read.tokens)*regrow > sysTokens+read.tokens {
		sum := sumOf(ends[end])
		key := base + sum
		refused, before := p.refused[key]
		if !before {
			cc, err := p.client.Caches.Create(ctx, r.Model, &genai.CreateCachedContentConfig{
				TTL:               p.ttl,
				SystemInstruction: sys,
				Contents:          contents[:ends[end]],
				Tools:             ts,
			})
			if err != nil {
				refused = err.Error()
				p.refused[key] = refused
			} else {
				read = cache{base: base, sum: sum, n: ends[end], msgs: end + 1, tokens: regionTokens, name: cc.Name, expires: now.Add(p.ttl)}
				p.caches = append(p.caches, read)
				use.Outcome = model.CacheMade
			}
		}
		if refused != "" {
			use.Outcome, use.Error = model.CacheRefused, refused
		}
	}
	if read.name == "" {
		return nil, use
	}
	tokens := model.Estimated(sysTokens+read.tokens, model.EstimatorVersion)
	use.Name, use.Messages, use.Tokens = read.name, read.msgs, &tokens
	return contents[read.n:], use
}

// refusal is a failed generation's word on a cache it could not make, so
// the error recorded in its place still says so; empty otherwise.
func refusal(use *model.Cache) string {
	if use == nil || use.Outcome != model.CacheRefused {
		return ""
	}
	return " (making its cache failed: " + use.Error + ")"
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// estimate is a message's estimated tokens, for the cache's minimum and
// its regrowth.
func estimate(m model.Message) int64 {
	n := model.EstimateTokens(m.Text)
	for _, c := range m.Calls {
		a, _ := json.Marshal(c.Args) // decoded from JSON, so it encodes again
		n += model.EstimateTokens(c.Name) + model.EstimateTokens(string(a))
	}
	for _, r := range m.Results {
		n += model.EstimateTokens(r.Text)
	}
	return n
}

// contents are the conversation's messages as Gemini reads them, with the
// number of contents through each message: a model message's text, then
// its function calls with the thought signatures Gemini requires back; a
// user message's function responses, then its text in a content of its
// own. Vertex refuses a content holding function responses and text
// together, saying "Requests ending with a model turn are not supported"
// (found live).
func contents(ms []model.Message) ([]*genai.Content, []int) {
	out := make([]*genai.Content, 0, len(ms))
	ends := make([]int, 0, len(ms))
	for _, m := range ms {
		if m.Role == model.RoleModel {
			var parts []*genai.Part
			if m.Text != "" {
				parts = append(parts, genai.NewPartFromText(m.Text))
			}
			for _, c := range m.Calls {
				part := genai.NewPartFromFunctionCall(c.Name, c.Args)
				part.FunctionCall.ID = c.ID
				part.ThoughtSignature = c.Signature
				parts = append(parts, part)
			}
			if len(parts) == 0 {
				parts = append(parts, genai.NewPartFromText(""))
			}
			out = append(out, &genai.Content{Role: genai.RoleModel, Parts: parts})
			ends = append(ends, len(out))
			continue
		}
		if len(m.Results) > 0 {
			parts := make([]*genai.Part, 0, len(m.Results))
			for _, r := range m.Results {
				key := "output"
				if r.IsError {
					key = "error"
				}
				part := genai.NewPartFromFunctionResponse(r.Name, map[string]any{key: r.Text})
				part.FunctionResponse.ID = r.CallID
				parts = append(parts, part)
			}
			out = append(out, &genai.Content{Role: genai.RoleUser, Parts: parts})
		}
		if m.Text != "" || len(m.Results) == 0 {
			out = append(out, genai.NewContentFromText(m.Text, genai.RoleUser))
		}
		ends = append(ends, len(out))
	}
	return out, ends
}

func tools(ts []model.Tool) []*genai.Tool {
	if len(ts) == 0 {
		return nil
	}
	decls := make([]*genai.FunctionDeclaration, 0, len(ts))
	for _, t := range ts {
		decls = append(decls, &genai.FunctionDeclaration{Name: t.Name, Description: t.Description, ParametersJsonSchema: t.Parameters})
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}

// Usage normalizes Gemini's usage metadata into disjoint categories. The
// API omits zero counts, so an absent field in present metadata is a
// measured zero; absent metadata leaves everything unavailable.
//
// promptTokenCount includes cachedContentTokenCount, so input is their
// difference (derived). Thinking tokens are reported apart from candidate
// tokens. Gemini reports no cache writes. Tool-use prompt tokens count
// built-in tools' own prompts, which are never enabled (function
// declarations are counted as prompt, 0 tool-use tokens live);
// if they occur, input composition is unknown and input is unavailable.
func Usage(m *genai.GenerateContentResponseUsageMetadata) model.Usage {
	u := model.Usage{
		InputTokens: model.Unavailable, CacheReadTokens: model.Unavailable, CacheWriteTokens: model.Unavailable,
		OutputTokens: model.Unavailable, ReasoningTokens: model.Unavailable,
		Normalization: model.NormalizationDerived,
	}
	if m == nil {
		return u
	}
	u.CacheReadTokens = model.Reported(int64(m.CachedContentTokenCount))
	u.OutputTokens = model.Reported(int64(m.CandidatesTokenCount))
	u.ReasoningTokens = model.Reported(int64(m.ThoughtsTokenCount))
	if m.ToolUsePromptTokenCount > 0 || m.CachedContentTokenCount > m.PromptTokenCount {
		u.Ambiguous = []string{"input_tokens"}
		return u
	}
	u.InputTokens = model.Reported(int64(m.PromptTokenCount - m.CachedContentTokenCount))
	return u
}
