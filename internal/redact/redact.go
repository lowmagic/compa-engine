// Package redact holds the engine's redaction policies, one per egress
// class, and applies them to what the engine sends and to retained payloads.
//
// The policy for content leaving the deployment host is secrets only:
// tokens, passwords, private keys, kubeconfig and
// pull-secret credentials and cloud credentials are replaced by a typed
// placeholder, so a model still sees that a secret was there. Hostnames,
// IP addresses, cluster and customer names are kept: the provider is one
// the deployment contracts with, and the host is the security envelope.
// Nothing is redacted for local_only.
//
// Secrets also hide in encoded and escaped forms:
// a MachineConfig carries the pull secret and proxy credentials as the
// URL-encoded or base64 payload of an Ignition data URL, and an installer
// log or install state escapes the quotes around a password or an auth.
// A data URL is decoded and, if its payload holds a secret, the payload is
// replaced whole; escaped quotes are read as quotes. secrets-4
// tightened this: a quoted value runs to its own closing
// quote; a backslash belongs to an unquoted value; percent-encoded base64
// data URLs; block scalars with a comment or as grep prints them; quoted
// literals and short values under a secret's noun; keys naming where a
// secret is had are kept. The version changed with the rules, so a
// conversation redacted by the weaker ones is begun again.
package redact

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type Policy struct {
	Version string
	rules   []rule
	// blocks redacts secret-named YAML block scalars as well.
	blocks bool
	// keepLines makes every replacement keep the line breaks it covers,
	// so redacted text has the stored text's lines and a line number
	// written after redaction is the stored line's (secrets-5).
	keepLines bool
}

type rule struct {
	kind string
	re   *regexp.Regexp
	// keep, if set, decides from the submatches whether a match is left
	// alone, for patterns that would otherwise take a name for a secret.
	keep func(m []string) bool
	// repl rebuilds the match with the secret replaced.
	repl func(m []string, placeholder string) string
}

func placeholder(kind string) string { return "[REDACTED:" + kind + "]" }

func whole(_ []string, p string) string { return p }

const (
	VersionNone    = "none-1"
	VersionSecrets = "secrets-5"
	// versionSecrets4 is the policy before replacements kept line breaks,
	// still known by its version.
	versionSecrets4 = "secrets-4"
)

var (
	none     = Policy{Version: VersionNone}
	secrets4 = Policy{Version: versionSecrets4, rules: []rule{
		{kind: "private_key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), repl: whole},
		{kind: "token", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`), repl: whole},
		{kind: "token", re: regexp.MustCompile(`\bsha256~[A-Za-z0-9_-]{20,}`), repl: whole},
		{kind: "token", re: regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{8,})`), repl: func(m []string, p string) string { return m[1] + p }},
		{kind: "password", re: regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)([^\s@/]+)@`),
			keep: func(m []string) bool { return strings.HasPrefix(m[2], "[REDACTED:") }, repl: func(m []string, p string) string { return m[1] + p + "@" }},
		{kind: "pull_secret", re: regexp.MustCompile(`(\\?"auth\\?"\s*:\s*\\?")([A-Za-z0-9+/=]{8,})(\\?")`), repl: func(m []string, p string) string { return m[1] + p + m[3] }},
		{kind: "credential", re: regexp.MustCompile(`(?i)\b(client-key-data|client-certificate-data|\.dockerconfigjson|\.dockercfg)(["']?\s*:\s*["']?)([A-Za-z0-9+/=]{16,})`),
			repl: func(m []string, p string) string { return m[1] + m[2] + p }},
		{kind: "cloud_credential", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), repl: whole},
		// A quoted value is taken whole, so a password with spaces in it
		// is not cut at its first word by the rule after; it runs to its
		// own closing quote, past an escaped quote in double quotes and a
		// doubled one in single quotes, as JSON and YAML write them.
		{kind: "secret", re: regexp.MustCompile(`(?i)\b(` + secretKey + `)(["']?[ \t]*[:=][ \t]*)(?:"((?:[^"\\\n]|\\.)*)"|'((?:[^'\n]|'')*)')`),
			keep: func(m []string) bool { return m[3]+m[4] == "" || keepQuotedValue(m[1], m[3]+m[4]) },
			repl: func(m []string, p string) string {
				q := m[0][len(m[1])+len(m[2]) : len(m[1])+len(m[2])+1]
				return m[1] + m[2] + q + p + q
			}},
		// A value in escaped quotes, as a log line or a JSON document
		// inside a string writes it: password: \"…\".
		{kind: "secret", re: regexp.MustCompile(`(?i)\b(` + secretKey + `)(\\?["']?[ \t]*[:=][ \t]*)\\"((?:[^"\\\n]|\\[^"])*)\\"`),
			keep: func(m []string) bool { return m[3] == "" || keepQuotedValue(m[1], m[3]) },
			repl: func(m []string, p string) string { return m[1] + m[2] + `\"` + p + `\"` }},
		// An unquoted value runs to a space, a quote or a delimiter; a
		// backslash is part of it unless it escapes a quote, which the rule
		// before takes.
		{kind: "secret", re: regexp.MustCompile(`(?i)\b(` + secretKey + `)(["']?[ \t]*[:=][ \t]*["']?)((?:[^\s"',;{}\[\]\\]|\\[^"\s])+)`),
			keep: func(m []string) bool { return tooShort(m[1], m[3]) || keepNamedValue(m[1], m[3]) }, repl: func(m []string, p string) string { return m[1] + m[2] + p }},
	}, blocks: true}
	secrets = Policy{Version: VersionSecrets, rules: secrets4.rules, blocks: true, keepLines: true}
)

