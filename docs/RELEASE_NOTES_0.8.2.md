# seekfs v0.8.2

This patch release rolls up the prior release-candidate CLI compatibility work
and improves resident-service reliability, stale-index recovery, and
repo-scoped path query planning.

## Highlights

- Repo-scoped known-file searches now prefer exact filename postings before
  materializing a subtree or scanning broad path terms.
- Path queries with dotted extension terms, for example `Downloads .docx`, now
  use extension postings and verify the remaining path terms.
- The service now rebuilds a volume index when its USN checkpoint is no longer
  recoverable from the journal.
- Transient named-pipe failures are retried, and pipe access-denied errors now
  point users toward refreshing the service ACL.
- `service-index-usn` refreshes the loaded resident index after a rebuild.
- Search-like invocations may omit the `search` subcommand, so commands such as
  `seekfs --under F:\workspace\project "main.go"` run directly instead of
  returning syntax guidance.
- Bare wildcard filename terms such as `*_test.go` are treated as filename
  globs without requiring an explicit `glob:` prefix.
- Agent-facing README/help/search-syntax guidance now documents commandless
  search and implicit filename glob behavior.
- CLI compatibility tests and the PowerShell integration test cover
  commandless `--under` search and implicit wildcard queries.

## Validation

```text
go test ./...
go vet ./...
.\test_seekfs_cli.ps1
```

## Compatibility

- Existing v8 indexes remain compatible.
- Stale USN indexes may be rebuilt automatically by the service when the journal
  can no longer replay from the stored checkpoint.
- The release artifact is unsigned.

## Notes

- This is an independent project and is not affiliated with Everything or
  voidtools.
