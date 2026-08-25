# seekfs v1.3.0

This release teaches fuzzy search to work inside multi-term queries, and
hardens the resident service against two operational failure modes.

## Highlights

- **Multi-term fuzzy chaining (`reprot pdf` now finds `report.pdf`)**.
  When a multi-term name query returns fewer than 10 results, terms that
  match nothing on their own - or almost nothing, masked by incidental
  substrings - are replaced with their best close-match variants and the
  query is re-run once through the normal exact pipeline. Terms that match
  plenty stay as hard constraints, so `pdf` in `reprot pdf` keeps filtering.

  Design notes:
  - Solo term health is measured with a bounded trigram candidate lookup;
    terms too popular to evaluate are left alone rather than guessed at.
  - Variant candidates are ranked by Damerau-Levenshtein distance, then a
    cheap gram-selectivity plausibility score; the top two variants per
    broken term become ordered trials (max 4 total).
  - Each trial re-runs the full exact pipeline - PNGC posting intersection,
    ranking, overlay merging - so only verified results are ever surfaced,
    and the response carries `fuzzy: true`.

- **Stale-volume recovery** now retries failed rebuilds with capped backoff
  instead of giving up after the first attempt (shipped mid-cycle with
  v1.2.x hardening).

## Behavior

| Query | Before | After |
|---|---|---|
| `reprot pdf` | 0 results | Report PDFs, `fuzzy: true` |
| `acme log` | exact hits | unchanged (exact, no fuzzy flag) |
| `netwrok` | single-term fuzzy | unchanged |

Counts (`seekfs count`) remain strictly exact; regex/glob/path-mode and
case-sensitive queries are never fuzzed; terms under 3 characters decline.
