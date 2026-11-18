package engine

import (
	"encoding/json"
	"strings"

	"github.com/lowmagic/compa-engine/internal/routing"
)

// routingFacts are what a case contributes to routing before any artifact
// is read. Artifact-derived facts still need explicit domain producers.
func routingFacts(c Case) routing.Facts {
	return routing.Facts{Keys: append([]string(nil), c.RoutingKeys...), Components: append([]string(nil), c.Components...), Tags: append([]string(nil), c.Tags...)}
}

// caseFacts is what routing needs of the case, from its latest metadata:
// its routing keys.
func caseFacts(c *openCase) Case {
	latest, _, err := caseCurrent(c)
	if err != nil || latest[TypeCaseMetadata] == "" {
		return Case{ID: c.id}
	}
	it, err := c.ev.Get(latest[TypeCaseMetadata])
	if err != nil || it.Ref.Type != TypeCaseMetadata {
		return Case{ID: c.id}
	}
	kase := Case{ID: c.id}
	head, body, _ := strings.Cut(string(it.Content), "\n\n")
	kase.Description = strings.TrimSuffix(body, "\n")
	for _, l := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(l, ": ")
		switch k {
		case "subject":
			kase.Subject = v
		case "status":
			kase.Status = v
		case "severity":
			kase.Severity = v
		case "product":
			kase.Product = v
		case "product version":
			kase.ProductVersion = v
		case "routing components":
			// A list the metadata cannot hold as JSON is no list.
			_ = json.Unmarshal([]byte(v), &kase.Components)
		case "routing tags":
			_ = json.Unmarshal([]byte(v), &kase.Tags)
		case "account":
			kase.Account.Name = v
		}
		if v, ok := strings.CutPrefix(l, "routing keys: "); ok {
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					kase.RoutingKeys = append(kase.RoutingKeys, s)
				}
			}
		}
	}
	return kase
}
