# seekfs v1.10.3

An internal refactor and test-coverage release. Search, indexing, and the
service behave the same as v1.10.2; the visible changes are two small
compatibility cleanups.

## Changed

- **The direct builder subcommand is now `direct`** (was `direct-v9`). The old
  name is still accepted as an alias, so existing scripts keep working.

- **Service environment variables dropped the `V9` qualifier**: `SEEKFS_V9_*`
  is now `SEEKFS_*` and `SEEKFS_DIRECT_V9_*` is now `SEEKFS_DIRECT_*`. The old
  spellings are still read, so no configuration change is required; prefer the
  new names in new scripts.

- **Removed the deprecated `-skip-startup-sync` service flag.** It had been a
  documented no-op (startup WAL replay and catch-up always run). A service
  launched with the flag now fails to start, so remove it from any launch
  script or service configuration that still passes it.

- **Removed the legacy 5-part posting-rank-bounds decode.** Indexes written by
  early v9 builds still load and answer correctly; they simply lose the
  precomputed extension/component rank bounds and fall back at query time.
  Rebuild (`index-volumes` or `index-usn`) to restore the optimization.

## Notes

- The large `main.go`, `query_planner.go`, and `query_planner_test.go` are split
  into focused files, and the direct topology/atomic-write and service command
  dispatch functions are decomposed. These are pure moves within `package main`
  and change no behavior.

- Index load/save errors now name the index file, so a failed load reports the
  path instead of a bare format error.

- The frontend's pure helpers (`normalizeLiveQuery`, `formatSize`, `formatDate`,
  `escapeHtml`, `sortSupported`, `debounce`) live in `ui_frontend/{query,util}.js`
  and are covered by dependency-free `node --test` tests in CI.

- The `seekfs_ui`-tagged Go tests (which CI's plain `go test ./...` never ran)
  now run in CI, and the `-race` subset was widened from 8 to 20 tests covering
  overlay mutation, persistence, WAL replay, background builds, and watch deltas.

- The runtime service environment knobs are now documented in `docs/CONFIG.md`.

- The codebase is gofmt-clean and CI enforces it.

- CI gates on `staticcheck` and `govulncheck` and reports statement coverage.

- Dependencies were updated (`golang.org/x/text` v0.42.0, `golang.org/x/sys`
  v0.48.0, `wails/v2` v2.16.0); `govulncheck` reports no reachable
  vulnerabilities. Building from source needs Go 1.26.3, which the `go`
  directive in `go.mod` fetches automatically.

- Pushing a `v*` tag builds and publishes the release zip plus a SHA-256
  checksum (`docs/RELEASE.md`).

## Validation

- `go test ./...` (67% statement coverage), `go vet ./...`, staticcheck,
  govulncheck, and `go test -tags "seekfs_ui production" ./cmd/seekfs` are green.

- `scripts/ci.ps1` runs those gates locally and the `Windows CI` workflow calls
  it, so local and CI runs exercise the same commands.

- The frontend unit tests run via `node --test` on explicit files.

- The refactors were verified as pure moves: declaration sets and function
  bodies match the originals, and the existing parity/hash tests
  (`TestDirectCanonicalDerivedParity`, `TestDirectRankFamiliesMatchComparator`,
  the overlay/oracle matrices) are unchanged and green.

## Upgrade

Drop-in for v1.10.2. No index rebuild is required.

- Remove `-skip-startup-sync` from any launch script or service configuration.
- `direct-v9` and the `SEEKFS_V9_*` / `SEEKFS_DIRECT_V9_*` variable names still
  work as aliases; migrate to `direct` and the unqualified names at your
  convenience.
- Rebuild indexes only if you want the extension/component rank-bounds
  optimization back on indexes written by early v9 builds.
