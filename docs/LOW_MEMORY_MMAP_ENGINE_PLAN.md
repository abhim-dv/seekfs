# Low-Memory mmap Engine Plan

## Target

Build a seekfs mode that keeps private memory under 1 GB while preserving
roughly 50-150 ms query latency for common selective filename/path searches
across the current C: and F: indexes.

This is a different profile from the current resident engine. The current
resident engine optimizes for single-digit millisecond latency by keeping most
records, strings, child ranges, FRN lookup arrays, name order, and candidate
postings in Go heap. The low-memory engine should make the index mostly
disk-backed and let Windows file cache hold hot pages.

## Baseline To Beat

Current approximate baseline from the resident UI service:

- Entries: about 28.3M across C: and F:
- seekfs service private bytes: about 5.3 GB before FRN split, about 4.7 GB
  after split-array FRN index
- Everything64 private bytes: about 3.1 GB on the same machine
- Representative seekfs service p95: about 5-6 ms

Low-memory target:

- Private bytes: less than 1 GB
- Representative service p95: about 100 ms, with selective queries preferably
  below 50 ms
- Correctness unchanged: candidate indexes only narrow records; final
  name/path verification remains authoritative.

## Design Direction

Move from fully resident Go slices/maps to a disk-backed, block-compressed,
mmap-oriented engine.

Keep in RAM:

- Volume metadata and schema version.
- Small term/component/posting directories.
- Small recent USN overlay.
- Bounded LRU caches for posting blocks, decoded name blocks, and path
  reconstruction.
- Optional hot summaries for very common extensions/components.

Move out of Go heap:

- Compact records.
- FRN lookup arrays.
- Parent/child/subtree arrays.
- Name and lowercase blobs.
- Name order/rank.
- Extension/component/trigram postings.

Use mmap or direct block reads for those structures. The process private heap
should stay bounded; Windows file cache can absorb hot pages.

## Proposed On-Disk Layout

Use a column-store style index directory or a revised `.gsi` section layout.

Suggested files/sections:

```text
records.meta
records.frn.u64
records.parent.u32
records.flags.u8-bitset
records.mode.u32
records.size.varint-or-u64
records.mtime.delta-or-i64

names.blocks
names.block_index
lower.blocks
lower.block_index

order.name.u32
order.rank.u32 or omitted in lowmem

frn.sorted.u64
frn.sorted_ids.u32

children.offsets.u32 or compressed blocks
children.ids.u32 or compressed delta blocks
subtree.start.u32
subtree.end.u32
subtree.order.u32

postings.ext.dir
postings.ext.blocks
postings.component.dir
postings.component.blocks
postings.ngram.dir
postings.ngram.blocks
```

The first implementation does not need all files split physically. A single
container with offsets is fine if it supports mmap-friendly random access and
block-level loading.

## Query Model

Keep the current semantic model:

- Whitespace splitting is strict.
- `path:` enables path verification; it does not rewrite terms into `ext:`.
- `.foo` remains a literal substring unless the user explicitly uses `ext:foo`.
- Volume terms constrain volume routing.
- Candidate indexes are approximate and must be verified.

Low-memory query flow:

1. Parse query once.
2. Pick the most selective candidate source using directory stats.
3. Read/decode only needed posting blocks.
4. Intersect/merge candidate IDs in memory.
5. Verify basename/path constraints by reading name/parent columns through mmap.
6. Reconstruct full paths only for returned rows or where parent-chain checks
   cannot decide.

## Posting Block Strategy

Postings should not be decoded as one huge list by default.

Use block-compressed postings:

- Fixed-size logical blocks, e.g. 128-1024 record IDs per compressed block.
- Delta-coded IDs inside blocks.
- Directory entry stores gram/component/ext key, total count, block offsets,
  min/max record ID, and optionally min/max name-rank.
- Decode blocks lazily and cache decoded blocks with a hard cap.

Possible encodings:

- Delta varint initially, because code already exists.
- Later evaluate StreamVByte, Frame-of-Reference, or Roaring-style containers
  for better speed/space tradeoffs.

## Cache Policy

Introduce explicit low-memory cache caps.

Suggested defaults:

```text
SEEKFS_MEMORY_MODE=lowmem
SEEKFS_POSTING_CACHE_MB=128
SEEKFS_NAME_BLOCK_CACHE_MB=64
SEEKFS_PATH_CACHE_MB=64
SEEKFS_TOTAL_CACHE_MB=256
```

Cache candidates:

- compressed posting blocks
- decoded posting blocks
- decoded name/lowercase blocks
- reconstructed path strings for returned/hot rows
- component directory lookups

Do not let caches grow unbounded. Avoid Go maps with millions of entries in
low-memory mode.

## Path Verification Without Resident Full Paths

Path verification should use parent-chain traversal over mmap columns:

1. Check basename first.
2. If path terms remain, walk `parent_id` chain.
3. Decode parent names from name/lowercase blocks.
4. Stop early when all terms are found.
5. Cache decoded parent chains/path fragments for hot rows.

This preserves correctness without a full-path trigram index.

## Child/Subtree Handling

The resident engine currently uses large child/subtree arrays. Low-memory mode
should avoid loading all of them into heap.

Options:

- mmap child offset/id arrays directly
- block-compress child lists by parent directory
- keep only top-level/high-fanout directory summaries in RAM
- decode subtree blocks on demand

For broad directory queries like `path:Windows`, use directory-component
postings to identify the root directory, then return top ordered descendants by
reading subtree/order blocks. If a subtree is too large, return progressive top
results rather than materializing every descendant.

## Live Updates

Avoid mutating mmap base files in place.

Use:

