// Package openai is the model layer's provider for OpenAI-compatible Chat
// Completions endpoints, such as OpenRouter or a self-hosted inference
// server: one streamed request per call over the standard library's HTTP
// client, which retries nothing.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lowmagic/compa-engine/internal/model"
)

// ID names the provider in configuration and records.
const ID = "openai"

// Keywords are the JSON Schema keywords a tool's parameters may use: the
// ones a live two-tool call through OpenRouter accepted
// (TestLiveTwoToolTurn).
var Keywords = []string{"additionalProperties", "default", "description", "enum", "items", "maxLength", "maximum", "minimum", "pattern", "properties", "required", "type"}

// Options configure a provider.
type Options struct {
	// BaseURL is the endpoint's, up to /chat/completions.
	BaseURL string
	// APIKey is sent as a bearer token; empty sends none, for an endpoint
	// that takes none.
	APIKey string
	// EgressClass is where the endpoint's calls go, as the deployment
	// declares it: external_provider for a public service, nearer for
	// its own.
	EgressClass string
	// CacheMarks sends the engine's cache breaks as Anthropic-style
	// cache_control marks on content parts, which OpenRouter passes to
	// the models that take them; an endpoint that refuses the field is
	// sent none.
	CacheMarks bool
	// HTTPClient replaces the default client, for tests.
	HTTPClient *http.Client
}

// Provider calls one OpenAI-compatible endpoint.
type Provider struct {
	o Options
}

// New builds a provider; it makes no request.
func New(o Options) (*Provider, error) {
	if o.BaseURL == "" {
		return nil, errors.New("openai: a base URL is required")
	}
	switch o.EgressClass {
	case "local_only", "internal_network", "external_provider":
	default:
		return nil, fmt.Errorf("openai: egress class %q", o.EgressClass)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{}
	}
	o.BaseURL = strings.TrimSuffix(o.BaseURL, "/")
	return &Provider{o: o}, nil
}

// Describe says where the provider's calls go: the class the deployment
// declared for its endpoint.
func (p *Provider) Describe() model.Descriptor {
	return model.Descriptor{ID: ID, EgressClass: p.o.EgressClass}
}

// Describe is what a provider of these options says of itself, before one
// is made.
func Describe(egressClass string) model.Descriptor {
	return model.Descriptor{ID: ID, EgressClass: egressClass}
}

// messageBreakpoints is how many of the conversation's last breaks carry
// a cache mark, besides the system prompt's: Anthropic takes four marks a
// request.
const messageBreakpoints = 3

