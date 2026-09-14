package main

import "strings"

// regexRequiredLiteralAlternatives finds literal runs that any match of a
// regex must contain, so they can serve as an exact-superset pre-filter.
//
// The old `regexRequiredLiteral` returns a single run and gives up on any
// alternation, so a pattern like `^.*\.(go|rs|py|js)$` matches nothing the
// filter can use and falls back to a full-volume scan. This extractor instead
// expands the pattern to a disjunction of literal sequences (DNF), keeps the
// literal runs of each disjunct, and returns them as alternatives: a match must
// contain at least one run chosen from each disjunct's run list.
//
// The result is a slice of disjuncts; each disjunct is the list of runs
// (length >= 3, in pattern order) that a match taking that branch must contain.
// A caller picks one run per disjunct and unions the results into a superset
// membership filter. Returns nil when no sound set can be proven (a disjunct
// with no usable run, an unrelaxable construct such as a backreference, or an
// expansion that blows past the disjunct cap).
//
// # Soundness
//
// Every rewrite only widens the matched language: character classes and `.`
// become an unconstrained gap, an optional (`?`/`*`/`{0,n}`) sub-expression is
// dropped, and zero-width assertions are discarded. Widening is the safe
// direction: the widened language is a superset of the original, so a run that
// every widened match contains is also contained by every original match.
// Narrowing would risk dropping a genuine hit, so anything that cannot be
// widened (backreferences, conditionals, `\K`, `\G`) returns nil instead.
func regexRequiredLiteralAlternatives(pattern string) [][]string {
	pattern = strings.TrimPrefix(pattern, "(?i)")
	// This parser consumes bytes, but Go regex quantifiers bind to runes and the
	// literals are later compared as bytes.  A non-ASCII pattern (e.g. "abcé?")
	// could therefore prove an unsound run, so decline non-ASCII patterns.
	for i := 0; i < len(pattern); i++ {
		if pattern[i] >= 0x80 {
			return nil
		}
	}
	p := &rxParser{src: pattern}
	node := p.parseAlt()
	if node == nil || p.pos != len(p.src) {
		return nil
	}
	disjuncts := rxDNF(node, rxMaxDisjuncts)
	if len(disjuncts) == 0 {
		return nil
	}
	out := make([][]string, 0, len(disjuncts))
	for _, seq := range disjuncts {
		runs := rxLiteralRuns(seq)
		if len(runs) == 0 {
			return nil
		}
		for i := range runs {
			runs[i] = strings.ToLower(runs[i])
		}
		out = append(out, runs)
	}
	return out
}

const rxMaxDisjuncts = 128

// rxTok is one element of an expanded sequence: a literal byte (0..255) or a
// gap (-1) that breaks a literal run.
type rxTok struct{ b int }

const rxGapTok = -1

// rxLiteralRuns returns the maximal contiguous literal runs of at least three
// bytes in a disjunct sequence, in order.
func rxLiteralRuns(seq []rxTok) []string {
	var runs []string
	run := make([]byte, 0, 8)
	flush := func() {
		if len(run) >= 3 {
			runs = append(runs, string(run))
		}
		run = run[:0]
	}
	for _, tk := range seq {
		if tk.b == rxGapTok {
			flush()
			continue
		}
		run = append(run, byte(tk.b))
	}
	flush()
	return runs
}

type rxNode interface{ rxNode() }

type rxLit struct{ b byte }
type rxGap struct{}
type rxSeq struct{ parts []rxNode }
type rxAlt struct{ branches []rxNode }
type rxRep struct {
	sub rxNode
	min int
}

func (rxLit) rxNode() {}
func (rxGap) rxNode() {}
func (rxSeq) rxNode() {}
func (rxAlt) rxNode() {}
func (rxRep) rxNode() {}

type rxParser struct {
	src string
	pos int
}

func (p *rxParser) parseAlt() rxNode {
	first := p.parseSeq()
	if first == nil {
		return nil
	}
	branches := []rxNode{first}
	for p.pos < len(p.src) && p.src[p.pos] == '|' {
		p.pos++
		b := p.parseSeq()
		if b == nil {
			return nil
		}
		branches = append(branches, b)
	}
	if len(branches) == 1 {
		return first
	}
	return rxAlt{branches: branches}
}

