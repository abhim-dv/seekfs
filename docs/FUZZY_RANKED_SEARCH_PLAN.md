# Fuzzy Ranked Search — Implementation Plan

Branch: `feature/fuzzy-ranked-search`
Status: planned
Research basis: `docs/USER_RESEARCH_FEATURE_MATRIX_2026-08.md` (P1 "Rank" item)

## Problem statement

seekfs has zero typo tolerance: matching is case-insensitive substring
accelerated by trigram postings, so `reprot` finds nothing. Where competitors
do offer fuzzy matching, ranking is broken (Everything's own TODO: *"fuzzy
search needs a ranking system so 'tonic' shows 'tonic' results above
'sonic'"*; PowerToys #43864 users pick tools on this).

## Design: two strict tiers

**Tier 1 (exact substring/trigram/regex) is never polluted.** Fuzzy results
only ever appear *below* exact results, and only when exact results underfill
the requested limit.

### Syntax

- `term~` marks a single term fuzzy: `seekfs search "reprot~"`
- `--fuzzy` flag enables the same fallback for every plain term in the query
- Response carries `fuzzy: true`, trace `PlannerMode: fuzzy-trigram`; UI shows
  "showing close matches"

### Candidate generation (reuses existing machinery)

1. Lowercase the term, generate its distance-1 deletion/transposition
   neighborhood (SymSpell-style): e.g. `reprot` -> {`repot`, `repor`, `erport`,
   ...} plus the original.
2. Emit trigrams of every variant, look them up in the per-volume PNGR name
   trigram posting lists, intersect per variant and union across variants to
   get a candidate ID superset. Existing candidate caps apply
   (`serviceNameTrigramCandidateMaxIDs` family).
3. Verify each candidate's lowercase name against the original term with true
   Damerau-Levenshtein distance, threshold = max(1, len(term)/4), capped at 2.

Terms shorter than 3 characters are not fuzzed (noise). Fuzzy is skipped for
regex/glob/wildcard terms and for field-filter terms (`ext:` etc.).

### Ranking (lexicographic)

`(distance, prefixBonus, nameRank)`:

1. lower Damerau-Levenshtein distance first (tier starts at 1)
2. prefix match (name starts with the term) beats infix
3. tiebreak via the existing RANK vector (same global ordering as exact search)

Exact-tier results always sort above all fuzzy results.

### Normalization add-on

Fold diacritics (NFD mark stripping) and CJK fullwidth->halfwidth forms during
lowercase comparison so `sodanco` matches `Só Danço Samba` in ALL modes. This
rides the same LOWR/lower-name path the verifier uses.

## Non-goals (this branch)

- Pinyin / transliteration postings (index format change — separate effort)
- Auto-fuzzy-on-empty without user opt-in (product decision deferred)
- Content matching

## Test plan

- Unit: Damerau-Levenshtein table tests; deletion-neighborhood generator;
  parser accepts/strips `~` and plumbs `--fuzzy`
- Golden ranking: `tonic~` ranks `tonic*` above `sonic*`; prefix above infix;
  exact tier strictly above fuzzy tier when both present
- Planner: short terms decline fuzzy; caps respected; trace mode reported
- Full package suite green; build + vet clean

## Files touched

- `cmd/seekfs/main.go`: query parsing, distance + neighborhood helpers,
  fuzzy planner stage, response/trace fields
- `cmd/seekfs/search-syntax docs`: document `term~` / `--fuzzy`
- new `cmd/seekfs/fuzzy_test.go`
