# NTFS Component Trigram Engine Plan

This plan is for moving seekfs toward an NTFS-native filename/path search engine
that can match or exceed Everything on selective filename and path-component
queries without building a memory-heavy full-path substring index.

## Goal

Build a resident search path where:

- Query semantics are parsed once and never changed for optimization.
- Candidate sources narrow record IDs but final name/path verification remains
  authoritative.
- Fast substring search is driven by basename/component indexes, not duplicated
  full-path strings.
- Broad/common queries fall back to scan paths when candidate indexes are not
  selective enough.
- Resident memory, build time, and live update behavior are measurable and
  bounded.

## Non-Goals

- Do not make a naive full reconstructed path trigram index the default.
- Do not rewrite `.foo` or path terms into `ext:` unless the user explicitly
  used `ext:`.
- Do not block service startup on large experimental indexes.
- Do not make UI-specific behavior part of backend query semantics.

## Current Evidence

Real `.gsi` prefix sizing showed:

- Name trigrams projected for C+F: about 876 MB compressed postings.
- Full-path trigrams projected for C+F: about 3.6 GB compressed postings,
  before map overhead, caches, and build-time raw postings.
- Full-path trigrams duplicate parent directory grams across every child, which
  fights the NTFS component/parent-reference model.

Prototype benchmarks showed:

- Selective terms like `.opencode`, `.raw`, `.pdf`, `.nrrd` benefit strongly.
- Common terms like `workspace` or `plain` can be scan-equivalent or worse.
- A selectivity threshold is required.

## Architecture

### 1. Semantic Query Model

Keep parsing strict and simple:

- Split tokens on whitespace.
- Preserve literal dotted substring terms.
- `ext:` remains an explicit extension filter.
- `path:` enables path verification.
- Drive terms such as `F:` constrain volume routing.

Every token becomes a semantic constraint. The planner may choose candidate
sources, but it must not alter the meaning of the query.

### 2. Candidate Sources

Candidate sources should be interchangeable and verified later:

- extension postings
- exact basename/component postings
- name/component trigram postings
- directory component matches plus descendant expansion
- subtree or `--under` roots
- type/date/size metadata postings
- limited ordered scan
- broad parallel scan fallback

The planner should sort/select by estimated candidate count and avoid building
large postings for common terms.

### 3. Name Trigram Index

Add an opt-in resident basename trigram index:

- Gate with `SEEKFS_NAME_TRIGRAMS=1`.
- Store lowercased basename trigrams.
- Use record IDs as postings.
- Store postings compressed with delta-varint or a better compressed format.
- Use as a candidate source only.
- Always verify with `strings.Contains(lowerName, term)` or path verification.

Use it for:

- non-path filename substring queries
- path queries where a term appears in basename
- extension-like bare substrings such as `.nrrd`, `.raw`, `.pdf`
- dotted non-extension substrings such as `.opencode`

Skip it when:

- term length is less than 3
- query is case-sensitive
- estimated posting/intersection count exceeds threshold
- index is unavailable or not yet built

### 4. Component Trigram Index

Add a component-aware index after name trigrams are stable:

- Treat each NTFS basename as a component.
- Directory components should be able to expand to descendants.
- File components should produce the file record directly.
- Matching directory component IDs can be expanded using child ranges/subtree
  intervals when available.

This supports:

- `path:Downloads`
- `path:AppData`
- `path:.opencode`
- `path:reaper_base_new_workspaces`
- `path:Downloads .nrrd`

Final verification must still walk the parent chain or reconstruct the path.

### 5. Avoid Default Full-Path Trigrams

Full-path trigrams should remain experimental because:

- Parent path text is repeated for every child.
- Real index estimates put compressed postings in multi-GB territory.
- Directory renames invalidate large subtrees.
- Build-time raw postings can cause high heap spikes.

Keep any full-path trigram implementation behind a separate flag such as
`SEEKFS_FULL_PATH_TRIGRAMS=1`.

## Memory And Build Strategy

### Problem With Naive Build

The prototype builds:

```text
map[trigram][]uint32 -> compressed postings
```

This is simple but causes large temporary heap spikes on real indexes.

### Target Build Strategy

Use segmented immutable builds:

1. Split records into bounded chunks.
2. Each worker builds a local gram map for its chunk.
3. Compress chunk-local postings.
4. Discard raw maps immediately.
5. Store `[]trigramSegment`.
6. Query across segments and merge/intersect candidate IDs.

Avoid concurrent writes to one global map.

### Concurrency Rules

- Use bounded worker pools.
- Do not build all volumes in parallel by default.
- Build trigram indexes in the background after the service is query-ready.
- Cap memory while building.
- Parallelize verification only when candidate count exceeds a threshold.

## Live Updates

Avoid in-place mutation of giant compressed postings.

