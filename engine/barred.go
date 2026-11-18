package engine

import (
	"bufio"
	"context"
	"os"
	"strings"
)

// The deployment may name a list of accounts barred from AI processing
// (reasoning.barredAccounts.list): one account number a line, blank lines
// and # comments ignored. Where it does, no model
// call is made for a case whose account is on it, and the rule fails
// closed: a case that cannot be checked, because the list cannot be read
// or its source gave no account number, is refused too, until the
// engineer overrides it for that case (docs/standards.md §Safety). The
// check is made at the gate every companion model call passes
// (companionGate), so no entry point can leave it out, and the list is
// read at every call, so a case barred while open is refused from its next
// call on.

// BarredDecision is whether a case's model calls are refused, and why.
type BarredDecision struct {
	refused bool
	// reason says why, for the engineer; it never carries the account
	// number.
	reason string
	// unchecked is a refusal because the check could not be made, which
	// an engineer's override may lift; a list match is never unchecked.
	unchecked bool
	// overridden is the engineer's override under which an unchecked
	// case's calls go; nil where none applies.
	overridden *ModelsOverride
}

// barredDecision decides for a case now: the list read afresh, the account
// number as the case's source last gave it.
func (e *Engine) barredDecision(ctx context.Context, c *openCase) BarredDecision {
	path := e.config.BarredAccountsPath()
	if path == "" {
		return BarredDecision{}
	}
	// A chat with no case has no account. With no directory it holds no
	// case's data; a directory of artifacts belongs to an account that
	// cannot be checked, so it is refused, with nothing to override.
	if c.chat {
		if c.root == "" {
			return BarredDecision{}
		}
		return BarredDecision{refused: true, unchecked: true, reason: "a directory's account cannot be checked against the deployment's list of accounts barred from AI processing; open its case with compa <case> instead"}
	}
	d := checkAccount(ctx, c, path)
	if !d.unchecked {
		return d
	}
	// A check that could not be made is the engineer's to override, for
	// this case, after the deployment's instructions.
	if o, err := readOverride(ctx, c); err != nil {
		d.reason += "; and " + err.Error()
	} else if o != nil {
		return BarredDecision{overridden: o}
	}
	return d
}

// checkAccount decides from the list at path and the case's account number.
func checkAccount(ctx context.Context, c *openCase, path string) BarredDecision {
	number, _, err := c.st.Meta(ctx, metaAccountNumber)
	if err != nil {
		return BarredDecision{refused: true, unchecked: true, reason: "the case's account number could not be read from its store: " + err.Error()}
	}
	list, err := readBarredList(path)
	if err != nil {
		return BarredDecision{refused: true, unchecked: true, reason: "the deployment's list of accounts barred from AI processing could not be read: " + err.Error()}
	}
	if number == "" {
		return BarredDecision{refused: true, unchecked: true, reason: "the case's source gave no account number to check against the deployment's list of accounts barred from AI processing"}
	}
	if list[normalizeAccount(number)] {
		return BarredDecision{refused: true, reason: "this case's account is on the deployment's list of accounts barred from AI processing"}
	}
	return BarredDecision{}
}

// readBarredList reads the list's account numbers, normalized. An empty
// list is a valid list.
func readBarredList(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if n := normalizeAccount(line); n != "" {
			out[n] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeAccount trims an account number and strips the leading zeros of
// an all-digit one, so a zero-padded form matches, erring toward
// refusing.
func normalizeAccount(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && strings.Trim(s, "0123456789") == "" {
		if t := strings.TrimLeft(s, "0"); t != "" {
			return t
		}
		return "0"
	}
	return s
}

// Line is what the case's heading and the command line say of the
// rule, or nothing where it refuses nothing.
func (d BarredDecision) Line() string {
	switch {
	case d.overridden != nil:
		return d.overridden.Line()
	case !d.refused:
		return ""
	case d.unchecked:
		return "AI processing refused: " + d.reason
	}
	return "AI processing refused: barred account"
}
