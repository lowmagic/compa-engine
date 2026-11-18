package invocations

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lowmagic/compa-engine/internal/model"
)

// Totals renders a group's tokens and cost as docs/MODEL_API.md §Cost
// has them: priced tokens with their cost, reported and estimated costs
// apart, then the tokens no cost covers, as
//
//	82,000 tokens ($0.41), 15,000 tokens (unpriced)
//
// When nothing is priced only tokens are shown. Currencies are never
// converted. The figures count reported tokens only; Completeness says
// what they leave out.
func (a Aggregate) Totals() string {
	if a.Invocations == 0 {
		return "no model calls"
	}
	c := a.Cost
	var parts []string
	if c.PricedTokens > 0 || len(c.Reported) > 0 {
		parts = append(parts, fmt.Sprintf("%s tokens (%s)", Thousands(c.PricedTokens), money(c.Reported)))
	}
	if c.EstimatedPricedTokens > 0 || len(c.Estimated) > 0 {
		parts = append(parts, fmt.Sprintf("%s tokens (%s estimated)", Thousands(c.EstimatedPricedTokens), money(c.Estimated)))
	}
	switch {
	case len(parts) == 0 && c.UnpricedTokens == 0 && a.unknown():
		parts = append(parts, "tokens unknown")
	case len(parts) == 0:
		parts = append(parts, Thousands(c.UnpricedTokens)+" tokens")
	case c.UnpricedTokens > 0:
		parts = append(parts, Thousands(c.UnpricedTokens)+" tokens (unpriced)")
	}
	return strings.Join(parts, ", ")
}

// Compact is Totals in the few cells a frame's border has, from the same
// figures: "412.6k tok · $0.39 est", the tokens no cost covers as a
// further figure, "+ 15.0k tok unpriced". Empty when there were no calls.
func (a Aggregate) Compact() string {
	if a.Invocations == 0 {
		return ""
	}
	c := a.Cost
	var parts []string
	if c.PricedTokens > 0 || len(c.Reported) > 0 {
		parts = append(parts, Short(c.PricedTokens)+" tok · "+money(c.Reported))
	}
	if c.EstimatedPricedTokens > 0 || len(c.Estimated) > 0 {
		parts = append(parts, Short(c.EstimatedPricedTokens)+" tok · "+money(c.Estimated)+" est")
	}
	switch {
	case len(parts) == 0 && c.UnpricedTokens == 0 && a.unknown():
		parts = append(parts, "tokens unknown")
	case len(parts) == 0:
		parts = append(parts, Short(c.UnpricedTokens)+" tok")
	case c.UnpricedTokens > 0:
		parts = append(parts, Short(c.UnpricedTokens)+" tok unpriced")
	}
	return strings.Join(parts, " + ")
}

// Short writes n in thousands or millions to one decimal: 412.6k, 1.2M.
func Short(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// unknown reports whether no invocation in the group measured any token
// category.
func (a Aggregate) unknown() bool {
	for _, cat := range Categories {
		if a.Tokens[cat].Unavailable < a.Invocations {
			return false
		}
	}
	return true
}

// Completeness says what a group's figures rest on: how many invocations,
// the categories unknown for some of them, which make a sum a floor,
// estimated token counts kept apart, and the harnesses and models it
// mixes.
func (a Aggregate) Completeness() []string {
	out := []string{plural(a.Invocations, "invocation")}
	var floor, estimated []string
	for _, cat := range Categories {
		s := a.Tokens[cat]
		if s.Unavailable > 0 {
			floor = append(floor, fmt.Sprintf("%d without %s", s.Unavailable, cat))
		}
		if s.Estimated > 0 {
			estimated = append(estimated, fmt.Sprintf("%s %s", Thousands(s.Estimated), cat))
		}
	}
	if len(floor) > 0 {
		out = append(out, "a floor: "+strings.Join(floor, ", "))
	}
	if len(estimated) > 0 {
		out = append(out, "estimated apart: "+strings.Join(estimated, ", "))
	}
	if len(a.Harnesses) > 1 {
		out = append(out, "mixes harnesses "+strings.Join(a.Harnesses, ", "))
	}
	if len(a.Models) > 1 {
		out = append(out, "mixes models "+strings.Join(a.Models, ", "))
	}
	return out
}

// Fidelity is how an invocation's token usage was measured: reported,
// estimated, or unavailable, naming the categories it lacks.
func Fidelity(u model.Usage) string {
	counts := []model.Count{u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens, u.ReasoningTokens}
	known, estimated := 0, ""
	var missing []string
	for i, c := range counts {
		switch {
		case !c.Known:
			missing = append(missing, Categories[i])
		case c.Fidelity == model.FidelityEstimated:
			known++
			estimated = c.EstimatorVersion
		default:
			known++
		}
	}
	if known == 0 {
		return "usage unavailable"
	}
	s := "usage reported"
	if estimated != "" {
		s = "usage estimated (" + estimated + ")"
	}
	if len(missing) > 0 {
		s += ", " + strings.Join(missing, ", ") + " unavailable"
	}
	return s
}

// money renders amounts by currency, never converted: dollars as $, any
// other currency by its code; four decimals below a tenth, else two.
func money(by map[string]float64) string {
	if len(by) == 0 {
		return "no amount"
	}
	cur := make([]string, 0, len(by))
	for c := range by {
		cur = append(cur, c)
	}
	sort.Strings(cur)
	var parts []string
	for _, c := range cur {
		v := by[c]
		n := strconv.FormatFloat(v, 'f', 2, 64)
		if v < 0.1 {
			n = strconv.FormatFloat(v, 'f', 4, 64)
		}
		if c == "USD" {
			parts = append(parts, "$"+n)
		} else {
			parts = append(parts, strings.TrimSpace(n+" "+c))
		}
	}
	return strings.Join(parts, " + ")
}

// Thousands writes n with comma separators.
func Thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// Breakdown is an invocation's tokens by category, an unknown one said so:
// "input 52,100 · cache read 0 · output 2,310 · reasoning 35,200".
func Breakdown(u model.Usage) string {
	var parts []string
	for _, c := range []struct {
		name  string
		count model.Count
	}{
		{"input", u.InputTokens}, {"cache read", u.CacheReadTokens}, {"cache write", u.CacheWriteTokens},
		{"output", u.OutputTokens}, {"reasoning", u.ReasoningTokens},
	} {
		switch {
		case !c.count.Known:
			parts = append(parts, c.name+" unknown")
		case c.count.Fidelity == model.FidelityEstimated:
			parts = append(parts, c.name+" ~"+Thousands(c.count.Value))
		default:
			parts = append(parts, c.name+" "+Thousands(c.count.Value))
		}
	}
	return strings.Join(parts, " · ")
}
