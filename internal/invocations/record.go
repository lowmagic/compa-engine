package invocations

import (
	"encoding/json"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
)

// Record is the provenance of one model call, recorded whether it
// succeeded or not. Its JSON names are those of the records stored before
// it, so they still read; fields only the old design wrote are ignored.
type Record struct {
	InvocationID string `json:"invocation_id"`
	// InvestigationID keys the record to its case: it holds the case's
	// ID, under the name the invocations table's column keeps.
	InvestigationID string `json:"investigation_id"`
	CorrelationID   string `json:"correlation_id"`
	TaskType        string `json:"task_type"`
	Trigger         string `json:"trigger,omitempty"`

	HarnessID      string        `json:"harness_id,omitempty"`
	AdapterVersion string        `json:"adapter_version,omitempty"`
	Capabilities   *Capabilities `json:"capabilities,omitempty"`

	Model ModelIdentity `json:"model"`
	// ModelProfile is the profile the engine resolved the model from.
	ModelProfile *ModelProfile `json:"model_profile,omitempty"`

	RedactionPolicyVersion string `json:"redaction_policy_version,omitempty"`

	Request     json.RawMessage `json:"request,omitempty"`
	RawResponse []byte          `json:"raw_response,omitempty"`
	// Thinking is the model's thought summary, kept where the raw response
	// is kept and redacted as it is; absent where the provider returned
	// none.
	Thinking string `json:"thinking,omitempty"`

	// ConversationID and Turn place the call in its conversation, its
	// turn counted from 1; RebuiltFrom is the conversation this one
	// replaced.
	ConversationID string `json:"conversation_id,omitempty"`
	Turn           int    `json:"turn,omitempty"`
	RebuiltFrom    string `json:"rebuilt_from,omitempty"`
	// ResentFrom is the call a resent call sent again: its request, sent
	// once more, apart from any conversation.
	ResentFrom string `json:"resent_from,omitempty"`

	// ToolCalls are the function calls the call answered with.
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
	TurnCount model.Count `json:"turn_count"`

	Usage model.Usage  `json:"usage"`
	Cost  pricing.Cost `json:"cost"`
	// Cache is what the provider did with a cache of its own making for
	// the call — made, reused, refused with the provider's error, or none
	// and why — where it makes one.
	Cache *model.Cache `json:"cache,omitempty"`

	QueuedAt    time.Time     `json:"queued_at"`
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt time.Time     `json:"completed_at"`
	Duration    time.Duration `json:"duration"`

	// Override is the engineer's override the request went out under,
	// where its case's barred-account check could not be made.
	Override string   `json:"override,omitempty"`
	Outcome  string   `json:"outcome"`
	Problems []string `json:"problems,omitempty"`
}

// Capabilities are what a record says of the call's provider: the egress
// class its request was redacted for, and how its usage was reported.
type Capabilities struct {
	UsageReporting string `json:"usage_reporting"`
	UsageScope     string `json:"usage_scope"`
	EgressClass    string `json:"egress_class"`
}

// ScopePerInvocation is usage attributed to the one call it was reported
// for.
const ScopePerInvocation = "per_invocation"

// ModelIdentity says which model produced a result and how the engine
// knows: engine_selected, harness_reported or unknown.
type ModelIdentity struct {
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"model_id,omitempty"`
	Source   string `json:"source"`
	// Effort is what was sent with the model, chosen by its profile:
	// minimal, low, medium or high, or empty for a model that takes none.
	// Stored under its first name, thinking_level.
	Effort string `json:"thinking_level,omitempty"`
}

// EffortShown names the effort a model ran with, as a result shows it
// beside the model.
func (m ModelIdentity) EffortShown() string { return EffortShown(m.Effort) }

// EffortShown names an effort as a result shows it beside its model:
// "effort high", or "no effort set" where none was sent.
func EffortShown(effort string) string {
	if effort == "" {
		return "no effort set"
	}
	return "effort " + effort
}

// ModelProfile names a model profile class and where the choice of class
// came from: configuration, expert or default; and the profile's effort,
// empty for a model that takes none. Stored under its first name,
// thinking.
type ModelProfile struct {
	Class  string `json:"class"`
	Source string `json:"source"`
	Effort string `json:"thinking,omitempty"`
}

// ToolCall is one function call a model answered with: its ID, the tool
// it names and its arguments.
type ToolCall struct {
	ID     string         `json:"id"`
	Test   string         `json:"test"`
	Params map[string]any `json:"params"`
}
