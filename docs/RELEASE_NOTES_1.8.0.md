# seekfs v1.8.0

Directories now report the recursive size of their contents, matching
Everything's folder-size behavior, and `size:` filters see that aggregate.

## Highlights

- **Recursive directory sizes.** Folder results no longer report `0`. The v9
  index gains a `SUBS` section: one `uint64` per record holding the summed size
  of every file in that record's subtree (0 for plain files, which report their
  own size). The offline builder computes it in a single post-order pass over
  the children graph it already walks for `SUBT`, and the persist path
  recomputes it on every rewrite, so a folder's size refreshes when the index
  is folded.

- **`size:` filters apply to directories.** Because directory entries now carry
  their aggregate size, `size:>1gb` (and the other operators) match a folder by
  its recursive content size instead of always excluding it.

- **Backward compatible.** `SUBS` is an additive v9 section; readers that do not
  know it skip it and load the index normally, and an index written before this
  release decodes with directory sizes of 0. No format version bump.

## Notes

- The USN change journal carries no file size, so a folder's recursive size
  tracks live, size-bearing changes only at the next persist/rebuild, like file
  sizes already do. Deletes and new zero-byte files between persists can make a
  displayed folder size lag.
- The `SUBS` section is 8 bytes per record (about 78 MB for 9.8M C: records and
  137 MB for 17M F: records). In low-memory mode it is file-backed through the
  mmap; in resident mode it is loaded into the heap.

## Testing

- New `TestDirectV9DirectorySubtreeBytes` builds a known tree and checks the
  persisted aggregate, the reported directory entry size, and `size:` filtering.
- `go test ./...` and `go vet ./...` are green.

## Upgrade

Drop-in for v1.7.2. Existing v9 indexes load with directory sizes of 0 until the
next rebuild or persist; run `index-volumes` (or delete and rebuild the index)
to populate `SUBS` immediately.
