# seekfs v0.8.3

This patch release hardens large resident indexes and scoped searches found
during dogfood use after v0.8.2.

## Highlights

- Compact resident indexes now widen on-disk parent/name references past the
  old 24-bit limit, preventing `compact index too large for packed record
  format` failures on very large volumes.
- Oversized resident WAL files now trigger rebuild recovery at startup instead
  of forcing an expensive replay attempt.
- Hung resident `search`/`status`/`info` calls now time out client-side instead
  of blocking indefinitely on a bad pipe exchange.
- Resident saves and rebuilds now release heap pages so memory returns closer
  to the steady-state footprint after wide-index persistence.
- Search flags may appear before or after the query, so commands like
  `seekfs search main.go --under F:\workspace` scope the query as expected.
- Scoped filesystem fallback is bounded so no-hit `--under` searches on large
  roots cannot block the resident service behind a long recursive scan.
- Private dogfood terms were removed from regression test fixtures.

## Validation

```text
go test .\...
go vet .\...
```

## Compatibility

- Existing v8 indexes remain compatible.
- Large compact indexes saved by this release may use widened record references;
  older binaries should be upgraded before reading indexes created on very large
  volumes.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
