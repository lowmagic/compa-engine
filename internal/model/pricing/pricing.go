// Package pricing estimates what a model call cost from a price table the
// engine holds, as no provider reports a cost of its own. Its figures
// are estimates: list prices retrieved by hand, which lag real rates and
// ignore discounts. They are labelled estimated with the table's version
// and are never combined with reported costs.
package pricing

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lowmagic/compa-engine/internal/model"
)

// longContextThreshold is the prompt size above which a model's long
// context rates apply to the whole call, for both Gemini and Claude.
const longContextThreshold = 200_000

//go:embed vertex.yaml
var vertexYAML []byte

type Table struct {
	Version   string  `yaml:"version"`
	Source    string  `yaml:"source"`
	Retrieved string  `yaml:"retrieved"`
	Currency  string  `yaml:"currency"`
	Models    []Model `yaml:"models"`
}

// Model is one model's prices, in effect over successive periods.
type Model struct {
	Provider string  `yaml:"provider"`
	Model    string  `yaml:"model"`
	Prices   []Price `yaml:"prices"`
}

// Price is the rates in effect from From (inclusive) until Until
// (exclusive), dates in UTC; either may be empty.
type Price struct {
	From        string `yaml:"from"`
	Until       string `yaml:"until"`
	Rates       Rates  `yaml:"rates"`
	LongContext *Rates `yaml:"longContext"`
}

// Rates are per million tokens. A nil rate means the category is not
// billed per token.
type Rates struct {
	Input      *float64 `yaml:"input"`
	CacheRead  *float64 `yaml:"cacheRead"`
	CacheWrite *float64 `yaml:"cacheWrite"`
	Output     *float64 `yaml:"output"`
	Reasoning  *float64 `yaml:"reasoning"`
	// CacheStorage is an explicit cache's storage, per million tokens held
	// per hour, which no invocation's usage reports: it is charged for the
	// cache's lifetime whether or not a call reads it.
	CacheStorage *float64 `yaml:"cacheStorage"`
}

// Cost is a call's cost: the engine's estimate from a price table,
// labelled estimated with the table's version, or unavailable. Stored in
// invocation records, whose older entries may hold a reported cost.
type Cost struct {
	Amount   *float64 `json:"amount,omitempty"`
	Currency string   `json:"currency,omitempty"`
	Fidelity string   `json:"fidelity"` // reported, estimated or unavailable
	// Estimator is the price table an estimated cost came from.
	Estimator  string             `json:"estimator,omitempty"`
	ByCategory map[string]float64 `json:"by_category,omitempty"`
}

// Unavailable is the cost of a call nothing priced.
var Unavailable = Cost{Fidelity: "unavailable"}

func Vertex() *Table {
	var t Table
	if err := yaml.Unmarshal(vertexYAML, &t); err != nil {
		panic(fmt.Sprintf("pricing: bundled table: %v", err))
	}
	return &t
}

// CacheStorage is a model's explicit cache storage rate at at, per million
// tokens per hour, and false when the table lists none.
func (t *Table) CacheStorage(provider, modelID string, at time.Time) (float64, bool) {
	p, ok := t.price(provider, modelID, at)
	if !ok || p.Rates.CacheStorage == nil {
		return 0, false
	}
	return *p.Rates.CacheStorage, true
}

// versionSuffix is what may follow a priced model ID in the ID a provider
// reports: a release number such as -001 or a date such as -20251001.
var versionSuffix = regexp.MustCompile(`^[-@][0-9]+$`)

// Estimate prices one call's usage for the model it used, the provider's
// model ID, at the rates in effect at at. It returns false when the model is not in the
// table, or a category the model bills is unavailable, or a category it
// does not bill carries tokens: then nothing is estimated.
func (t *Table) Estimate(provider, modelID string, u model.Usage, at time.Time) (Cost, bool) {
	p, ok := t.price(provider, modelID, at)
	if !ok {
		return Cost{}, false
	}
	r := p.Rates
	if prompt := u.InputTokens.Value + u.CacheReadTokens.Value + u.CacheWriteTokens.Value; prompt > longContextThreshold && p.LongContext != nil {
		r = *p.LongContext
	}
	by := map[string]float64{}
	total := 0.0
	for _, c := range []struct {
		name  string
		count model.Count
		rate  *float64
	}{
		{"input_tokens", u.InputTokens, r.Input},
		{"cache_read_tokens", u.CacheReadTokens, r.CacheRead},
		{"cache_write_tokens", u.CacheWriteTokens, r.CacheWrite},
		{"output_tokens", u.OutputTokens, r.Output},
		{"reasoning_tokens", u.ReasoningTokens, r.Reasoning},
	} {
		switch {
		case c.rate == nil && c.count.Known && c.count.Value > 0:
			return Cost{}, false
		case c.rate == nil:
			continue
		case !c.count.Known || c.count.Fidelity != model.FidelityReported:
			return Cost{}, false
		}
		amount := float64(c.count.Value) * *c.rate / 1e6
		by[c.name] = amount
		total += amount
	}
	return Cost{Amount: &total, Currency: t.Currency, Fidelity: model.FidelityEstimated, Estimator: t.Version, ByCategory: by}, true
}

func (t *Table) price(provider, modelID string, at time.Time) (Price, bool) {
	day := at.UTC().Format(time.DateOnly)
	for _, e := range t.Models {
		if e.Provider != provider {
			continue
		}
		rest, ok := strings.CutPrefix(modelID, e.Model)
		if !ok || rest != "" && !versionSuffix.MatchString(rest) {
			continue
		}
		for _, p := range e.Prices {
			if (p.From == "" || day >= p.From) && (p.Until == "" || day < p.Until) {
				return p, true
			}
		}
	}
	return Price{}, false
}
