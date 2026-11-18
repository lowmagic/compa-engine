package model

import (
	"encoding/json"
	"errors"
	"strconv"
	"unicode"
)

// How a count was measured.
const (
	FidelityReported  = "reported"
	FidelityEstimated = "estimated"
)

// Count is a measured count or unavailable. Unavailable is a correct value
// and is never the same as zero: a measured zero is Known with Value 0.
type Count struct {
	Value int64
	Known bool
	// Fidelity is reported or estimated; an estimated value names its
	// estimator.
	Fidelity         string
	EstimatorVersion string
}

// Reported is a count the provider reported.
func Reported(v int64) Count { return Count{Value: v, Known: true, Fidelity: FidelityReported} }

// Estimated is a count the engine estimated, naming its estimator.
func Estimated(v int64, estimator string) Count {
	return Count{Value: v, Known: true, Fidelity: FidelityEstimated, EstimatorVersion: estimator}
}

// Unavailable is a count nobody reported: never zero.
var Unavailable = Count{}

type countJSON struct {
	Value            *int64 `json:"value"`
	Fidelity         string `json:"fidelity,omitempty"`
	EstimatorVersion string `json:"estimator_version,omitempty"`
}

// MarshalJSON writes "unavailable" for an unknown count, never 0.
func (c Count) MarshalJSON() ([]byte, error) {
	if !c.Known {
		return []byte(`"unavailable"`), nil
	}
	v := c.Value
	return json.Marshal(countJSON{Value: &v, Fidelity: c.Fidelity, EstimatorVersion: c.EstimatorVersion})
}

// UnmarshalJSON reads "unavailable" or a count with its fidelity.
func (c *Count) UnmarshalJSON(b []byte) error {
	if string(b) == `"unavailable"` {
		*c = Unavailable
		return nil
	}
	var j countJSON
	if err := json.Unmarshal(b, &j); err != nil || j.Value == nil {
		return errors.New(`count: want "unavailable" or {value, fidelity}`)
	}
	*c = Count{Value: *j.Value, Known: true, Fidelity: j.Fidelity, EstimatorVersion: j.EstimatorVersion}
	return nil
}

func (c Count) String() string {
	if !c.Known {
		return "unavailable"
	}
	s := strconv.FormatInt(c.Value, 10)
	if c.Fidelity == FidelityEstimated {
		s += " (estimated)"
	}
	return s
}

// How a provider made its usage disjoint (docs/MODEL_API.md §Normalization).
const (
	NormalizationNativeDisjoint = "native_disjoint"
	NormalizationDerived        = "derived"
	NormalizationAmbiguous      = "ambiguous"
)

// Usage holds one call's disjoint token categories, so no token is counted
// twice. No total is stored; it is derived when aggregating.
type Usage struct {
	InputTokens      Count  `json:"input_tokens"`
	CacheReadTokens  Count  `json:"cache_read_tokens"`
	CacheWriteTokens Count  `json:"cache_write_tokens"`
	OutputTokens     Count  `json:"output_tokens"`
	ReasoningTokens  Count  `json:"reasoning_tokens"`
	Normalization    string `json:"normalization"`
	// Ambiguous lists the categories whose composition the provider could
	// not determine; they are unavailable.
	Ambiguous []string `json:"ambiguous,omitempty"`
}

// EstimatorVersion names the token estimate, recorded wherever it is used.
const EstimatorVersion = "words1.3+symbols-v1"

// EstimateTokens estimates the tokens in s without a tokenizer: 1.3 per
// word, a word being a run of letters and digits, plus one per other
// non-space character. Neither provider's count endpoint is usable on our
// project, so nothing is counted before a call; the estimate came within
// 6% of both providers' reported counts on a diagnose call.
func EstimateTokens(s string) int64 {
	var words, symbols int64
	in := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if !in {
				words++
			}
			in = true
		case unicode.IsSpace(r):
			in = false
		default:
			symbols++
			in = false
		}
	}
	return (words*13+9)/10 + symbols
}
