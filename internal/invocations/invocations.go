// Package invocations keeps a case's invocation records in its case store
// and aggregates their usage. Aggregation is a read-only query over the
// records, with no store of its own (docs/MODEL_API.md §Usage
// accounting): it never presents a partial sum as a total, keeps estimated
// values apart from reported ones, and never imputes a cost.
package invocations

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lowmagic/compa-engine/internal/model"
	"github.com/lowmagic/compa-engine/internal/model/pricing"
	"github.com/lowmagic/compa-engine/internal/redact"
)

var ErrNotFound = errors.New("invocations: not found")

// Execer is what writing a record needs: the store's database, or a
// transaction on it.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// Queryer is what reading records needs: the store's database, or a
// transaction on it.
type Queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Put stores an invocation record. Storing the same invocation again
// changes nothing. Retained request and response bodies are redacted
// first, by the policy for the egress class recorded, or the secrets
// policy where none is.
func Put(db Execer, rec Record) error {
	policy := redact.For("")
	if rec.Capabilities != nil {
		policy = redact.For(rec.Capabilities.EgressClass)
	}
	var err error
	if rec.Request, err = policy.JSON(rec.Request); err != nil {
		return err
	}
	if rec.RawResponse, err = policy.JSON(rec.RawResponse); err != nil {
		return err
	}
	rec.Thinking = policy.String(rec.Thinking)
	j, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("invocations: %w", err)
	}
	// investigation_id holds the case's ID; unit_id, step_id and
	// session_scope are the old design's, kept with their table and
	// written as a call outside any unit.
	_, err = db.Exec(`INSERT INTO invocations
		(id, investigation_id, unit_id, step_id, trigger_ref, task_type, harness_id, provider, model_id, session_scope, outcome, started_at, record)
		VALUES (?, ?, '', '', ?, ?, ?, ?, ?, 'call', ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		rec.InvocationID, rec.InvestigationID, rec.Trigger, rec.TaskType, rec.HarnessID,
		rec.Model.Provider, rec.Model.ModelID, rec.Outcome, rec.StartedAt.UTC().Format(time.RFC3339Nano), string(j))
	if err != nil {
		return fmt.Errorf("invocations: %w", err)
	}
	return nil
}

// Get reads one invocation record by its ID.
func Get(db Queryer, id string) (Record, error) {
	var rec Record
	var j string
	err := db.QueryRow(`SELECT record FROM invocations WHERE id = ?`, id).Scan(&j)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return rec, fmt.Errorf("invocations: %w", err)
	}
	if err := json.Unmarshal([]byte(j), &rec); err != nil {
		return rec, fmt.Errorf("invocations: %s: %w", id, err)
	}
	return rec, nil
}

// Dimension is what usage can be grouped by.
type Dimension string

// The dimensions usage is grouped by: the case, the task type, the
// harness, the model and the conversation.
const (
	// ByCase groups by case, whose ID the records hold in
	// investigation_id.
	ByCase     Dimension = "case"
	ByTaskType Dimension = "task_type"
	ByHarness  Dimension = "harness"
	ByModel    Dimension = "model"
	// ByConversation groups the companion's calls by conversation, a
	// consult's being its own.
	ByConversation Dimension = "conversation"
)

var columns = map[Dimension]string{
	ByCase:     "investigation_id",
	ByTaskType: "task_type", ByHarness: "harness_id",
	ByModel:        "CASE WHEN model_id = '' THEN 'unknown' ELSE provider || '/' || model_id END",
	ByConversation: "COALESCE(json_extract(record, '$.conversation_id'), '')",
}

// Sum is one category's sum over a group, reported and estimated values
// apart, with the invocations that had no value counted. A sum with any
// unavailable is a floor, not a total.
type Sum struct {
	Reported    int64
	Estimated   int64
	Unavailable int
}

// Floor reports whether the sum leaves out invocations whose value is
// unknown.
func (s Sum) Floor() bool { return s.Unavailable > 0 }

func (s *Sum) add(c model.Count) {
	switch {
	case !c.Known:
		s.Unavailable++
	case c.Fidelity == model.FidelityEstimated:
		s.Estimated += c.Value
	default:
		s.Reported += c.Value
	}
}

// Cost is a group's cost: reported and estimated amounts apart, each by
// currency and never converted, beside the tokens they priced and the
// tokens no cost covers.
type Cost struct {
	Reported  map[string]float64
	Estimated map[string]float64
	// Tokens are reported token counts: of invocations with a reported
	// cost, with an estimated one, and with none.
	PricedTokens          int64
	EstimatedPricedTokens int64
	UnpricedTokens        int64
}

type Aggregate struct {
	Key         map[Dimension]string
	Invocations int
	Tokens      map[string]*Sum
	Cost        Cost
	// Harnesses and Models are the distinct harnesses and models in the
	// group, so a group mixing them says so.
	Harnesses []string
	Models    []string
}

// Categories are the token categories, in the order they are reported.
var Categories = []string{"input_tokens", "cache_read_tokens", "cache_write_tokens", "output_tokens", "reasoning_tokens"}

// AggregateUsage sums the usage of a case's invocations, grouped by the
// dimensions given, in group order. The records hold the case's ID in
// investigation_id, the column's name from before.
func AggregateUsage(db Queryer, caseID string, by ...Dimension) ([]Aggregate, error) {
	return aggregateUsage(db, caseID, nil, by...)
}

// TotalTaskUsage sums the case-keyed invocations of the tasks given as a
// single group, including calls in consults and conversations replaced by
// compaction.
func TotalTaskUsage(db Queryer, caseID string, taskTypes ...string) (Aggregate, error) {
	groups, err := aggregateUsage(db, caseID, taskTypes)
	if err != nil {
		return Aggregate{}, err
	}
	if len(groups) == 0 {
		return Of(), nil
	}
	return groups[0], nil
}

func aggregateUsage(db Queryer, caseID string, taskTypes []string, by ...Dimension) ([]Aggregate, error) {
	sel := []string{`json_object(
		'harness_id', json_extract(record, '$.harness_id'),
		'model', json_extract(record, '$.model'),
		'usage', json_extract(record, '$.usage'),
		'cost', json_extract(record, '$.cost'))`}
	for _, d := range by {
		c, ok := columns[d]
		if !ok {
			return nil, fmt.Errorf("invocations: unknown dimension %q", d)
		}
		sel = append(sel, c)
	}
	where, args := "investigation_id = ?", []any{caseID}
	if len(taskTypes) > 0 {
		where += " AND task_type IN (?" + strings.Repeat(", ?", len(taskTypes)-1) + ")"
		for _, t := range taskTypes {
			args = append(args, t)
		}
	}
	rows, err := db.Query(`SELECT `+strings.Join(sel, ", ")+` FROM invocations WHERE `+where+` ORDER BY started_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("invocations: %w", err)
	}
	defer rows.Close()
	groups := map[string]*Aggregate{}
	var order []string
	for rows.Next() {
		var j string
		vals := make([]string, len(by))
		dest := []any{&j}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("invocations: %w", err)
		}
		var rec usageFields
		if err := json.Unmarshal([]byte(j), &rec); err != nil {
			return nil, fmt.Errorf("invocations: %w", err)
		}
		k := strings.Join(vals, "\x00")
		a := groups[k]
		if a == nil {
			a = newAggregate(by, vals)
			groups[k] = a
			order = append(order, k)
		}
		a.add(rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("invocations: %w", err)
	}
	sort.Strings(order)
	out := make([]Aggregate, 0, len(order))
	for _, k := range order {
		a := groups[k]
		sort.Strings(a.Harnesses)
		sort.Strings(a.Models)
		out = append(out, *a)
	}
	return out, nil
}