type request struct {
	Model           string         `json:"model"`
	Messages        []message      `json:"messages"`
	Tools           []tool         `json:"tools,omitempty"`
	ToolChoice      string         `json:"tool_choice,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Stream          bool           `json:"stream"`
	StreamOptions   map[string]any `json:"stream_options"`
}

type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type part struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type toolCall struct {
	Index    *int     `json:"index,omitempty"`
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Function function `json:"function"`
}

type function struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string  `json:"type"`
	Function toolDef `json:"function"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			Reasoning string     `json:"reasoning"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

type usage struct {
	PromptTokens        *int64   `json:"prompt_tokens"`
	CompletionTokens    *int64   `json:"completion_tokens"`
	Cost                *float64 `json:"cost"`
	PromptTokensDetails *struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// Call streams one chat completion. NoCalls is sent as tool_choice none,
// the standard's own; effort as reasoning_effort, where the profile sets
// one.
func (p *Provider) Call(ctx context.Context, r model.Request, stream func(model.Delta)) (*model.Response, error) {
	body := request{
		Model: r.Model, Messages: p.messages(r), Tools: tools(r.Tools), MaxTokens: r.MaxOutputTokens,
		ReasoningEffort: r.Effort, Stream: true, StreamOptions: map[string]any{"include_usage": true},
	}
	if r.NoCalls && len(body.Tools) > 0 {
		body.ToolChoice = "none"
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.o.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.o.APIKey)
	}
	resp, err := p.o.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048)) // the error's text is what the engineer is shown; a failed read leaves the status alone
		return nil, fmt.Errorf("openai: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return read(resp.Body, stream)
}

// read accumulates the event stream: text and reasoning as they arrive,
// each tool call by its index, the finish reason and the usage, which
// comes in the last chunk.
func read(body io.Reader, stream func(model.Delta)) (*model.Response, error) {
	out := &model.Response{}
	var (
		raw      []json.RawMessage
		text     strings.Builder
		thinking strings.Builder
		calls    []*toolCall
		finish   string
		u        *usage
	)
	sc := bufio.NewScanner(body)
	// A chunk is one line; a long tool call's arguments arrive in pieces,
	// but a provider may still send a large one whole.
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // a comment, such as a keep-alive, or a blank line
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		raw = append(raw, json.RawMessage(data))
		var c chunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return nil, fmt.Errorf("openai: a stream chunk: %w", err)
		}
		if c.Error != nil {
			return nil, fmt.Errorf("openai: %v: %s", c.Error.Code, c.Error.Message)
		}
		if c.Model != "" {
			out.ModelID = c.Model
		}
		if c.Usage != nil {
			u = c.Usage
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			if d.Content != "" {
				text.WriteString(d.Content)
				if stream != nil {
					stream(model.Delta{Text: d.Content})
				}
			}
			if d.Reasoning != "" {
				thinking.WriteString(d.Reasoning)
				if stream != nil {
					stream(model.Delta{Thinking: d.Reasoning})
				}
			}
			for _, tc := range d.ToolCalls {
				i := len(calls)
				if tc.Index != nil {
					i = *tc.Index
				}
				for len(calls) <= i {
					calls = append(calls, &toolCall{})
				}
				acc := calls[i]
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.Function.Name = tc.Function.Name
				}
				acc.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	// Every element is a chunk's own JSON, so marshalling cannot fail.
	out.Raw, _ = json.Marshal(raw)
	out.Text, out.Thinking = text.String(), thinking.String()
	for _, c := range calls {
		var args map[string]any
		if strings.TrimSpace(c.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
				// The engine validates the call's parameters; arguments
				// that are not an object become none, which fail there.
				args = nil
			}
		}
		out.Calls = append(out.Calls, model.ToolCall{ID: c.ID, Name: c.Function.Name, Args: args})
	}
	out.Usage = usageOf(u)
	if u != nil && u.Cost != nil {
		// OpenRouter reports the call's cost in its credits, which are US
		// dollars.
		out.Cost = &model.Money{Amount: *u.Cost, Currency: "USD"}
	}
	switch finish {
	case "stop", "tool_calls":
	case "length":
		return out, fmt.Errorf("openai: finish_reason length: %w", model.ErrOutputLimit)
	case "":
		return out, fmt.Errorf("openai: the stream ended with no finish reason: %w", model.ErrStopped)
	default:
		return out, fmt.Errorf("openai: finish_reason %s: %w", finish, model.ErrStopped)
	}
	return out, nil
}

// usageOf maps the standard's usage, whose prompt count includes the cache
// reads and writes its details report and whose completion count includes
// the reasoning its details report: input is the prompt less them, output
// the completion less reasoning. An endpoint that reports no prompt
// details leaves the prompt's composition unknown, so input and the cache
// categories are unavailable; one that reports cache reads but no writes
// leaves writes unavailable, any it made counted once, in input. Reasoning
// not reported is unavailable and output is the completion as reported.
func usageOf(u *usage) model.Usage {
	out := model.Usage{
		InputTokens: model.Unavailable, CacheReadTokens: model.Unavailable, CacheWriteTokens: model.Unavailable,
		OutputTokens: model.Unavailable, ReasoningTokens: model.Unavailable, Normalization: model.NormalizationDerived,
	}
	if u == nil {
		out.Normalization = model.NormalizationAmbiguous
		out.Ambiguous = []string{"input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens"}
		return out
	}
	if d := u.PromptTokensDetails; u.PromptTokens != nil && d != nil && d.CachedTokens != nil {
		in := *u.PromptTokens - *d.CachedTokens
		out.CacheReadTokens = model.Reported(*d.CachedTokens)
		if d.CacheWriteTokens != nil {
			in -= *d.CacheWriteTokens
			out.CacheWriteTokens = model.Reported(*d.CacheWriteTokens)
		}
		out.InputTokens = model.Reported(in)
	} else {
		out.Normalization = model.NormalizationAmbiguous
		out.Ambiguous = []string{"input_tokens", "cache_read_tokens", "cache_write_tokens"}
	}
	if u.CompletionTokens != nil {
		o := *u.CompletionTokens
		if d := u.CompletionTokensDetails; d != nil && d.ReasoningTokens != nil {
			o -= *d.ReasoningTokens
			out.ReasoningTokens = model.Reported(*d.ReasoningTokens)
		}
		out.OutputTokens = model.Reported(o)
	}
	return out
}

// messages are the conversation as the standard reads it: the system
// prompt first; a user message's tool results, one tool message each, then
// its text; a model message's text and its calls in one. With cache marks,
// the system prompt and the last messageBreakpoints breaks carry one.
func (p *Provider) messages(r model.Request) []message {
	marked := map[int]bool{}
	if p.o.CacheMarks {
		for i := len(r.Messages) - 1; i >= 0 && len(marked) < messageBreakpoints; i-- {
			if r.Messages[i].Break {
				marked[i] = true
			}
		}
	}
	content := func(text string, mark bool) any {
		if !mark {
			return text
		}
		return []part{{Type: "text", Text: text, CacheControl: map[string]any{"type": "ephemeral"}}}
	}
	out := []message{{Role: "system", Content: content(r.System, p.o.CacheMarks)}}
	for i, m := range r.Messages {
		if m.Role == model.RoleModel {
			am := message{Role: "assistant"}
			if m.Text != "" {
				am.Content = content(m.Text, marked[i] && len(m.Calls) == 0)
			}
			for _, c := range m.Calls {
				args := c.Args
				if args == nil {
					args = map[string]any{}
				}
				a, _ := json.Marshal(args) // decoded from JSON, so it encodes again
				am.ToolCalls = append(am.ToolCalls, toolCall{ID: c.ID, Type: "function", Function: function{Name: c.Name, Arguments: string(a)}})
			}
			out = append(out, am)
			continue
		}
		for j, res := range m.Results {
			out = append(out, message{Role: "tool", ToolCallID: res.CallID, Content: content(res.Text, marked[i] && m.Text == "" && j == len(m.Results)-1)})
		}
		if m.Text != "" {
			out = append(out, message{Role: "user", Content: content(m.Text, marked[i])})
		}
	}
	return out
}

// tools declares each tool with its parameter schema whole.
func tools(ts []model.Tool) []tool {
	out := make([]tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, tool{Type: "function", Function: toolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	return out
}
