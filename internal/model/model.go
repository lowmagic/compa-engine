// Package model is the engine's model layer: one call to a model provider
// with a conversation and its tool declarations, answered with free text
// and tool calls, streamed as they arrive. A provider holds no state
// between calls but its own caches, runs no tool and retries nothing
// (docs/MODEL_API.md).
package model

import (
	"context"
	"errors"
)

// Provider makes one model call.
type Provider interface {
	Describe() Descriptor
	// Call sends r and returns the model's answer. Each piece of text or
	// thought summary is passed to stream as it arrives, when stream is
	// not nil; the Response holds the whole answer either way.
	Call(ctx context.Context, r Request, stream func(Delta)) (*Response, error)
}

// Descriptor is what the engine needs to know of a provider before a call.
type Descriptor struct {
	ID string
	// EgressClass is where the call's content travels: local_only,
	// internal_network or external_provider. It selects the redaction
	// policy the engine applies before the call.
	EgressClass string
}

// Request is one model call: the conversation as the engine assembled it,
// most stable first.
type Request struct {
	Model string
	// Effort is the model's effort, sent where the model takes one; empty
	// sends none.
	Effort string
	// System is the companion prompt and the expert index.
	System string
	// Tools are declared on every call, in the same order, so a cached
	// prefix holding them stays valid.
	Tools    []Tool
	Messages []Message
	// MaxOutputTokens bounds the answer; zero leaves the provider's
	// default.
	MaxOutputTokens int
	// NoCalls asks the model to answer without calling a tool, where the
	// provider can say so without breaking its cached prefix. It is a
	// request, not a guarantee: the engine refuses any call made anyway.
	NoCalls bool
}

// Role says who wrote a message.
type Role string

// The two roles: the engine's side of the conversation, which carries the
// engineer's words, the case and tool results, and the model's.
const (
	RoleUser  Role = "user"
	RoleModel Role = "model"
)

// Message is one entry of the conversation. A user message carries the
// results of the model's last calls, if any, then its text; a model
// message carries its text, then its calls.
type Message struct {
	Role    Role
	Text    string
	Calls   []ToolCall
	Results []ToolResult
	// Break marks a point after which a provider may cache the prefix
	// ending here: the engine sends everything up to it unchanged in
	// every later call of the conversation.
	Break bool
}

// Tool is one function the model may ask for. Parameters is a JSON
// Schema object.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is one function call the model made.
type ToolCall struct {
	ID   string
	Name string
	Args map[string]any
	// Signature is provider data that must go back with the call, such
	// as Gemini's thought signature; nil where there is none.
	Signature []byte
}

// ToolResult is what the engine returns for one call: its output, or the
// reason it was refused or failed.
type ToolResult struct {
	CallID string
	Name   string
	Text   string
	// IsError marks a refusal or failure, so the model reads Text as why.
	IsError bool
}

// Delta is a piece of the answer as it streams.
type Delta struct {
	Text     string
	Thinking string
}

// Response is the model's whole answer.
type Response struct {
	Text  string
	Calls []ToolCall
	// Thinking is the model's thought summary, where the provider
	// returned one; debug output, never sent back to a model.
	Thinking string
	Usage    Usage
	// Cache is what the provider did with a cache of its own making for
	// this call; nil where it makes none.
	Cache *Cache
	// ModelID is the model the provider reports having used.
	ModelID string
	// Raw is the response as received, for debugging: the streamed events
	// in order, as a JSON array.
	Raw []byte
	// Cost is the call's cost as the provider reported it, with its
	// currency; nil where it reported none.
	Cost *Money
}

// Money is an amount in a currency, as a provider reported it.
type Money struct {
	Amount   float64
	Currency string
}

// Cache is what a provider that makes its own caches did for one call:
// made one and read it, read one made before, failed to make one, or
// neither, and why. A provider holds its caches itself; this is only the
// call's account of them, for its invocation record.
type Cache struct {
	Outcome string `json:"outcome"`
	// Name is the cache the call read; empty where it read none.
	Name string `json:"name,omitempty"`
	// Messages is how many of the conversation's messages the cache read
	// holds, and Tokens its system prompt and those messages by the
	// engine's estimator; nil where the call read none.
	Messages int    `json:"messages,omitempty"`
	Tokens   *Count `json:"tokens,omitempty"`
	// Error is the provider's error where making a cache failed, now or,
	// for the same prefix, before.
	Error string `json:"error,omitempty"`
}

// The outcomes of a Cache.
const (
	// CacheMade: a cache of the call's prefix was made, and read.
	CacheMade = "made"
	// CacheReused: a cache made before was read and none made.
	CacheReused = "reused"
	// CacheRefused: making a cache failed; one made before is read where
	// Name says so.
	CacheRefused = "refused"
	// CacheUnderMinimum: the prefix is under the provider's minimum by
	// the estimate; nothing is read from cache.
	CacheUnderMinimum = "under_minimum"
	// CacheNoBreak: no break precedes the newest message; nothing is read
	// from cache.
	CacheNoBreak = "no_break"
)

// ErrOutputLimit means the answer stopped at its output budget; the
// Response returned beside it holds what was written.
var ErrOutputLimit = errors.New("output reached its token limit")

// ErrStopped means the provider stopped generation for a reason other
// than the end of the answer or its output budget, such as a safety
// filter; the error names the reason.
var ErrStopped = errors.New("generation stopped")
