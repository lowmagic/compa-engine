package engine

import (
	"math"
	"strconv"
	"strings"
)

// A code span that is only arithmetic is evaluated, not searched for in
// the evidence (docs/MODEL_API.md §Answer checks). The grammar is kept
// small:
//
//	span   = sum cmp sum { cmp sum }
//	cmp    = "=" | "!=" | "<" | ">" | "<=" | ">="
//	sum    = term { ("+" | "-") term }
//	term   = factor { ("*" | "/") factor }
//	factor = "-" factor | number [unit] | "(" sum ")"
//	number = digits [ "." digits ], digits grouped by commas or not
//
// with spaces anywhere between, at least one of + - * / between two
// operands, no number written with a leading zero (`06`), and at most one
// unit word, letters only, the same after every number that has one:
// `16 GiB / 4 = 4 GiB`. A span with no comparison or no operator — a
// version, `4.9 < 4.14` — or with a leading zero — a date,
// `2026-11-06 < 2026-11-08` — any other character or word, two different
// units or a division by zero is not arithmetic, and is searched for as
// any span is. Units are not converted: they are only the same word.
//
// = and != compare the two sides to the precision the span is written to:
// they are equal where they differ by at most half the last decimal place
// of the number written with the most, so `10 / 3 = 3.33` holds and
// `16 GiB / 4 = 5 GiB` does not. A chain holds where each comparison does.

// arithmetic evaluates span: holds says whether it is true, and ok whether
// it is arithmetic at all.
func arithmetic(span string) (holds, ok bool) {
	toks, decimals, ok := arithTokens(span)
	if !ok {
		return false, false
	}
	p := &arithParser{toks: toks}
	left, ok := p.sum()
	if !ok {
		return false, false
	}
	half := 0.5 * math.Pow10(-decimals)
	holds, compared := true, false
	for p.at < len(p.toks) {
		op := p.toks[p.at]
		if op.kind != arithCmp {
			return false, false
		}
		p.at++
		right, ok := p.sum()
		if !ok {
			return false, false
		}
		holds = holds && compare(op.text, left, right, half)
		compared, left = true, right
	}
	if !compared || p.ops == 0 {
		return false, false
	}
	return holds, true
}

// compare applies a comparison, = and != within half of the span's last
// decimal place, the others exactly but for a float's rounding.
func compare(op string, a, b, half float64) bool {
	eps := 1e-9 * max(1, math.Abs(a), math.Abs(b))
	equal := math.Abs(a-b) <= half+eps
	switch op {
	case "=":
		return equal
	case "!=":
		return !equal
	case "<":
		return a < b-eps
	case ">":
		return a > b+eps
	case "<=":
		return a <= b+eps
	case ">=":
		return a >= b-eps
	}
	return false
}

type arithKind int

const (
	arithNum arithKind = iota
	arithOp
	arithCmp
	arithOpen
	arithClose
)

type arithTok struct {
	kind arithKind
	text string
	num  float64
}

// arithTokens are span's tokens, its units dropped, and the most decimal
// places any of its numbers is written with; ok is false where span holds
// anything the grammar does not.
func arithTokens(span string) (toks []arithTok, decimals int, ok bool) {
	unit := ""
	s := span
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ':
			i++
		case isDigit(c):
			if c == '0' && i+1 < len(s) && isDigit(s[i+1]) {
				// A leading zero: a date's or a time's field, no number.
				return nil, 0, false
			}
			j := i
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			for j < len(s) && s[j] == ',' && grouped([]byte(s[j+1:])) {
				j += 4
			}
			d := 0
			if j+1 < len(s) && s[j] == '.' && isDigit(s[j+1]) {
				j++
				for j < len(s) && isDigit(s[j]) {
					j++
					d++
				}
			}
			n, err := strconv.ParseFloat(strings.ReplaceAll(s[i:j], ",", ""), 64)
			if err != nil {
				return nil, 0, false
			}
			decimals = max(decimals, d)
			toks = append(toks, arithTok{kind: arithNum, num: n})
			i = j
			// A unit word after the number, spaces between or none.
			k := i
			for k < len(s) && s[k] == ' ' {
				k++
			}
			w := k
			for w < len(s) && isLetter(s[w]) {
				w++
			}
			if w > k {
				if unit != "" && s[k:w] != unit {
					return nil, 0, false
				}
				unit, i = s[k:w], w
			}
		case c == '+' || c == '-' || c == '*' || c == '/':
			toks = append(toks, arithTok{kind: arithOp, text: string(c)})
			i++
		case c == '(':
			toks = append(toks, arithTok{kind: arithOpen})
			i++
		case c == ')':
			toks = append(toks, arithTok{kind: arithClose})
			i++
		case c == '=':
			toks = append(toks, arithTok{kind: arithCmp, text: "="})
			i++
		case c == '<' || c == '>' || c == '!':
			op := string(c)
			if i+1 < len(s) && s[i+1] == '=' {
				op += "="
			}
			if op == "!" {
				return nil, 0, false
			}
			toks = append(toks, arithTok{kind: arithCmp, text: op})
			i += len(op)
		default:
			return nil, 0, false
		}
	}
	return toks, decimals, true
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// arithParser reads the grammar's sums from toks; ops counts the
// operators it read between two operands.
type arithParser struct {
	toks []arithTok
	at   int
	ops  int
}

func (p *arithParser) peek(kind arithKind) (arithTok, bool) {
	if p.at < len(p.toks) && p.toks[p.at].kind == kind {
		return p.toks[p.at], true
	}
	return arithTok{}, false
}

func (p *arithParser) sum() (float64, bool) {
	v, ok := p.term()
	for ok {
		t, more := p.peek(arithOp)
		if !more || t.text != "+" && t.text != "-" {
			break
		}
		p.at++
		p.ops++
		var r float64
		if r, ok = p.term(); t.text == "+" {
			v += r
		} else {
			v -= r
		}
	}
	return v, ok
}

func (p *arithParser) term() (float64, bool) {
	v, ok := p.factor()
	for ok {
		t, more := p.peek(arithOp)
		if !more || t.text != "*" && t.text != "/" {
			break
		}
		p.at++
		p.ops++
		var r float64
		if r, ok = p.factor(); !ok {
			break
		}
		if t.text == "*" {
			v *= r
			continue
		}
		if r == 0 {
			return 0, false
		}
		v /= r
	}
	return v, ok
}

func (p *arithParser) factor() (float64, bool) {
	if t, ok := p.peek(arithOp); ok && t.text == "-" {
		p.at++
		v, ok := p.factor()
		return -v, ok
	}
	if t, ok := p.peek(arithNum); ok {
		p.at++
		return t.num, true
	}
	if _, ok := p.peek(arithOpen); ok {
		p.at++
		v, ok := p.sum()
		if _, closed := p.peek(arithClose); !ok || !closed {
			return 0, false
		}
		p.at++
		return v, true
	}
	return 0, false
}
