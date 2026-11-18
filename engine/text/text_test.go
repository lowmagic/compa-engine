package text

import "testing"

func TestSafeEscapesControlCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\n":               "plain\ttext\n",
		"title\x1b]0;owned\x07 rest":  `title\x1b]0;owned\x07 rest`,
		"clear\x1b[2J":                `clear\x1b[2J`,
		"c1 \u009b31m":                `c1 \x9b31m`,
		"bad utf-8 \xff":              `bad utf-8 \xfffd`,
		"carriage\rreturn overwrites": `carriage\x0dreturn overwrites`,
	} {
		if got := Safe(in); got != want {
			t.Errorf("Safe(%q) = %q, want %q", in, got, want)
		}
	}
}

// A count names its noun in the plural as English does: an s, or ies for
// a consonant and y.
func TestPlural(t *testing.T) {
	for n, want := range map[string]string{"entry": "10 entries", "queued evidence summary": "10 queued evidence summaries", "key": "10 keys", "call": "10 calls", "day": "10 days"} {
		if got := Plural(10, n); got != want {
			t.Errorf("Plural(10, %q) = %q, want %q", n, got, want)
		}
	}
	if got := Plural(1, "entry"); got != "1 entry" {
		t.Errorf("Plural(1, entry) = %q", got)
	}
}
