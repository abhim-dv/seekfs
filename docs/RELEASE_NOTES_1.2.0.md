# seekfs v1.2.0

This release adds a ranked fuzzy fallback tier for filename search, plus
resident-service reliability and observability work: automatic recovery of
stale volumes, an index-health surface in the service API, and a health
indicator dot in the UI.

## Highlights

- **Fuzzy fallback tier (`term~`, `--fuzzy`, and automatic on misses)**.
  When a single-term name query returns fewer than 10 results, close matches
  are appended strictly below the exact results. Candidates come from the
  existing PNGR name-trigram postings over a SymSpell-style
  deletion-plus-transposition neighborhood, are verified with bounded
  Damerau-Levenshtein distance (1 below 8 characters, 2 above), and are ranked
  by distance, then prefix alignment, then window position, then length delta,
  then normal rank. Zero-result queries trigger the tier automatically — no
  marker needed; `term~` or `--fuzzy` opts in explicitly and also tops up
  partially filled result sets. Measured ~50-100 ms on a 16.9M-entry volume;
  exact-rich queries never pay the cost because the tier only runs on
  underfill. Comparison-time accent folding (NFD mark stripping) and CJK
  fullwidth folding mean `sodanco` matches `So Danco` inside the tier.
- **Exact results always lead**. Fuzzy entries append strictly after every
  exact entry and the ordering is pinned by a regression test.
- **Stale-volume recovery loop**. Volumes that start stale (journal anomaly,
  transient open failure, failed startup rebuild) previously stayed broken
  until the next service restart. A background loop now retries the rebuild
  with exponential backoff (30s to 5m cap) and swaps the recovered volume in,
  restarting replay/persist loops automatically.
- **Service health surface**. The pipe `info` response and `loaded --json`
  report overall `health` (`ok` / `degraded` / `error`) with a human-readable
  reason: stale or errored volumes, persist failures, catch-up progress, and
  startup loading all classify explicitly. The UI shows a green/yellow/red dot
  in the bottom-right footer with the reason on hover.
- **Replay-loop hardening**. Loop pacing moved behind overridable delays so
  regression tests can exercise the replay loop; new tests pin USN journal
  validation (journal-id change, checkpoint-before-first-USN wrap, checkpoint-
  after-next-USN) and verify the loop marks unreachable volumes stale and
  shuts down without leaking goroutines.

## Notes

- Fuzzy matching is single-term, name-only, case-folded; regex/glob/path terms
  are never fuzzed. Terms shorter than three characters decline.
- The fuzzy candidate cap reuses `serviceNameTrigramCandidateMaxIDs`; volumes
  whose trigram sections are still building simply contribute no fuzzy results.

See `search-syntax.md` for query syntax.
