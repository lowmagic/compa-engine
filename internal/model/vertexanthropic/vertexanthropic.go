// Package vertexanthropic is the model provider for Claude models on
// Vertex AI, through Anthropic's Go SDK. It authenticates with Application
// Default Credentials, streams each answer as free text and tool calls,
// marks Claude's cache breakpoints, and makes exactly one HTTP attempt per
// call. It uses tool use alone, never the structured-output feature, which
// a Vertex organization policy may allow only per model.
package vertexanthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"

	"github.com/lowmagic/compa-engine/internal/model"
)

// ID names the provider in configuration and in invocation records.
const ID = "vertex-anthropic"

// Keywords are the JSON Schema keywords a tool's parameters may use: the
// ones a live call accepted (TestLiveTwoToolTurn).
var Keywords = []string{"additionalProperties", "default", "description", "enum", "items", "maxLength", "maximum", "minimum", "pattern", "properties", "required", "type"}

// defaultMaxTokens applies when a request sets no output budget; the API
// requires one.
const defaultMaxTokens = 8192

// messageBreakpoints is how many of the conversation's breaks are marked:
// Claude takes four breakpoints a call, and the system prompt has one.
const messageBreakpoints = 3

// Options configure a provider.
type Options struct {
	Project  string
	Location string
	// Credentials replace Application Default Credentials, and BaseURL the
	// Vertex endpoint, for tests.
	Credentials *google.Credentials
	BaseURL     string
}

// Provider calls Claude models on Vertex.
type Provider struct {
	client anthropic.Client
}

// New builds a provider. Project and location are required; the
// environment's Anthropic credentials and base URL are never used.
func New(ctx context.Context, o Options) (*Provider, error) {
	if o.Project == "" || o.Location == "" {
		return nil, errors.New("vertex-anthropic: project and location are required")
	}
	creds := o.Credentials
	if creds == nil {
		var err error
		if creds, err = google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform"); err != nil {
			return nil, fmt.Errorf("vertex-anthropic: %w", err)
		}
	}
	opts := []option.RequestOption{
		vertex.WithCredentials(ctx, o.Location, o.Project, creds),
		// The SDK retries by default.
		option.WithMaxRetries(0),
	}
	if o.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(o.BaseURL))
	}
	return &Provider{client: anthropic.NewClient(opts...)}, nil
}

// Describe says where the provider's calls go.
func (*Provider) Describe() model.Descriptor {
	return model.Descriptor{ID: ID, EgressClass: "external_provider"}
}

// Call streams one Messages request with no thinking. Tools come first in
// Claude's cached prefix, so the system prompt's breakpoint caches them
// too; the conversation's last three breaks carry the others, so the next
// call reads everything through its last break from the cache. A prefix
// under the model's minimum is silently not cached. NoCalls is not sent:
// changing tool_choice invalidates Claude's cached messages, so the
// engine's instruction and its refusal of any call carry it.
func (p *Provider) Call(ctx context.Context, r model.Request, stream func(model.Delta)) (*model.Response, error) {
	maxTokens := int64(r.MaxOutputTokens)
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	params := anthropic.MessageNewParams{
		Model:     r.Model,
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: r.System, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
		Messages:  messages(r.Messages),
		Tools:     tools(r.Tools),
	}
	s := p.client.Messages.NewStreaming(ctx, params)
	defer s.Close()

	var (
		msg anthropic.Message
		raw []json.RawMessage
	)
	for s.Next() {
		ev := s.Current()
		raw = append(raw, json.RawMessage(ev.RawJSON()))
		if err := msg.Accumulate(ev); err != nil {
			return nil, fmt.Errorf("vertex-anthropic: %w", err)
		}
		if stream == nil || ev.Type != "content_block_delta" {
			continue
		}
		if d := ev.AsContentBlockDelta().Delta; d.Type == "text_delta" && d.Text != "" {
			stream(model.Delta{Text: d.Text})
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("vertex-anthropic: %w", err)
	}

	out := &model.Response{ModelID: msg.Model, Usage: Usage(msg.Usage)}
	// Every element is an event's own JSON, so marshalling cannot fail.
	out.Raw, _ = json.Marshal(raw)
	var text strings.Builder
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			u := b.AsToolUse()
			var args map[string]any
			if err := json.Unmarshal(u.Input, &args); err != nil {
				// The engine validates the call's parameters; an input that
				// is not an object becomes none, which fails there.
				args = nil
			}
			out.Calls = append(out.Calls, model.ToolCall{ID: u.ID, Name: u.Name, Args: args})
		}
	}
	out.Text = text.String()

	switch msg.StopReason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonToolUse, anthropic.StopReasonStopSequence:
	case anthropic.StopReasonMaxTokens:
		return out, fmt.Errorf("vertex-anthropic: max_tokens %d: %w", maxTokens, model.ErrOutputLimit)
	default:
		return out, fmt.Errorf("vertex-anthropic: %s: %w", msg.StopReason, model.ErrStopped)
	}
	return out, nil
}

