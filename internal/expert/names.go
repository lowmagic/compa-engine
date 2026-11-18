package expert

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// forbiddenTokens are names of reasoning harnesses, model providers and
// model families. A manifest naming one would tie the expert to a harness or
// model, which the Expert API forbids; the engine resolves requirements to a
// harness and model instead.
var forbiddenTokens = map[string]bool{
	// Reasoning harnesses.
	"pi": true, "codex": true, "opencode": true, "aider": true,
	// Model providers and platforms.
	"anthropic": true, "openai": true, "vertex": true, "bedrock": true, "ollama": true, "azure": true,
	// Model families.
	"claude": true, "gemini": true, "gpt": true, "opus": true, "sonnet": true, "haiku": true,
	"llama": true, "mistral": true, "mixtral": true, "granite": true, "qwen": true, "deepseek": true,
}

type nameHit struct{ path, token string }

// namesHarnessOrModel walks every string value and key in a decoded
// manifest and reports tokens naming a harness, provider or model. Tokens
// are split on anything other than letters and digits, then on letter/digit
// boundaries, so "gpt4o", "claude-haiku-5" and "Gemini_3" are all caught,
// while a word merely containing one ("pipeline", "api") is not.
func namesHarnessOrModel(file string, doc any) []nameHit {
	var hits []nameHit
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case string:
			for _, tok := range tokens(x) {
				if forbiddenTokens[tok] {
					hits = append(hits, nameHit{path, tok})
				}
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(path+"."+k, k)
				walk(path+"."+k, x[k])
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		}
	}
	walk(file, doc)
	return hits
}

func tokens(s string) []string {
	var out []string
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		start := 0
		for i := 1; i <= len(word); i++ {
			if i == len(word) || unicode.IsDigit(rune(word[i])) != unicode.IsDigit(rune(word[i-1])) {
				out = append(out, word[start:i])
				start = i
			}
		}
	}
	return out
}
