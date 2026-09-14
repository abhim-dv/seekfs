# seekfs v1.10.0

Full-volume index rebuilds are several times faster and use far less memory,
and `regex:` queries can now pre-filter candidates by the literal text the
pattern must contain instead of scanning every record.

## Highlights

- **Much faster, lower-memory builds.** A full rebuild of a real
  9.9M/17.1M-record volume dropped from 7m19s/13m06s to about 1m08s/1m54s, and
  peak memory from ~3.9/8.5 GiB to ~2.3/4.4 GiB. The rank builders
  (name/size/modified/extension/type/path) now compute their sort keys once
  instead of re-parsing records and re-lowercasing whole paths inside every
  comparison; the path rank uses an allocation-free memo. The name-trigram
  builder merges a whole gram at a time and groups a spill buffer in an
  open-addressed table, and the record table deduplicates names by their source
  token id instead of hashing every name string.

- **Bounded-memory builds by default.** Real volumes build their name trigrams
  with the external merge builder, whose peak memory is flat instead of growing
  with the index. Set `SEEKFS_NAME_GRAM_EXTERNAL=0` to force the in-memory
  builder.

- **Bounded regex pre-filter.** `regex:` queries are pre-filtered by the
  literal runs that every match must contain, expanded as a disjunction of
  alternatives, so a pattern such as `^.*\.(go|rs|py|js)$` bounds-scans a
  candidate subset instead of every record. The rewrite only widens the match
  language, so it can never drop a real result.

## Notes

- Non-ASCII names are case-folded with the same Unicode lowercasing the query
  side uses, so searches for names with accented or other non-ASCII characters
  match. The previous selective builder folded only ASCII and could miss those.

- The external builder spills to the system temp directory by default; set
  `SEEKFS_NAME_GRAM_SPOOL_DIR` to place spill files elsewhere. See
  `docs/CONFIG.md` for the build-tuning variables.

- Regex pre-filter extraction declines patterns it cannot prove sound
  (non-ASCII patterns, backreferences, `\K`, conditionals), which simply falls
  back to the existing scan.

## Validation

- `go test ./...` and `go vet ./...` are green.

- Build time and peak measured on the real C: (9.9M records) and F: (17.1M
  records) indexes.

- Name-gram output verified byte-identical between the external and in-memory
  builders, including forced multi-level spills and repeated-gram names; the
  path rank verified equal to the original comparator on real data.

## Upgrade

Drop-in for v1.9.0. No index rebuild or configuration change is required.
Existing indexes load and search unchanged. Rebuild (`seekfs index-volumes` or
`index-usn`) to pick up the faster builder, the lower peak memory, and the
Unicode case-fold fix.
