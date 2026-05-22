# Release TODO

This checklist tracks work required before a public release.

## Agent-Specific CLI Features

- [x] `--json` output for `search`, `count`, `info`, and `service-status`.
- [x] Stable JSON error format with nonzero exit codes.
- [x] `service-info` command showing loaded DBs, entry counts, build time,
  volume, and journal checkpoint.
- [ ] Config file support, for example `seekfs.toml`, covering indexed
  volumes, DB paths, service pipe, and default limits.
- [x] Machine-readable result metadata:
  - [x] `path`
  - [x] `name`
  - [x] `volume`
  - [x] `is_dir`
  - [x] `size`, when available
  - [x] `modified`, when available
  - [ ] `index_source`
- [x] Agent-safe default limits so accidental broad queries do not dump millions
  of paths.
- [x] `--absolute` / normalized path guarantees.
- [ ] `--exists` stale-result verification mode.
- [ ] Query syntax useful for agents:
  - [ ] `ext:go`
  - [ ] `dir:src`
  - [ ] `glob:*.py`
  - [ ] `regex:`
  - [ ] `case:`
  - [ ] `type:file`
  - [ ] `type:dir`
- [ ] `--cwd-bias` or `--root-bias` ranking for coding agents working in a repo.
- [ ] `--under <path>` filter to constrain search to a project/workspace.
- [ ] `--recent` / `--modified-after` filters.
- [x] Open protocol documentation for service pipe request/response JSON.
- [ ] Benchmark mode for agents: random local queries, repeated service queries,
  and JSON summary.
- [ ] Optional MCP server later, after CLI/service are stable.

## Release Requirements

- [ ] Choose final project name.
- [x] Add license: MIT or Apache-2.0.
- [x] Add disclaimer: independent project, not affiliated with Everything or
  voidtools.
- [x] Clean README:
  - [x] what it is
  - [x] who it is for
  - [x] quickstart
  - [x] service setup
  - [x] indexing setup
  - [x] examples
  - [x] limitations
- [x] Remove or move reverse-engineering notes out of the public repo, or keep
  them in a clearly separate `research/` folder.
- [x] Add `.gitignore` for binaries, DBs, sidecars, logs, benchmark outputs.
- [x] Add release build script, for example `scripts/build.ps1`.
- [x] Add Windows CI:
  - [x] `go test ./...`
  - [x] `go vet ./...`
  - [x] build binary
  - [x] run CLI integration test
- [x] Add `version` command wired to build metadata.
- [x] Add service upgrade/reinstall docs that preserve DB paths.
- [x] Add uninstall cleanup docs.
- [x] Add security notes:
  - [x] service privileges
  - [x] pipe permissions
  - [x] index file locations
- [x] Add production readiness / limitations section.
- [x] Add benchmark documentation with reproducible commands.
- [x] Add basic GitHub release artifact plan:
  - [x] zip containing `seekfs.exe`
  - [x] scripts
  - [x] README
- [x] Decide whether code signing is needed now or later.
- [x] Add issue templates for bug reports and feature requests.
- [x] Add contribution notes, even minimal.
- [x] Run a clean clone test from `F:\git\seekfs`.
- [x] Commit only source/docs/scripts, not DBs, benchmark CSVs, extracted
  binaries, or built executables.
