package model

import "sort"

// Keywords lists the JSON Schema keywords a schema uses, sorted, so a
// provider's test can hold the tools' parameter schemas to the keywords a
// live call accepted: a provider refuses a whole schema over one keyword
// it does not support. Property names are not keywords.
func Keywords(schema map[string]any) []string {
	seen := map[string]bool{}
	var walk func(s map[string]any)
	walk = func(s map[string]any) {
		for k, v := range s {
			seen[k] = true
			switch k {
			case "properties", "$defs", "definitions", "patternProperties":
				if m, ok := v.(map[string]any); ok {
					for _, sub := range m {
						if sm, ok := sub.(map[string]any); ok {
							walk(sm)
						}
					}
				}
			case "items", "additionalProperties", "not":
				if sm, ok := v.(map[string]any); ok {
					walk(sm)
				}
			case "anyOf", "oneOf", "allOf", "prefixItems":
				if l, ok := v.([]any); ok {
					for _, sub := range l {
						if sm, ok := sub.(map[string]any); ok {
							walk(sm)
						}
					}
				}
			}
		}
	}
	walk(schema)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
