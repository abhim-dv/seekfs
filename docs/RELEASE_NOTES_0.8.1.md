# seekfs v0.8.1

This patch release fixes resident-service memory growth and repo-scoped search
latency regressions found while dogfooding v0.8.0.

## Highlights

- Fixed resident `NameBlob` growth during live USN updates when file names do
  not change.
- Added a resident repack step after catch-up and background persistence when
  packed name blobs have grown beyond expected size.
- Reduced default resident memory by making subtree interval arrays and path
  component 3-gram postings opt-in:
  - `SEEKFS_SUBTREE_INTERVALS=1`
  - `SEEKFS_PATH_GRAMS=1`
- Reordered repo-scoped planning so selective filename, extension, and glob
  postings can drive `--under` queries before materializing a subtree.
- Improved stale-volume handling: searches skip stale indexes that cannot match
  the query root, and report a clear stale-index error when the matching volume
  is stale.
- Improved CLI guidance when agents omit the `search` subcommand.

## Dogfood Results

On the maintainer's machine after these fixes:

```text
seekfs search --under F:\git\seekfs dogfood
  before: about 34 seconds
  after:  under 1 second

seekfs search --under F:\git\rnd-lejepa-encoder-v1 retrieval
  before: about 19 seconds
  after:  under 1 second
```

The live service's steady working set dropped from the v0.8.0 leak case to
roughly 3 GB after disabling the optional acceleration structures by default.

## Compatibility

- Existing v8 indexes remain compatible.
- Rebuild stale indexes when `loaded --json` reports that a checkpoint is before
  the first valid USN.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
