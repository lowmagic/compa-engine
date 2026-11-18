// Package fixture is a model provider that answers from a function instead
// of a model, for tests and offline runs. It sends nothing anywhere.
package fixture

import (
	"context"
	"sync"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
)

// Provider answers each call with Respond, streaming the answer's text in
// the pieces Respond gave it. The default answers "ok" with fixed usage.
type Provider struct {
	ID      string
	Respond func(ctx context.Context, r model.Request) (*Answer, error)
	// Egress is the egress class it declares, local_only when empty, so a
	// test can have calls redacted as a remote provider's would be.
	Egress string

	mu       sync.Mutex
	requests []model.Request
}

// Answer is a fixture's response: its text in the pieces it streams, its
// calls, its usage, Usage() when left zero, and the cache outcome it
// reports, none when nil. Pace, where set, is the pause before each piece.
type Answer struct {
	Pieces []string
	Calls  []model.ToolCall
	Usage  model.Usage
	Cache  *model.Cache
	Pace   time.Duration
}

// New is a fixture provider that answers "ok".
func New() *Provider { return &Provider{ID: "fixture"} }

// Describe says the fixture's egress class, local_only unless set.
func (p *Provider) Describe() model.Descriptor {
	egress := p.Egress
	if egress == "" {
		egress = "local_only"
	}
	return model.Descriptor{ID: p.ID, EgressClass: egress}
}

// Requests are the requests the provider was given, in order.
func (p *Provider) Requests() []model.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]model.Request(nil), p.requests...)
}

// Call records r and answers it with Respond.
func (p *Provider) Call(ctx context.Context, r model.Request, stream func(model.Delta)) (*model.Response, error) {
	p.mu.Lock()
	p.requests = append(p.requests, r)
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := &Answer{Pieces: []string{"ok"}}
	if p.Respond != nil {
		var err error
		if a, err = p.Respond(ctx, r); err != nil {
			return nil, err
		}
	}
	out := &model.Response{Calls: a.Calls, Usage: a.Usage, Cache: a.Cache, ModelID: r.Model}
	if out.Usage.Normalization == "" {
		out.Usage = Usage()
	}
	for _, piece := range a.Pieces {
		if a.Pace > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(a.Pace):
			}
		}
		out.Text += piece
		if stream != nil {
			stream(model.Delta{Text: piece})
		}
	}
	return out, nil
}

// Text answers with text alone, streamed in the given pieces.
func Text(pieces ...string) *Answer { return &Answer{Pieces: pieces} }

// Calling answers with tool calls alone.
func Calling(calls ...model.ToolCall) *Answer { return &Answer{Calls: calls} }

// Usage is the fixed usage the fixture reports.
func Usage() model.Usage {
	return model.Usage{
		InputTokens: model.Reported(100), CacheReadTokens: model.Reported(0), CacheWriteTokens: model.Reported(0),
		OutputTokens: model.Reported(20), ReasoningTokens: model.Unavailable,
		Normalization: model.NormalizationNativeDisjoint,
	}
}