Use:

- immutable base trigram segments
- small recent overlay for USN/WAL changes
- fallback verification over `recentIDs`
- periodic background segment rebuild or merge

Correctness requirements:

- creates add recent overlay entries
- deletes suppress old base matches
- renames update overlay and deletion state
- directory renames preserve final path verification correctness
- stale journals trigger existing rebuild recovery

## Planner Integration

Add planner stages carefully.

Recommended order:

1. Hard constraints: volume, `--under`, type, ext, size/date.
2. Exact component/name postings.
3. Selective name/component trigram candidates.
4. Directory component descendant expansion if selective.
5. Existing path subtree / path-root candidates.
6. Limited ordered scan.
7. Broad parallel scan fallback.

Do not use a candidate source when its estimate is worse than scan.

Suggested threshold inputs:

- smallest gram posting count
- estimated intersection count
- record count per volume
- query limit
- whether a stricter source already exists
- whether candidate term is in path mode or name mode

## Verification

Candidate indexes are approximate.

Always verify:

- basename substring via lowercased compact name blob
- path substring via parent-chain component checks or reconstructed path
- extension via `filepath.Ext`
- type/date/size through record metadata
- NOT/OR groups through existing parsed-query matchers

Avoid reconstructing full paths unless returning results or when no cheaper
parent-chain verification is available.

## Tests

Add deterministic coverage for:

- `.opencode` as literal substring
- `ext:opencode` remains exact extension filter
- `path:C: .opencode`
- `path:Downloads .nrrd`
- `path:F: .nrrd`, `.raw`, `.pdf`
- `path:AppData` and directory-component queries
- common terms that should fall back
- no-hit queries
- short terms under 3 characters
- case-sensitive queries decline trigram path
- stale/recent overlay records
- deleted records are excluded
- candidate parity with full scan

Preferred locations:

- planner/candidate behavior: `cmd/seekfs/query_planner_test.go`
- resident cache/memory flags: `cmd/seekfs/service_cache_test.go`
- focused trigram structures: `cmd/seekfs/trigram_index_test.go`

## Benchmarks

Benchmark against both seekfs baseline and Everything.

Query categories:

- filename selective: `.nrrd`, `.raw`, `.pdf`, `.opencode`
- folder component selective: `path:opencode`,
  `path:reaper_base_new_workspaces`
- mixed path/name: `path:Downloads .nrrd`, `path:F: .pdf`
- broad/common: `path:Users`, `path:AppData`, `path:workspace`
- no-hit: uncommon random substrings
- strict parsing cases: `path:Downloads.nrrd`, `path:C:.nrrd`

Metrics:

- p50/p95 latency
- result count and first page parity
- resident heap after startup
- trigram posting bytes
- trigram key count
- build duration
- fallback rate
- candidate count before verification

## Everything Comparison

Use Everything as a behavioral and latency reference, not as a strict
implementation target.

For each query:

- run seekfs service query
- run Everything query if CLI is available
- compare first-page paths where semantics align
- record latency
- record mismatches and explain syntax differences

Focus on matching or beating Everything for selective slow paths first. Broad
path-only queries may remain scan-bound until parent-chain verification and
memory layout are further optimized.

## Rollout

### Phase 1: Opt-In Name Trigrams

- Implement `SEEKFS_NAME_TRIGRAMS=1`.
- Build index in resident startup or background.
- Add memory accounting.
- Use selectivity thresholds.
- Keep disabled by default.

### Phase 2: Component Trigrams

- Reuse basename trigram index for components.
- Add directory descendant expansion.
- Verify full path through parent chain.
- Benchmark path-component queries.

### Phase 3: Segmented Build

- Replace global raw map build with compressed segments.
- Add chunked worker pool.
- Add build memory caps.
- Add background build readiness status.

### Phase 4: Recent Overlay

- Add mutable overlay for live updates.
- Merge overlay or rebuild segments in background.
- Add USN replay correctness tests.

### Phase 5: Default Enablement Decision

Enable by default only if:

- resident memory increase is acceptable
- build does not block startup materially
- selective query p95 improves significantly
- common query fallback avoids regressions
- live update overlay is correct

## Risks

- Heap spikes during build.
- Common grams causing worse query latency.
- Directory rename correctness.
- Increased service startup time.
- Query planner accidentally changing semantics.
- UI/IPC masking backend improvements.

## Success Criteria

- Selective dotted/name queries are consistently sub-200 ms on real C/F indexes.
- `path:F: .nrrd/.raw/.pdf` is close to `ext:` runtime after verification.
- `.opencode` and similar middle substrings match Everything-like behavior.
- Common path terms do not regress.
- Memory increase is measured and bounded.
- Tests catch parser, planner, and candidate parity failures before UI dogfood.