// messages are the conversation's messages, consecutive ones of one role
// merged, as Claude reads them: a user message's tool results first, then
// its text; a model message's text, then its tool calls. The last block of
// each of the last three breaks carries a cache breakpoint.
func messages(ms []model.Message) []anthropic.MessageParam {
	marked := map[int]bool{}
	for i := len(ms) - 1; i >= 0 && len(marked) < messageBreakpoints; i-- {
		if ms[i].Break {
			marked[i] = true
		}
	}
	var out []anthropic.MessageParam
	for i, m := range ms {
		var blocks []anthropic.ContentBlockParamUnion
		role := anthropic.MessageParamRoleUser
		if m.Role == model.RoleModel {
			role = anthropic.MessageParamRoleAssistant
			if m.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Text))
			}
			for _, c := range m.Calls {
				args := c.Args
				if args == nil {
					args = map[string]any{}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, args, c.Name))
			}
		} else {
			for _, r := range m.Results {
				blocks = append(blocks, anthropic.NewToolResultBlock(r.CallID, r.Text, r.IsError))
			}
			if m.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Text))
			}
		}
		if len(blocks) == 0 {
			continue
		}
		if marked[i] {
			mark(blocks[len(blocks)-1])
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, anthropic.MessageParam{Role: role, Content: blocks})
	}
	return out
}

func mark(b anthropic.ContentBlockParamUnion) {
	cc := anthropic.NewCacheControlEphemeralParam()
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = cc
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = cc
	}
}

// tools declares each tool with its parameter schema whole: its properties
// and required list in their fields, every other keyword beside them.
func tools(ts []model.Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(ts))
	for _, t := range ts {
		schema := anthropic.ToolInputSchemaParam{Properties: t.Parameters["properties"]}
		extra := map[string]any{}
		for k, v := range t.Parameters {
			switch k {
			case "type", "properties":
			case "required":
				schema.Required = append(schema.Required, asStrings(v)...)
			default:
				extra[k] = v
			}
		}
		if len(extra) > 0 {
			schema.ExtraFields = extra
		}
		tool := anthropic.ToolParam{Name: t.Name, Description: anthropic.String(t.Description), InputSchema: schema}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return out
}

func asStrings(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		var out []string
		for _, n := range l {
			if s, ok := n.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Usage maps Claude's usage, whose categories are already disjoint: input
// excludes cache reads and cache writes. Thinking is not enabled, and the
// API reports no separate reasoning count, so reasoning is unavailable.
func Usage(u anthropic.Usage) model.Usage {
	return model.Usage{
		InputTokens:      model.Reported(u.InputTokens),
		CacheReadTokens:  model.Reported(u.CacheReadInputTokens),
		CacheWriteTokens: model.Reported(u.CacheCreationInputTokens),
		OutputTokens:     model.Reported(u.OutputTokens),
		ReasoningTokens:  model.Unavailable,
		Normalization:    model.NormalizationNativeDisjoint,
	}
}
