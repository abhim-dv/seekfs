# seekfs v1.9.0

File sizes and dates stay current, folder sizes update live, and size sorting
ranks directories by their recursive content — the way Everything maintains
them.

## Highlights

- **Live file metadata.** The USN change journal reports *that* a file changed
  but carries no size or timestamp, so a created, modified, or renamed file
  used to report `size: 0` (and a zero date) until a full index rebuild — and
  the periodic persist folded that zero into the base index. Now the file is
  re-read from disk when the journal reports a change, at most once per file
  per batch, and the overlay carries its real size and modification time. This
  applies to live replay, startup catch-up, and WAL replay, matching how
  Everything re-reads file metadata on a journal event.

- **Live folder sizes.** When a file is created, modified, moved, or deleted,
  the size delta propagates to its ancestor directories. The deltas are
  published as an immutable snapshot so lock-free queries see a stable view,
  are applied to directory results and `size:` filters, and are cleared when
  the volume is persisted (the new base already includes them).

- **Directory-aware size ranking.** `sort:size` and the scalar `size:` ranges
  now rank a directory by its recursive subtree total instead of its stored
  `0`. The offline builder computes directory totals in a read-only pre-pass
  and feeds them to the size rank; the persist path builds the child graph and
  directory totals before the size rank and re-emits `SUBS`; and the query-side
  rank compares by effective size.

## Notes

- A directory created since the last index build reports 0 until it is folded
  into the base (its own size is not persisted yet).
- Because the journal has no size, a file's size updates when its change event
  is applied; a file being written can lag until it is closed, as with
  Everything.

## Validation

- Live-validated against the real C: volume: a created 12,345-byte file
  reported 12,345, a rename preserved it, a rewrite to 25,000 reported 25,000,
  and a folder holding a 25,000-byte file plus a 10,000-byte subdirectory
  reported 35,000, with `sort:size` ordering directories by that total.
- `go test ./...` and `go vet ./...` are green.

## Upgrade

Drop-in for v1.8.0. Rebuild the indexes (`seekfs index-volumes` or
`index-usn`) to populate `SUBS` and the directory-aware size rank immediately;
an existing v9 index without `SUBS` loads with directory sizes of 0 and ranks
directories by their stored size until the next persist.