// secretKey is a key that names a secret, such as DB_PASSWORD or tls.key.
const secretKey = `[A-Za-z0-9_.-]*(?:password|passwd|passphrase|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?)[A-Za-z0-9_.-]*|[A-Za-z0-9_-]+\.key`

// secretNoun is a key ending in the secret's own noun: its value is the
// secret whatever its shape, a number included.
var secretNoun = regexp.MustCompile(`(?i)(?:password|passwd|passphrase|secret|token|key|credentials?)$`)

// namesOnly reports a key that names a secret rather than holding one —
// secretName, tokenSecretRef, max_tokens — or names where one is had:
// token_endpoint, secretProviderURL, tokenIssuer.
func namesOnly(key string) bool {
	key = strings.ToLower(key)
	for _, suffix := range []string{"name", "ref", "namespace", "path", "file", "type", "seconds", "expiration", "ttl", "count", "tokens", "version",
		"endpoint", "url", "uri", "issuer"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// keepNamedValue leaves alone a key-value match whose key only names a
// secret, or whose value cannot be one: a boolean, a null, a placeholder
// already, or a number under a key such as token_limit, which counts
// rather than holds. A number under DB_PASSWORD is the password.
func keepNamedValue(key, value string) bool {
	if namesOnly(key) {
		return true
	}
	value = strings.ToLower(value)
	switch value {
	case "true", "false", "null", "none", "nil", "redacted":
		return true
	}
	if strings.HasPrefix(value, "[redacted:") {
		return true
	}
	return strings.Trim(value, "0123456789.") == "" && !secretNoun.MatchString(key)
}

// keepQuotedValue is keepNamedValue for a quoted value, which is a string
// whatever it spells: "false" under a password key is the password.
func keepQuotedValue(key, value string) bool {
	return namesOnly(key) || strings.HasPrefix(strings.ToLower(value), "[redacted:")
}

// tooShort reports an unquoted value too short to take for a secret: under
// four characters, unless the key ends in the secret's own noun, where any
// value is the secret but a block scalar's indicator, which the block rule
// reads.
func tooShort(key, value string) bool {
	if secretNoun.MatchString(key) {
		return blockIndicator.MatchString(value)
	}
	return len(value) < 4
}

var blockIndicator = regexp.MustCompile(`^[|>][-+0-9]*$`)

var blockHeader = regexp.MustCompile(`(?i)^[ \t-]*["']?(` + secretKey + `)["']?[ \t]*:[ \t]*[|>][-+0-9]*[ \t]*(?:#.*)?$`)

// grepPrefix is what grep writes before a line: its file and line number,
// "f:12:" for a match and "f-13-" for context.
var grepPrefix = regexp.MustCompile(`^([^\s:]+)[:-](\d+)[:-]`)

// unprefixed is a line without grep's prefix for file, if it has one.
func unprefixed(line, file string) (string, bool) {
	if m := grepPrefix.FindStringSubmatch(line); m != nil && m[1] == file {
		return line[len(m[0]):], true
	}
	return line, false
}

// blockValues replaces the value of a secret-named key written as a YAML
// block scalar — password: | and the lines indented beneath it — which
// the key-value rules cannot see past the indicator. It reports whether
// it found one not yet replaced.
func blockValues(text, placeholder string, keepLines bool) (string, bool) {
	if !strings.ContainsAny(text, "|>") {
		return text, false
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	found := false
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		// A line grep printed is read without its prefix, and so are the
		// block's lines after it, of the same file.
		head, file := lines[i], ""
		if g := grepPrefix.FindStringSubmatch(head); g != nil {
			head, file = head[len(g[0]):], g[1]
		}
		m := blockHeader.FindStringSubmatch(head)
		if m == nil || namesOnly(m[1]) {
			continue
		}
		body := func(j int) (string, string) {
			if file == "" {
				return "", lines[j]
			}
			if rest, ok := unprefixed(lines[j], file); ok {
				return lines[j][:len(lines[j])-len(rest)], rest
			}
			return "", "\x00"
		}
		// The block is every line indented past the key, blank lines
		// within it included.
		col := len(head) - len(strings.TrimLeft(head, " \t-"))
		end := i + 1
		for j := i + 1; j < len(lines); j++ {
			_, l := body(j)
			if l == "\x00" {
				break
			}
			if strings.TrimSpace(l) == "" {
				continue
			}
			if indent(l) <= col {
				break
			}
			end = j + 1
		}
		if end == i+1 {
			continue
		}
		prefix, first := body(i + 1)
		if end == i+2 && strings.TrimSpace(first) == placeholder {
			out = append(out, lines[i+1])
		} else {
			found = true
			out = append(out, prefix+first[:indent(first)]+placeholder)
			// The block's other lines, as empty lines, so the text keeps
			// its line count.
			if keepLines {
				for j := i + 2; j < end; j++ {
					pfx, _ := body(j)
					out = append(out, pfx)
				}
			}
		}
		i = end - 1
	}
	return strings.Join(out, "\n"), found
}

func indent(line string) int { return len(line) - len(strings.TrimLeft(line, " \t")) }

// For returns the policy for an egress class. internal_network gets the
// secrets policy too: it leaves the host, and only local_only is exempt.
func For(egressClass string) Policy {
	if egressClass == "local_only" {
		return none
	}
	return secrets
}

func ByVersion(version string) (Policy, bool) {
	switch version {
	case VersionNone:
		return none, true
	case VersionSecrets:
		return secrets, true
	case versionSecrets4:
		return secrets4, true
	}
	return Policy{}, false
}

func (p Policy) String(text string) string {
	if len(p.rules) > 0 {
		text = dataURL.ReplaceAllStringFunc(text, func(match string) string {
			m := dataURL.FindStringSubmatch(match)
			if kind, ok := p.find(decode(m[1], m[2])); ok {
				return "data:" + m[1] + "," + placeholder(kind)
			}
			return match
		})
	}
	if p.blocks {
		text, _ = blockValues(text, placeholder("secret"), p.keepLines)
	}
	for _, r := range p.rules {
		text = r.re.ReplaceAllStringFunc(text, func(match string) string {
			m := r.re.FindStringSubmatch(match)
			if r.keep != nil && r.keep(m) {
				return match
			}
			out := r.repl(m, placeholder(r.kind))
			// A replacement keeps the line breaks it covers: a private
			// key's block becomes its placeholder and empty lines.
			if n := strings.Count(match, "\n") - strings.Count(out, "\n"); p.keepLines && n > 0 {
				out += strings.Repeat("\n", n)
			}
			return out
		})
	}
	return text
}

// dataURL is a data URL, its media type and parameters, then its payload,
// up to whatever ends it in YAML, JSON or a log line.
var dataURL = regexp.MustCompile(`data:([A-Za-z0-9.+/=;_-]*),([^\s"'\\<>]+)`)

// decode is a data URL's payload as text: base64 where its parameters
// say so, percent-encoding undone first, otherwise URL-encoded. A payload that does not decode is read as
// written.
func decode(params, payload string) string {
	if strings.HasSuffix(params, ";base64") {
		// Percent-encoding may wrap base64 too: %3D for its padding.
		if s, err := url.PathUnescape(payload); err == nil {
			payload = s
		}
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding} {
			if b, err := enc.DecodeString(payload); err == nil {
				return string(b)
			}
		}
		return payload
	}
	if s, err := url.PathUnescape(payload); err == nil {
		return s
	}
	return payload
}

// Holds reports the kind of the first secret the policy names left in
// text, if any: a query still holding one after String is refused, as a
// request is.
func (p Policy) Holds(text string) (string, bool) { return p.find(text) }

// find reports the kind of the first secret left in text, if any; one in
// a data URL's payload counts.
func (p Policy) find(text string) (string, bool) {
	for _, m := range dataURL.FindAllStringSubmatch(text, -1) {
		if kind, ok := p.find(decode(m[1], m[2])); ok {
			return kind, true
		}
	}
	if p.blocks {
		if _, found := blockValues(text, placeholder("secret"), false); found {
			return "secret", true
		}
	}
	for _, r := range p.rules {
		for _, m := range r.re.FindAllStringSubmatch(text, -1) {
			if r.keep == nil || !r.keep(m) {
				return r.kind, true
			}
		}
	}
	return "", false
}

// JSON redacts the string values of a JSON document, leaving its keys and
// structure. A payload that is not JSON is redacted as text.
func (p Policy) JSON(doc []byte) ([]byte, error) {
	if len(p.rules) == 0 || len(doc) == 0 {
		return doc, nil
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return []byte(p.String(string(doc))), nil
	}
	out, err := json.Marshal(walk(v, p.String))
	if err != nil {
		return nil, fmt.Errorf("redact: %w", err)
	}
	return out, nil
}

func walk(v any, f func(string) string) any {
	switch n := v.(type) {
	case string:
		return f(n)
	case []any:
		for i := range n {
			n[i] = walk(n[i], f)
		}
	case map[string]any:
		for k, c := range n {
			n[k] = walk(c, f)
		}
	}
	return v
}
