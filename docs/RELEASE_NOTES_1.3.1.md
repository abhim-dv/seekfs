# seekfs v1.3.1

Refines multi-term fuzzy chaining based on live usage.

## Changes

- Multi-term fuzzy chaining now fires only when the exact query returns
  zero results (or with an explicit `--fuzzy`/`~` marker). Query rewrites
  are invasive; a query that already matched anything is left alone.
- Within zero-result queries, every eligible term becomes a rewrite
  candidate regardless of its solo-match health: typos can collide with
  real substrings ("llog" appears in dozens of legitimate names), and very
  popular terms' solo lookups decline outright. Ordered trials (max 4)
  validated by exact re-searches decide what is surfaced.
- Known limitation, accepted: typos whose corrupted form is itself a real,
  reasonably common substring in combination with the other terms may
  return no rewrite (e.g. some `llog` queries).

Single-term auto-fuzzy behavior (<10 results appends close matches below
exact hits) is unchanged.
