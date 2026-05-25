# Repository Instructions

- Do not push planning, scoping, or research-only commits to the public remote.
  Push only tangible code changes, feature work, fixes, tests, docs tied to
  implemented behavior, or release artifacts/metadata.
- Dogfood seekfs for local file discovery in this repository whenever practical.
  Prefer `seekfs search`/`seekfs count` for finding files by name or path, using
  the resident service. Use `rg` as the fallback for text-content search,
  precise line matches, or when the local seekfs service/binary is unavailable.