// Of sums the usage of the records given, as one group: a step's
// invocations, say.
func Of(recs ...Record) Aggregate {
	a := newAggregate(nil, nil)
	for _, rec := range recs {
		a.add(usageFields{HarnessID: rec.HarnessID, Model: rec.Model, Usage: rec.Usage, Cost: rec.Cost})
	}
	sort.Strings(a.Harnesses)
	sort.Strings(a.Models)
	return *a
}

func newAggregate(by []Dimension, vals []string) *Aggregate {
	a := &Aggregate{Key: map[Dimension]string{}, Tokens: map[string]*Sum{},
		Cost: Cost{Reported: map[string]float64{}, Estimated: map[string]float64{}}}
	for i, d := range by {
		a.Key[d] = vals[i]
	}
	for _, c := range Categories {
		a.Tokens[c] = &Sum{}
	}
	return a
}

// usageFields are the small projection aggregation reads, leaving retained
// request and response bodies in the database.
type usageFields struct {
	HarnessID string        `json:"harness_id"`
	Model     ModelIdentity `json:"model"`
	Usage     model.Usage   `json:"usage"`
	Cost      pricing.Cost  `json:"cost"`
}

func (a *Aggregate) add(rec usageFields) {
	a.Invocations++
	u := rec.Usage
	var tokens int64
	for _, c := range []struct {
		name  string
		count model.Count
	}{
		{"input_tokens", u.InputTokens}, {"cache_read_tokens", u.CacheReadTokens}, {"cache_write_tokens", u.CacheWriteTokens},
		{"output_tokens", u.OutputTokens}, {"reasoning_tokens", u.ReasoningTokens},
	} {
		a.Tokens[c.name].add(c.count)
		if c.count.Known && c.count.Fidelity != model.FidelityEstimated {
			tokens += c.count.Value
		}
	}
	switch c := rec.Cost; {
	case c.Amount != nil && c.Fidelity == model.FidelityReported:
		a.Cost.Reported[c.Currency] += *c.Amount
		a.Cost.PricedTokens += tokens
	case c.Amount != nil && c.Fidelity == model.FidelityEstimated:
		a.Cost.Estimated[c.Currency] += *c.Amount
		a.Cost.EstimatedPricedTokens += tokens
	default:
		a.Cost.UnpricedTokens += tokens
	}
	a.Harnesses = addDistinct(a.Harnesses, rec.HarnessID)
	model := "unknown"
	if rec.Model.ModelID != "" {
		model = rec.Model.Provider + "/" + rec.Model.ModelID
	}
	a.Models = addDistinct(a.Models, model)
}

func addDistinct(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
