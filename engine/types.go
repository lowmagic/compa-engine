package engine

import (
	"github.com/lowmagic/compa-engine/internal/config"
	"github.com/lowmagic/compa-engine/internal/evidence"
	"github.com/lowmagic/compa-engine/internal/expert"
	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/redact"
	"github.com/lowmagic/compa-engine/internal/sources"
)

// The case's records pass through the engine's public API as aliases:
// they copy nothing, and Go's rule on internal packages restricts
// importing them, not using their types through this package.
type (
	// ModelIdentity is the model a result was made by, with its effort.
	ModelIdentity = invocations.ModelIdentity
	// UsageAggregate is the token use and cost of a set of invocations.
	UsageAggregate = invocations.Aggregate
	// SourceRecord is what the store records of a source, such as an
	// attachment the artifact source reported.
	SourceRecord = sources.Source
	// EvidenceItem is a stored piece of evidence.
	EvidenceItem = evidence.Item
	// ResolvedExpert is an expert with each catalog it declares resolved.
	ResolvedExpert = expert.Resolved
	// Intention is what a comment is drafted for: its label, purpose and
	// audience.
	Intention = config.Intention
	// RedactionPolicy is the redaction a request is made under, by its
	// egress class.
	RedactionPolicy = redact.Policy
	// PipelineStage is one stage of a command's pipeline: a primitive or
	// an argv command.
	PipelineStage = expert.Stage
	// InvocationRecord is what is recorded of one model call.
	InvocationRecord = invocations.Record
	// TokenUsage is a call's tokens by category, each a count or
	// unavailable.
	TokenUsage = model.Usage
	// TokenCount is one category's count, with how it was measured.
	TokenCount = model.Count
	// UsageDimension is what usage is aggregated by.
	UsageDimension = invocations.Dimension
)

// How a TokenCount was measured.
const (
	FidelityReported  = model.FidelityReported
	FidelityEstimated = model.FidelityEstimated
)

// What usage is aggregated by.
const (
	UsageByTaskType = invocations.ByTaskType
	UsageByModel    = invocations.ByModel
	// UsageByConversation groups the companion's calls by conversation, a
	// consult's being its own.
	UsageByConversation = invocations.ByConversation
)

// EffortShown names an effort as a result shows it beside its model:
// "effort high", or "no effort set" where none was sent.
func EffortShown(effort string) string { return invocations.EffortShown(effort) }

// Thousands writes n with comma separators.
func Thousands(n int64) string { return invocations.Thousands(n) }

// UsageOf sums the usage of the records given, as one group.
func UsageOf(recs ...InvocationRecord) UsageAggregate { return invocations.Of(recs...) }

// UsageFidelity is how an invocation's token usage was measured: reported,
// estimated, or unavailable, naming the categories it lacks.
func UsageFidelity(u TokenUsage) string { return invocations.Fidelity(u) }
