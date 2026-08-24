package main

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Fuzzy matching fills only the shortfall left by exact search: it runs after
// the normal planner underfills the result limit, and its results always sort
// below exact-tier results. See docs/FUZZY_RANKED_SEARCH_PLAN.md.

const (
	fuzzyMinTermRunes  = 3
	fuzzyMaxDistance   = 2
	fuzzyCandidateCap  = serviceNameTrigramCandidateMaxIDs
	fuzzyCollectFactor = 16
)

// fuzzyThresholdForTerm picks the allowed Damerau-Levenshtein distance for a
// term. Thresholds stay conservative because sliding-window verification over
// long names multiplies candidate chances: distance 2 only opens up for long
// terms where typos are more likely to be multiple and distinct.
func fuzzyThresholdForTerm(term string) int {
	if utf8.RuneCountInString(term) < 8 {
		return 1
	}
	return 2
}

// damerauLevenshteinBounded returns the optimal-string-alignment distance
// between a and b, capped at max. It returns max+1 as soon as the distance is
// known to exceed max so callers can skip cheaply.
func damerauLevenshteinBounded(a, b []rune, max int) int {
	la, lb := len(a), len(b)
	if la-lb > max || lb-la > max {
		return max + 1
	}
	if la == 0 {
		if lb <= max {
			return lb
		}
		return max + 1
	}
	if lb == 0 {
		if la <= max {
			return la
		}
		return max + 1
	}
	prev2 := make([]int, lb+1)
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d := prev[j] + 1
			if v := cur[j-1] + 1; v < d {
				d = v
			}
			if v := prev[j-1] + cost; v < d {
				d = v
			}
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				if t := prev2[j-2] + 1; t < d {
					d = t
				}
			}
			cur[j] = d
			if d < rowMin {
				rowMin = d
			}
		}
		if rowMin > max {
			return max + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	if prev[lb] > max {
		return max + 1
	}
	return prev[lb]
}

// fuzzyDeletionVariants returns the term plus every string reachable by up to
// depth edits restricted to single-character deletions and adjacent
// transpositions (deduplicated). Deletion chains cover substitutions,
// insertions, and deletions per the SymSpell argument; explicit transposition
// variants cover Damerau swaps, which pure deletion neighborhoods miss.
func fuzzyDeletionVariants(term string, depth int) []string {
	set := map[string]struct{}{term: {}}
	layer := []string{term}
	for d := 0; d < depth; d++ {
		next := make([]string, 0, len(layer)*6)
		for _, s := range layer {
			runes := []rune(s)
			for i := 0; i < len(runes); i++ {
				variant := string(runes[:i]) + string(runes[i+1:])
				if _, ok := set[variant]; !ok {
					set[variant] = struct{}{}
					next = append(next, variant)
				}
				if i > 0 {
					swappedRunes := []rune(s)
					swappedRunes[i-1], swappedRunes[i] = swappedRunes[i], swappedRunes[i-1]
					variant := string(swappedRunes)
					if _, ok := set[variant]; !ok {
						set[variant] = struct{}{}
						next = append(next, variant)
					}
				}
			}
		}
		layer = next
	}
	out := make([]string, 0, len(set))
	out = append(out, term)
	for variant := range set {
		if variant != term {
			out = append(out, variant)
		}
	}
	return out
}

// foldFuzzyText normalizes text for fuzzy comparison: fullwidth forms fold to
// their ASCII equivalents and combining marks are stripped so "sodanco"
// matches "Só Danço". Comparison-side only; index data is untouched.
func foldFuzzyText(s string) string {
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		} else if r == 0x3000 {
			r = ' '
		}
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fuzzyNameDistance finds the best (lowest) bounded Damerau-Levenshtein
// distance between the term and any window of the lowercased name whose rune
// length is within tau of the term length. prefix reports whether the best
// alignment starts at the beginning of the name. ok is false when even the
// best window exceeds tau.
func fuzzyNameDistance(nameLower, term string, tau int) (dist int, prefix bool, windowPos int, ok bool) {
	nameRunes := []rune(nameLower)
	termRunes := []rune(term)
	lt, ln := len(termRunes), len(nameRunes)
	if ln == 0 {
		return tau + 1, false, 0, false
	}
	best := tau + 1
	prefixBest := false
	bestStart := ln
	end := ln - lt + tau
	if end > ln {
		end = ln
	}
	for start := 0; start <= end; start++ {
		maxWidth := lt + tau
		if maxWidth > ln-start {
			maxWidth = ln - start
		}
		minWidth := lt - tau
		if minWidth < 1 {
			minWidth = 1
		}
		for width := minWidth; width <= maxWidth; width++ {
			d := damerauLevenshteinBounded(nameRunes[start:start+width], termRunes, tau)
			atPrefix := start == 0
			if d < best || (d == best && start < bestStart) {
				if d < best {
					best = d
					prefixBest = atPrefix
				}
				bestStart = start
			} else if d == best && atPrefix {
				prefixBest = true
			}
			if best == 0 {
				break
			}
		}
		if best == 0 {
			break
		}
	}
	prefix = prefixBest
	if best > tau {
		return best, false, bestStart, false
	}
	return best, prefix, bestStart, true
}