- immutable base index files
- small in-memory recent overlay
- tombstone bitset/overlay for deletes
- periodic background compaction/rebuild

Correctness requirements:

- new files appear via overlay
- deleted files suppress base matches
- renamed files verify against overlay path metadata
- stale USN journal still triggers rebuild recovery

## Implementation Phases

### Phase 1: Finish Resident Compaction

Before building mmap mode, finish cheap resident reductions:

- Split padded structs into parallel arrays where safe.
- Pack deleted flags into bitsets.
- Evaluate sparse lowercase offsets.
- Evaluate optional `nameRank` removal or lazy rank construction.
- Measure after each change.

Evidence required:

- `seekfs loaded --json` memory breakdown
- process private bytes via perf counters
- representative service benchmark

### Phase 2: mmap Records And Names

Add read-only mmap-backed accessors for:

- FRN
- parent ID
- mode
- size
- mtime
- deleted flag
- name/lowercase string blocks

Keep existing `Index`/`PackedRecords` API where possible, but route low-memory
mode through mmap-backed implementations.

Gate:

```text
SEEKFS_MEMORY_MODE=lowmem
```

### Phase 3: mmap FRN And Name Order

Move sorted FRN arrays and name order/rank out of heap.

Options:

- mmap `frn.sorted.u64` and `frn.sorted_ids.u32`
- mmap `order.name.u32`
- omit `rank` in low-memory mode unless a query needs top-K ranking that cannot
  be answered by scanning `order.name.u32`

### Phase 4: Disk-Backed Postings

Move ext/component/trigram postings to compressed posting blocks.

Keep in RAM only:

- key -> directory entry
- count/selectivity stats
- hot block cache

Candidate intersection should decode only relevant blocks.

### Phase 5: Disk-Backed Child/Subtree

Move child/subtree arrays to mmap or compressed block format.

Keep only tiny root/component summaries in RAM.

Benchmark broad path queries:

- `path:Windows`
- `path:C: Users`
- `path:C: AppData`
- `path:F: workspace`
- `path:node_modules`

### Phase 6: Low-Memory Benchmark Matrix

Required query matrix:

```text
.nrrd
.raw
.pdf
.json
.dll
.exe
.opencode
path:F: .nrrd
path:F: .raw
path:F: .pdf
path:C: .exe
path:C: Users
path:C: AppData
path:F: workspace
path:Windows
path:node_modules
path:Downloads .nrrd
path:F: Downloads .nrrd
path:C:.nrrd
zzzz-no-hit-seekfs
```

Measure:

- process private bytes
- working set
- Go heap alloc/sys
- per-volume known bytes
- p50/p95/max service latency
- backend latency
- source/candidate counts
- cache hit/miss rates

## Literature And Production References

Use these as design references, not as exact implementations.

### REI: Regular Expression Indexing For Log Analysis

- arXiv: `2510.10348`
- URL: https://arxiv.org/abs/2510.10348
- Key takeaway: small configurable n-gram filters plus final verification can
  produce large speedups with low extra space. Avoid full inverted indexing
  when memory is constrained.

### Russ Cox Code Search

- URL: https://swtch.com/~rsc/regexp/regexp4.html
- Key takeaway: trigram candidate retrieval plus verification is a practical
  architecture for substring/regex search.

### Zoekt

- URL: https://github.com/sourcegraph/zoekt
- Memory optimization discussion:
  https://sourcegraph.com/blog/zoekt-memory-optimizations-for-sourcegraph-cloud
- Key takeaway: production trigram search relies on mmap/index files and careful
  memory behavior rather than keeping every structure in application heap.

### Tantivy

- Project: https://github.com/quickwit-oss/tantivy
- Architecture:
  https://github.com/quickwit-oss/tantivy/blob/main/ARCHITECTURE.md
- Overview: https://fulmicoton.com/posts/behold-tantivy/
- Key takeaway: mmap-backed search segments are a standard way to get fast
  queries with low private memory.

### Compressed Inverted List Caching

- Paper: https://research.engineering.nyu.edu/~suel/papers/listcaching.pdf
- Key takeaway: compressed postings and cache policy should be designed
  together; caching decoded or compressed blocks is a major performance lever.

### Block-Max WAND / Block-Level Skipping

- Paper: https://research.engineering.nyu.edu/~suel/papers/bmw.pdf
- Overview: https://weaviate.io/blog/blockmax-wand
- Key takeaway: block summaries let query processing skip work. seekfs does not
  need ranked retrieval, but block summaries are still useful for avoiding
  decoding broad postings.

### N-Gram Selection Strategies

- arXiv reference from REI bibliography: `2504.12251`
- URL: https://arxiv.org/abs/2504.12251
- Key takeaway: gram selection and granularity should be configurable by memory
  budget and workload. This is relevant for choosing which component/name grams
  stay indexed in low-memory mode.

## Open Questions

- Can low-memory mode omit `nameRank` and rely on scanning mmap name order for
  top-K results?
- What posting block size gives the best p95 under a 256 MB cache cap?
- Is lowercase storage best represented as:
  - full lower blob,
  - sparse lower overrides,
  - ASCII casefold-on-read,
  - or block-compressed lower names?
- How much path verification latency comes from parent-chain random access once
  parent/name columns are mmap-backed?
- Should low-memory mode use bigram filters for very low space, then verify,
  instead of full trigram postings?

## Success Criteria

Low-memory mode is successful when:

- private bytes stay below 1 GB after C: and F: are loaded
- representative service p95 is below 150 ms
- selective queries are commonly below 50 ms
- correctness parity tests pass against full scan
- current fast resident mode remains available and does not regress

