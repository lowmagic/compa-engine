package invocations_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/lowmagic/compa-engine/internal/invocations"
	"github.com/lowmagic/compa-engine/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestRenderGolden renders usage as docs/MODEL_API.md §Cost has it:
// priced tokens with their cost, reported and estimated apart, unpriced
// tokens with none, currencies unconverted, and what each sum leaves out.
func TestRenderGolden(t *testing.T) {
	u := usage(r(80000), r(1000), r(0), r(1000), r(0))
	small := usage(r(14000), r(0), r(0), r(1000), model.Unavailable)
	est := usage(model.Estimated(1200, "chars-4"), model.Estimated(0, "chars-4"), model.Estimated(0, "chars-4"), model.Estimated(300, "chars-4"), model.Unavailable)
	none := usage(model.Unavailable, model.Unavailable, model.Unavailable, model.Unavailable, model.Unavailable)
	cases := []struct {
		name string
		recs []invocations.Record
	}{
		{"nothing", nil},
		{"all priced", []invocations.Record{record("i1", "analysis.diagnose", "relay", "gemini-3.8-flash", u, cost(0.41, "USD", model.FidelityReported), 0)}},
		{"priced and unpriced", []invocations.Record{
			record("i1", "analysis.diagnose", "relay", "gemini-3.8-flash", u, cost(0.41, "USD", model.FidelityReported), 0),
			record("i2", "analysis.diagnose", "relay", "gemini-3.8-flash", small, noCost, 1),
		}},
		{"estimated apart from reported", []invocations.Record{
			record("i1", "analysis.diagnose", "relay", "gemini-3.8-flash", u, cost(0.41, "USD", model.FidelityReported), 0),
			record("i2", "analysis.diagnose", "relay", "gemini-3.8-flash", u, cost(0.0208, "USD", model.FidelityEstimated), 1),
		}},
		{"nothing priced", []invocations.Record{record("i1", "analysis.diagnose", "relay", "gemini-3.8-flash", small, noCost, 0)}},
		{"currencies apart", []invocations.Record{
			record("i1", "analysis.diagnose", "gemini-cli", "gemini-3.8-flash", u, cost(1.5, "USD", model.FidelityReported), 0),
			record("i2", "analysis.diagnose", "relay", "claude-haiku-4-5", u, cost(0.05, "EUR", model.FidelityReported), 1),
		}},
		{"estimated counts and unknowns", []invocations.Record{
			record("i1", "analysis.diagnose", "relay", "gemini-3.8-flash", small, noCost, 0),
			record("i2", "analysis.diagnose", "pi", "gemini-3.8-flash", est, noCost, 1),
			record("i3", "analysis.diagnose", "pi", "gemini-3.8-flash", none, noCost, 2),
		}},
		{"nothing measured", []invocations.Record{record("i1", "analysis.diagnose", "pi", "gemini-3.8-flash", none, noCost, 0)}},
	}
	var b strings.Builder
	for _, c := range cases {
		a := invocations.Of(c.recs...)
		b.WriteString(c.name + ": " + a.Totals() + "\n  compact: " + a.Compact() + "\n  " + strings.Join(a.Completeness(), "; ") + "\n")
		for _, rec := range c.recs {
			b.WriteString("  " + rec.InvocationID + ": " + invocations.Fidelity(rec.Usage) + "; " + invocations.Breakdown(rec.Usage) + "\n")
		}
	}
	const golden = "testdata/render.golden"
	if *update {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("rendering differs from %s (go test -update to accept):\n%s", golden, b.String())
	}
}

func TestShort(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1.0k", 412600: "412.6k", 1234567: "1.2M"} {
		if got := invocations.Short(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 82000: "82,000", 1234567: "1,234,567", -4500: "-4,500"} {
		if got := invocations.Thousands(n); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