type fuzzyScoredEntry struct {
	entry      Entry
	dist       int
	prefix     bool
	windowPos  int
	rank       uint64
	path       string
	lenDiff    int
}

// appendFuzzyServiceMatches tops up an underfilled exact result set with
// close-match entries gathered from the per-volume name trigram postings.
// It reports whether any fuzzy results were added; callers surface that via
// the response's fuzzy flag and planner mode.
func appendFuzzyServiceMatches(volumes []*serviceVolumeIndex, opts queryOptions, matches []Entry) ([]Entry, bool) {
	limit := normalizedLimit(opts.Limit, false)
	need := limit - len(matches)
	if need <= 0 {
		return matches, false
	}
	pq, err := parseQuery(opts)
	if err != nil || !pq.Fuzzy || pq.CaseSensitive || pq.MatchPath || len(pq.Terms) != 1 || len(pq.OrGroups) > 0 || len(pq.Regexps) > 0 || len(pq.Globs) > 0 {
		return matches, false
	}
	term := foldFuzzyText(strings.ToLower(pq.Terms[0]))
	if utf8.RuneCountInString(term) < fuzzyMinTermRunes {
		return matches, false
	}
	tau := fuzzyThresholdForTerm(term)
	if tau > fuzzyMaxDistance {
		tau = fuzzyMaxDistance
	}
	variants := fuzzyDeletionVariants(term, tau)
	// Deterministic order: the candidate collection cap cuts off work, so
	// iteration order must not change which results users see.
	sort.Strings(variants)
	termRunes := int64(utf8.RuneCountInString(term))

	pqNoTerms := pq
	pqNoTerms.Terms = nil

	existing := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		existing[m.Path] = struct{}{}
	}

	var out []fuzzyScoredEntry
	enough := need * fuzzyCollectFactor
	for _, vol := range volumes {
		if vol == nil || vol.index == nil || vol.nameTrigramIndex() == nil {
			continue
		}
		rankVec := vol.rankForQuery(pqNoTerms)
		pathCache := make(map[int]string)
		seen := make(map[int]struct{})
		for _, variant := range variants {
			ids, ok := vol.nameTrigramNameTermPostingLimited(variant, fuzzyCandidateCap)
			if !ok {
				continue
			}
			for _, id := range ids {
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				if id < 0 || id >= vol.index.compactRecordCount() {
					continue
				}
				rec := vol.index.compactRecord(id)
				if rec.Deleted {
					continue
				}
				entry := compactEntryFromRecord(vol.index, id, rec, pathCache, true)
				if _, dupPath := existing[entry.Path]; dupPath {
					continue
				}
				nameLower := foldFuzzyText(strings.ToLower(entry.Name))
				dist, prefix, windowPos, okDist := fuzzyNameDistance(nameLower, term, tau)
				if !okDist || dist == 0 {
					continue
				}
				if !entryMatches(entry, pqNoTerms, pq.MatchPath) {
					continue
				}
				var rank uint64
				if id < len(rankVec) {
					rank = uint64(rankVec[id])
				}
				lenDiff := utf8.RuneCountInString(entry.Name) - int(termRunes)
				if lenDiff < 0 {
					lenDiff = -lenDiff
				}
				out = append(out, fuzzyScoredEntry{entry: entry, dist: dist, prefix: prefix, windowPos: windowPos, rank: rank, path: entry.Path, lenDiff: lenDiff})
			}
			if len(out) >= enough {
				break
			}
		}
		if len(out) >= enough {
			break
		}
	}
	if len(out) == 0 {
		return matches, false
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		if out[i].prefix != out[j].prefix {
			return out[i].prefix
		}
		if out[i].windowPos != out[j].windowPos {
			return out[i].windowPos < out[j].windowPos
		}
		if out[i].lenDiff != out[j].lenDiff {
			return out[i].lenDiff < out[j].lenDiff
		}
		if out[i].rank != out[j].rank {
			return out[i].rank < out[j].rank
		}
		return out[i].path < out[j].path
	})
	if len(out) > need {
		out = out[:need]
	}
	appended := make([]Entry, 0, len(out))
	for _, scored := range out {
		appended = append(appended, scored.entry)
	}
	return append(matches, appended...), true
}
