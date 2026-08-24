# seekfs v1.2.1

This release fixes a performance cliff on broad single-term name queries and
tightens the file-discovery guidance for repository agents.

## Fixes

- **Broad single-term queries no longer fall to the full scan**. Queries such
  as `acme`, `exxon`, or `discovery` previously took 1-2 seconds because
  every fast lane declined: the selective trigram lane rejected their common
  grams (omitted-common), the ranked top-K lane was skipped whenever any
  volume carried overlay state (always true on a live service), and the
  exact PNGC intersection lane was gated to multi-term queries only. The
  query then landed in the full compact-name-order scan over every record.
  Two planner changes fix this:
  - The ranked top-K lane now runs under overlays. It already filtered
    hidden base IDs per candidate, and `mergeGlobalVerifiedEntries`
    re-merges overlay matches, so the old guard was over-conservative.
  - Single-term queries can now be rescued through the complete PNGC
    gram-intersection lane when the selective lane declines, with a 2M
    driver-posting guard, plus explicit decline traces on previously
    silent exits for diagnosability.

  Measured on production (26.9M entries): `exxon` 1229ms -> 89ms,
  `discovery` 853ms -> 129ms, `acme` 1425ms -> ~300ms, all serving
  from posting lanes (`global:filename-pngc` / `global:filename-pngr`)
  instead of bounded scans.

- Volumes whose indexes predate the PNGC companion section should be
  rebuilt once (`seekfs index-volumes -volume C:`) so both fast lanes are
  available; the ranked lane requires complete gram postings.

## Other

- Clarified AGENTS guidance for indexed filename/path discovery (plain
  terms, `--under` scoping, flags before the query).
