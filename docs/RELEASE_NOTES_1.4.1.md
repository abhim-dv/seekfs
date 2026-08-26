# seekfs v1.4.1

Fixes the `ext:` filter scan cliff surfaced by `seekfs watch` in v1.4.0.

## Highlights

- **Term + `ext:` queries are fast again.** Previously any query carrying an
  `ext:` filter (e.g. `acme ext:pdf`) fell to the global bounded-scan lane,
  taking 4-22 seconds. The name-planner gates rejected `ext:` outright, and
  the top-posting lane could not use the extension to bound its work.

- **New ext-driven fast lane.** Single-term queries with exactly one `ext:`
  filter now drive from the extension posting (rank-ordered, block-skippable)
  and verify the term per record, bounding the walk to the extension's own
  population instead of the term's full gram posting.

  ```
  acme ext:pdf        12.9s  ->   106 ms
  C: acme ext:pdf     19.4s  ->   253 ms
  acme ext:docx       13.6s  ->   168 ms
  ```

- **`seekfs watch` with `ext:` filters now polls the fast lane** instead of
  multi-second bounded scans every tick.

- All remaining filters are verified in the top-posting walk, and count/search
  parity is preserved (results are re-verified downstream before returning).

## Fixes

- `filenameTrigramCandidates` no longer rejects `len(pq.Exts) > 0`.
- `globalNameQuerySupported` no longer requires `len(pq.Exts) == 0`.
- Added `completeExtTermTopPosting` for the single-term + single-ext case.
- `completeFilenameTopPosting` verifies every filter via `recordMatchesNonPath`.