func (p *rxParser) parseSeq() rxNode {
	var parts []rxNode
	for p.pos < len(p.src) {
		if c := p.src[p.pos]; c == '|' || c == ')' {
			break
		}
		atom := p.parseAtom()
		if atom == nil {
			return nil
		}
		parts = append(parts, p.parseQuant(atom))
	}
	switch len(parts) {
	case 0:
		return rxSeq{}
	case 1:
		return parts[0]
	default:
		return rxSeq{parts: parts}
	}
}

// parseQuant consumes a quantifier immediately following an atom. A minimum of
// zero makes the atom optional, so it is dropped (a gap); a minimum of one
// keeps a single occurrence, which is sound because every `sub{min>=1}` match
// contains at least one `sub` occurrence.
func (p *rxParser) parseQuant(atom rxNode) rxNode {
	if p.pos >= len(p.src) {
		return atom
	}
	switch p.src[p.pos] {
	case '?', '*':
		p.pos++
		return rxRep{sub: atom, min: 0}
	case '+':
		p.pos++
		return rxRep{sub: atom, min: 1}
	case '{':
		save := p.pos
		i := p.pos + 1
		start := i
		for i < len(p.src) && p.src[i] >= '0' && p.src[i] <= '9' {
			i++
		}
		if i == start {
			return atom
		}
		min := 0
		for _, d := range p.src[start:i] {
			min = min*10 + int(d-'0')
		}
		if i < len(p.src) && p.src[i] == ',' {
			i++
			for i < len(p.src) && p.src[i] >= '0' && p.src[i] <= '9' {
				i++
			}
		}
		if i >= len(p.src) || p.src[i] != '}' {
			p.pos = save
			return atom
		}
		p.pos = i + 1
		if min >= 1 {
			return rxRep{sub: atom, min: 1}
		}
		return rxRep{sub: atom, min: 0}
	}
	return atom
}

func (p *rxParser) parseAtom() rxNode {
	c := p.src[p.pos]
	switch c {
	case '(':
		return p.parseGroup()
	case '[':
		return p.parseClass()
	case '.', '^', '$':
		p.pos++
		return rxGap{}
	case '\\':
		return p.parseEscape()
	case '*', '+', '?', '{', '}', ']', ')':
		return nil
	default:
		p.pos++
		return rxLit{b: c}
	}
}

// parseEscape handles a backslash sequence. Escaped punctuation is a literal
// byte; class shorthands and anchors become gaps; backreferences and the
// constructs that cannot be widened return nil.
func (p *rxParser) parseEscape() rxNode {
	p.pos++
	if p.pos >= len(p.src) {
		return nil
	}
	e := p.src[p.pos]
	p.pos++
	switch e {
	case 'd', 'w', 's', 'D', 'W', 'S', 'b', 'B', 'n', 'r', 't', 'f', 'v':
		return rxGap{}
	case '0':
		// \0 starts an octal escape; consume up to two more octal digits so they
		// are not misread as literal characters (e.g. \0123 is \012 then '3').
		for n := 0; n < 2 && p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '7'; n++ {
			p.pos++
		}
		return rxGap{}
	case 'k', 'g', 'K', 'G':
		return nil
	default:
		if e >= '1' && e <= '9' {
			return nil
		}
		// Only escaped punctuation is a literal byte.  Escaped letters/digits
		// such as \x, \u, \A, \z, \Q, \p, \c are not that literal letter;
		// treating them as one would drop real matches, so decline instead.
		if (e >= 'a' && e <= 'z') || (e >= 'A' && e <= 'Z') {
			return nil
		}
		return rxLit{b: e}
	}
}

// parseGroup parses a parenthesized group. Plain, non-capturing, named and
// flag groups recurse into the body; lookaround is dropped as a gap and atomic
// groups are treated as ordinary (a superset). Conditionals decline.
func (p *rxParser) parseGroup() rxNode {
	p.pos++ // (
	if p.pos < len(p.src) && p.src[p.pos] == '?' {
		p.pos++
		if p.pos >= len(p.src) {
			return nil
		}
		switch p.src[p.pos] {
		case '=', '!':
			return p.skipGroupAsGap()
		case '<':
			if p.pos+1 < len(p.src) && (p.src[p.pos+1] == '=' || p.src[p.pos+1] == '!') {
				return p.skipGroupAsGap()
			}
			for p.pos < len(p.src) && p.src[p.pos] != '>' {
				p.pos++
			}
			if p.pos < len(p.src) {
				p.pos++
			}
		case 'P':
			if p.pos+1 < len(p.src) && p.src[p.pos+1] == '<' {
				for p.pos < len(p.src) && p.src[p.pos] != '>' {
					p.pos++
				}
				if p.pos < len(p.src) {
					p.pos++
				}
			} else {
				return nil
			}
		case '>':
			p.pos++
		case ':', 'i', 'm', 's', 'x', 'U', 'R', '-':
			for p.pos < len(p.src) && p.src[p.pos] != ':' && p.src[p.pos] != ')' {
				p.pos++
			}
			if p.pos < len(p.src) && p.src[p.pos] == ':' {
				p.pos++
			}
		default:
			return nil
		}
	}
	inner := p.parseAlt()
	if inner == nil {
		return nil
	}
	if p.pos >= len(p.src) || p.src[p.pos] != ')' {
		return nil
	}
	p.pos++
	return inner
}

// skipGroupAsGap consumes a balanced group body after the `(?…` prefix and
// returns a gap, dropping a zero-width assertion.
func (p *rxParser) skipGroupAsGap() rxNode {
	depth := 1
	for p.pos < len(p.src) && depth > 0 {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
			continue
		case '(':
			depth++
		case ')':
			depth--
		}
		p.pos++
	}
	return rxGap{}
}

func (p *rxParser) parseClass() rxNode {
	p.pos++ // [
	if p.pos < len(p.src) && p.src[p.pos] == '^' {
		p.pos++
	}
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
	}
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case '\\':
			p.pos += 2
			continue
		case ']':
			p.pos++
			return rxGap{}
		}
		p.pos++
	}
	return nil
}

// rxDNF expands a node to a disjunction of token sequences, capped at limit
// sequences so a pattern with many alternations cannot explode.
func rxDNF(n rxNode, limit int) [][]rxTok {
	switch v := n.(type) {
	case rxLit:
		return [][]rxTok{{{b: int(v.b)}}}
	case rxGap:
		return [][]rxTok{{{b: rxGapTok}}}
	case rxRep:
		if v.min == 0 {
			return [][]rxTok{{{b: rxGapTok}}}
		}
		sub := rxDNF(v.sub, limit)
		if sub == nil {
			return nil
		}
		// A min>=1 repetition matches at least one occurrence but may match more,
		// so a literal after it is not contiguous with literals inside or before
		// it.  Keep one occurrence (those literals are required) then force a run
		// break; without this, `ab+cd` would fabricate the run `abcd`.
		out := make([][]rxTok, len(sub))
		for i, s := range sub {
			toks := make([]rxTok, 0, len(s)+1)
			toks = append(toks, s...)
			toks = append(toks, rxTok{b: rxGapTok})
			out[i] = toks
		}
		return out
	case rxAlt:
		var out [][]rxTok
		for _, br := range v.branches {
			sub := rxDNF(br, limit)
			if sub == nil {
				return nil
			}
			out = append(out, sub...)
			if len(out) > limit {
				return nil
			}
		}
		return out
	case rxSeq:
		acc := [][]rxTok{{}}
		for _, part := range v.parts {
			sub := rxDNF(part, limit)
			if sub == nil {
				return nil
			}
			var next [][]rxTok
			for _, a := range acc {
				for _, s := range sub {
					merged := make([]rxTok, 0, len(a)+len(s))
					merged = append(merged, a...)
					merged = append(merged, s...)
					next = append(next, merged)
					if len(next) > limit {
						return nil
					}
				}
			}
			acc = next
		}
		return acc
	}
	return nil
}
